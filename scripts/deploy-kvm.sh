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
#   APP_REGISTRY_HOST=10.0.10.20:8089
#   APP_REGISTRY_SERVICE_KEY=<same shared service key — the OCI registry accepts it>
#   APP_IMAGEBOOT_ADVERTISE_HOST=10.0.10.244   (defaults exist for the other APP_IMAGEBOOT_*)
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

  echo "==> extracting pinned spire-agent binary"
  EXTRACT_CTR="sentiae-spire-agent-extract-$$"
  docker create --name "$EXTRACT_CTR" ghcr.io/spiffe/spire-agent:1.11.2 >/dev/null
  docker cp "$EXTRACT_CTR":/opt/spire/bin/spire-agent .build/spire-agent
  docker rm "$EXTRACT_CTR" >/dev/null

  echo "==> shipping agent artifacts to $HOST"
  "${SCP[@]}" .build/spire-agent \
    "$REPO_ROOT/infrastructure/spire/kvm/agent.conf" \
    "$REPO_ROOT/infrastructure/spire/kvm/spire-agent.service" \
    .build/bootstrap.crt \
    "$HOST":/tmp/

  echo "==> installing spire-agent on $HOST"
  "${SSH[@]}" 'sudo install -m 0755 /tmp/spire-agent /usr/local/bin/spire-agent \
    && sudo install -d -m 0755 /etc/spire /var/lib/spire/agent \
    && sudo install -m 0644 /tmp/agent.conf /etc/spire/agent.conf \
    && sudo install -m 0644 /tmp/bootstrap.crt /etc/spire/bootstrap.crt \
    && sudo install -m 0644 /tmp/spire-agent.service /etc/systemd/system/spire-agent.service \
    && rm -f /tmp/spire-agent /tmp/agent.conf /tmp/bootstrap.crt /tmp/spire-agent.service'

  echo "==> writing join token (tmpfs, 0600)"
  "${SSH[@]}" "sudo install -d -m 0755 /run/spire/agent \
    && printf '%s' '$JOIN_TOKEN' | sudo tee /run/spire/agent/join_token >/dev/null \
    && sudo chmod 0600 /run/spire/agent/join_token"

  echo "==> enabling spire-agent.service"
  "${SSH[@]}" 'sudo systemctl daemon-reload && sudo systemctl enable --now spire-agent'

  echo "==> ensuring runtime-service.env mesh keys"
  "${SSH[@]}" 'grep -q "^SPIFFE_ENDPOINT_SOCKET=" /etc/runtime-service.env \
    || echo "SPIFFE_ENDPOINT_SOCKET=unix:///run/spire/agent-sockets/api.sock" | sudo tee -a /etc/runtime-service.env >/dev/null'
  "${SSH[@]}" 'grep -q "^APP_GRPC_MTLS_MODE=" /etc/runtime-service.env \
    || echo "APP_GRPC_MTLS_MODE=permissive" | sudo tee -a /etc/runtime-service.env >/dev/null'
fi
# --- end SPIRE agent provisioning -------------------------------------------

if [[ "${1:-}" != "--no-restart" ]]; then
  echo "==> restarting runtime-service.service"
  "${SSH[@]}" 'sudo systemctl restart runtime-service && sleep 2 && sudo systemctl is-active runtime-service'
fi
echo "==> done"
