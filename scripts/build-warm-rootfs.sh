#!/usr/bin/env bash
# build-warm-rootfs.sh — productizes the WARM-VM template rootfs.
#
# A warm rootfs is a language base rootfs (built OCI→ext4 elsewhere) plus the
# in-guest execution agent and a persistent init that brings up networking and
# execs the agent. Booting it leaves a resident VM the host POSTs code to (no
# per-execution boot); snapshotting it yields the fast-start / CoW-clone template.
#
# This replaces the by-hand steps proven live on the Firecracker host (warm VM
# 8ms/run; snapshot restore 131ms; CoW clone 42MB/clone, netns-isolated).
#
# Usage (run as root on the Firecracker host):
#   build-warm-rootfs.sh <language> <base-rootfs.ext4> <agent-binary> [out.ext4]
# Example:
#   build-warm-rootfs.sh python /var/lib/firecracker/rootfs/python.ext4 \
#       /usr/local/bin/guest-agent /var/lib/firecracker/rootfs/python-warm.ext4
set -euo pipefail

lang="${1:?language (python|javascript)}"
base="${2:?base rootfs ext4 path}"
agent="${3:?guest-agent binary path}"
out="${4:-${base%.ext4}-warm.ext4}"

[ -f "$base" ] || { echo "base rootfs not found: $base" >&2; exit 1; }
[ -f "$agent" ] || { echo "agent binary not found: $agent" >&2; exit 1; }

echo "[build-warm-rootfs] $lang: $base -> $out"
cp -f "$base" "$out"

mnt="$(mktemp -d)"
cleanup() { umount "$mnt" 2>/dev/null || true; rmdir "$mnt" 2>/dev/null || true; }
trap cleanup EXIT
mount -o loop "$out" "$mnt"

install -D -m 0755 "$agent" "$mnt/usr/local/bin/agent"

# Persistent init: bring up loopback + eth0 (kernel `ip=` boot arg configures the
# address), set a PATH for the interpreters, then exec the resident agent. The
# agent — not a run-once script — is PID 1's payload, so the VM stays warm.
install -d -m 0755 "$mnt/sbin"
cat > "$mnt/sbin/warm-init" <<'INIT'
#!/bin/sh
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sys /sys 2>/dev/null
ip link set lo up 2>/dev/null
ip link set eth0 up 2>/dev/null
export PATH=/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin
exec /usr/local/bin/agent
INIT
chmod 0755 "$mnt/sbin/warm-init"

sync
echo "[build-warm-rootfs] done: $out"
echo "  boot with: init=/sbin/warm-init ip=<guestIP>::<hostIP>:255.255.255.252::eth0:off"
echo "  add a virtio-rng (entropy) device so restored clones reseed unique RNG."
