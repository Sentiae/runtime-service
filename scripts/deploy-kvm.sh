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

if [[ "${1:-}" != "--no-restart" ]]; then
  echo "==> restarting runtime-service.service"
  "${SSH[@]}" 'sudo systemctl restart runtime-service && sleep 2 && sudo systemctl is-active runtime-service'
fi
echo "==> done"
