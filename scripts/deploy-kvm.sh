#!/usr/bin/env bash
# Deploys runtime-service to the KVM host (VM 121, 10.0.10.244) where the
# Firecracker substrate runs as a systemd service (NOT docker — /dev/kvm host).
# Builds linux/amd64 binaries locally, ships them, restarts the unit.
#
# The compose runtime-service on 10.0.10.20 is deployed separately by the root
# scripts/deploy.sh; this script owns ONLY the KVM host binary.
#
# Usage: scripts/deploy-kvm.sh            # build + deploy + restart
#        scripts/deploy-kvm.sh --no-restart
#
# Required keys in /etc/runtime-service.env on the KVM host (values from the
# stack's .env.secrets; this file is host-provisioned, never committed):
#   APP_EXECUTOR_TYPE=firecracker  APP_FC_* (binary/kernel/rootfs paths)
#   APP_DATABASE_* (points at the 10.0.10.20 postgres, db runtime_service_fc)
#   APP_GRPC_SERVICE_API_KEY=<shared service key>
#   APP_REGISTRY_HOST=10.0.10.20:8078
#   APP_REGISTRY_SERVICE_KEY=<same shared service key — the OCI registry accepts it>
#   APP_IMAGEBOOT_ADVERTISE_HOST=10.0.10.244   (defaults exist for the other APP_IMAGEBOOT_*)
#
# Secret resolution (P3.4 — required only for resident deploys with secret_refs;
# unset ⇒ secret-less deploys still boot, secret-bearing ones fail closed):
#   VAULT_ADDR=https://10.0.10.20:8200
#   VAULT_AUTH_MODE=svid          # authenticate as svc/runtime via the KVM SPIRE agent's JWT-SVID
#   VAULT_SVID_ROLE=runtime       # Vault jwt-backend role → svc-runtime-resolver policy (pre-provisioned)
#   (needs mTLS/SPIRE on: APP_GRPC_MTLS_MODE!=off, so the SPIFFE socket exists)
set -euo pipefail

HOST="${KVM_HOST:-ubuntu@10.0.10.244}"
KEY="${KVM_SSH_KEY:-$HOME/.ssh/sentiae_homelab}"
SSH=(ssh -i "$KEY" -o ConnectTimeout=10 "$HOST")
SCP=(scp -i "$KEY")

cd "$(dirname "$0")/.."

echo "==> building linux/amd64 binaries"
mkdir -p .build
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o .build/runtime-service ./cmd/server
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o .build/image-init ./cmd/image-init
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o .build/guest-agent ./cmd/guest-agent

echo "==> shipping to $HOST"
"${SCP[@]}" .build/runtime-service .build/image-init .build/guest-agent "$HOST":/tmp/
"${SSH[@]}" 'sudo install -m 0755 /tmp/runtime-service /usr/local/bin/runtime-service.new \
  && sudo install -m 0755 /tmp/image-init /usr/local/bin/image-init \
  && sudo install -m 0755 /tmp/guest-agent /usr/local/bin/guest-agent \
  && sudo mv /usr/local/bin/runtime-service.new /usr/local/bin/runtime-service \
  && rm -f /tmp/runtime-service /tmp/image-init /tmp/guest-agent'

# --- SPIRE agent provisioning (mTLS mesh) -----------------------------------
# Reproducible bring-up of the second SPIRE agent on the KVM host so the
# runtime-service binary here gets a spiffe://sentiae.io/svc/runtime SVID and
# joins the mesh. Whole block is a no-op unless mTLS is on — no runtime-service
# systemd unit is committed in-repo, so the spire-agent -> runtime-service
# ordering (spire-agent Before=runtime-service; add Wants=/After=spire-agent to
# the host-provisioned runtime-service.service) is provisioned on the host.
if [[ "${APP_GRPC_MTLS_MODE:-off}" != off ]]; then
  echo "==> provisioning SPIRE agent on $HOST (mTLS mode: $APP_GRPC_MTLS_MODE)"
  REPO_ROOT="$(cd .. && pwd)"
  SPIRE_HOST="${SPIRE_SERVER_HOST:-ubuntu@10.0.10.20}"
  SPIRE_SSH=(ssh -i "$KEY" -o ConnectTimeout=10 "$SPIRE_HOST")
  SPIRE_EXEC='docker exec sentiae-spire-server /opt/spire/bin/spire-server'
  SPIRE_SOCK='/tmp/spire-server/private/api.sock'
  KVM_AGENT_ID="spiffe://sentiae.io/agent/kvm"

  echo "==> ensuring KVM runtime-service registration entry"
  "${SPIRE_SSH[@]}" "$SPIRE_EXEC entry show -socketPath $SPIRE_SOCK -parentID $KVM_AGENT_ID -spiffeID spiffe://sentiae.io/svc/runtime 2>/dev/null | grep -q svc/runtime \
    || $SPIRE_EXEC entry create -socketPath $SPIRE_SOCK -parentID $KVM_AGENT_ID \
         -spiffeID spiffe://sentiae.io/svc/runtime \
         -selector unix:path:/usr/local/bin/runtime-service >/dev/null 2>&1 \
    || echo '[deploy-kvm] warn: kvm svc/runtime create non-zero (exists?)'"

  echo "==> minting one-time join token"
  JOIN_TOKEN="$("${SPIRE_SSH[@]}" "$SPIRE_EXEC token generate -socketPath $SPIRE_SOCK -spiffeID $KVM_AGENT_ID" | awk '/^Token:/{print $2}')"
  if [[ -z "$JOIN_TOKEN" ]]; then
    echo "==> ERROR: failed to mint join token" >&2
    exit 1
  fi

  echo "==> fetching trust bundle"
  "${SPIRE_SSH[@]}" "$SPIRE_EXEC bundle show -socketPath $SPIRE_SOCK" > .build/bootstrap.crt
  if [[ ! -s .build/bootstrap.crt ]]; then
    echo "==> ERROR: empty trust bundle" >&2
    exit 1
  fi

  echo "==> shipping agent config to $HOST"
  "${SCP[@]}" \
    "$REPO_ROOT/infrastructure/spire/kvm/agent.conf" \
    "$REPO_ROOT/infrastructure/spire/kvm/spire-agent.service" \
    .build/bootstrap.crt \
    "$HOST":/tmp/

  # Install the pinned spire-agent by downloading the official static (musl)
  # release ON the KVM host — no local Docker daemon required. Idempotent: skip
  # if the pinned version is already installed.
  echo "==> installing spire-agent 1.11.2 + config on $HOST"
  "${SSH[@]}" 'set -e
    if ! /usr/local/bin/spire-agent --version 2>&1 | grep -q 1.11.2; then
      curl -fsSL https://github.com/spiffe/spire/releases/download/v1.11.2/spire-1.11.2-linux-amd64-musl.tar.gz -o /tmp/spire-agent.tgz
      sudo tar -xzf /tmp/spire-agent.tgz -C /usr/local/bin --strip-components=2 spire-1.11.2/bin/spire-agent
      sudo chmod 0755 /usr/local/bin/spire-agent
      rm -f /tmp/spire-agent.tgz
    fi
    sudo install -d -m 0755 /etc/spire /var/lib/spire/agent
    sudo install -m 0644 /tmp/agent.conf /etc/spire/agent.conf
    sudo install -m 0644 /tmp/bootstrap.crt /etc/spire/bootstrap.crt
    sudo install -m 0644 /tmp/spire-agent.service /etc/systemd/system/spire-agent.service
    rm -f /tmp/agent.conf /tmp/bootstrap.crt /tmp/spire-agent.service'

  echo "==> writing join token (tmpfs, 0600)"
  "${SSH[@]}" "sudo install -d -m 0755 /run/spire/agent \
    && printf '%s' '$JOIN_TOKEN' | sudo tee /run/spire/agent/join_token >/dev/null \
    && sudo chmod 0600 /run/spire/agent/join_token"

  echo "==> enabling spire-agent.service"
  "${SSH[@]}" 'sudo systemctl daemon-reload && sudo systemctl enable --now spire-agent'

  # Upsert the mesh keys authoritatively (not add-if-absent) so the KVM runtime's
  # mode matches this deploy's APP_GRPC_MTLS_MODE on every run — no host drift.
  echo "==> setting runtime-service.env mesh keys (mode=$APP_GRPC_MTLS_MODE)"
  "${SSH[@]}" 'grep -q "^SPIFFE_ENDPOINT_SOCKET=" /etc/runtime-service.env \
    || echo "SPIFFE_ENDPOINT_SOCKET=unix:///run/spire/agent-sockets/api.sock" | sudo tee -a /etc/runtime-service.env >/dev/null'
  "${SSH[@]}" "sudo sed -i '/^APP_GRPC_MTLS_MODE=/d' /etc/runtime-service.env \
    && echo 'APP_GRPC_MTLS_MODE=${APP_GRPC_MTLS_MODE}' | sudo tee -a /etc/runtime-service.env >/dev/null"
fi
# --- end SPIRE agent provisioning -------------------------------------------

# --- Vault per-tenant secret resolver (P3.4/D-085, rt#10) -------------------
# The fleet host authenticates to Vault as svc/runtime (via its SPIRE JWT-SVID)
# to resolve + decrypt per-tenant secrets for resident deploys. Set these
# UNCONDITIONALLY (not gated on the mTLS if-block above) so a plain re-run keeps
# the resolver wired — the SPIRE agent is provisioned durably on the host, so
# svid auth works whether or not this run re-provisioned it. secret-less deploys
# ignore these; secret-bearing ones fail closed if Vault is unreachable.
echo "==> setting runtime-service.env Vault resolver keys (unconditional)"
for _kv in "VAULT_ADDR=https://10.0.10.20:8200" "VAULT_AUTH_MODE=svid" "VAULT_SVID_ROLE=runtime"; do
  _k="${_kv%%=*}"
  "${SSH[@]}" "sudo sed -i '/^${_k}=/d' /etc/runtime-service.env && echo '${_kv}' | sudo tee -a /etc/runtime-service.env >/dev/null"
done
# --- end Vault per-tenant secret resolver -----------------------------------

# --- Structured app logging (fleet-outage RCA enabler) ----------------------
# The fleet warm-pool/provision paths log via platform-kit's slog logger;
# cmd/server/main.go wires that as the slog default from APP_LOGGING_* so those
# Info/Warn/Error lines reach journald (previously only GORM SQL was visible on
# this host, hiding fleet provisioner failures). Stamp the keys UNCONDITIONALLY
# + idempotently so a clean fleet-host deploy reproduces app logs with zero
# manual env edits. No OTLP endpoint: the fleet host has no reachable collector,
# so JSON-to-stdout (captured by journald) is the durable sink.
echo "==> setting runtime-service.env structured-logging keys (unconditional)"
for _kv in "APP_LOGGING_LEVEL=info" "APP_LOGGING_FORMAT=json" "APP_LOGGING_OUTPUT=stdout"; do
  _k="${_kv%%=*}"
  "${SSH[@]}" "sudo sed -i '/^${_k}=/d' /etc/runtime-service.env && echo '${_kv}' | sudo tee -a /etc/runtime-service.env >/dev/null"
done
# --- end structured app logging ---------------------------------------------

# --- D-061 Phase B: enforce the verified-org boundary on FleetOrchestration --
# Provision authorizes owner_org + cross-checks the attested x-organization-id.
# Shadow-verified flip-safe; upsert so a fresh fleet host reproduces enforce.
echo "==> setting runtime-service.env APP_AUTH_ORG_ENFORCE=true (D-061)"
"${SSH[@]}" "sudo sed -i '/^APP_AUTH_ORG_ENFORCE=/d' /etc/runtime-service.env \
  && echo 'APP_AUTH_ORG_ENFORCE=true' | sudo tee -a /etc/runtime-service.env >/dev/null"

if [[ "${1:-}" != "--no-restart" ]]; then
  echo "==> restarting runtime-service.service"
  "${SSH[@]}" 'sudo systemctl restart runtime-service && sleep 2 && sudo systemctl is-active runtime-service'
fi
echo "==> done"
