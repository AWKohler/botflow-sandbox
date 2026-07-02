#!/usr/bin/env bash
set -euo pipefail

STAGE=${STAGE:-/tmp/sandbox-host-release}
TAILSCALE_HOST=${TAILSCALE_HOST:-ai-club-pc-ms-7c56.taila01548.ts.net}
TAILSCALE_IP=${TAILSCALE_IP:-100.93.37.55}
STORE_FILE=/var/lib/sandbox-host.btrfs
DATA_DIR=/var/lib/sandbox-host

if [[ $EUID -ne 0 ]]; then
  echo "install-host.sh must run as root" >&2
  exit 1
fi
for file in sandbox-api sandboxd egressd guest-agent firecracker-v1.15.1-x86_64.tgz vmlinux-6.1.155; do
  [[ -f "$STAGE/$file" ]] || { echo "missing $STAGE/$file" >&2; exit 1; }
done
echo 'd4a32ab2322d887ca1bc4a4e7afa9cc35393e6362dfc2b3becb389d362e4275a  '"$STAGE/firecracker-v1.15.1-x86_64.tgz" | sha256sum -c -
echo 'e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2  '"$STAGE/vmlinux-6.1.155" | sha256sum -c -

apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  btrfs-progs ca-certificates curl debootstrap e2fsprogs iproute2 jq nftables openssl xz-utils

getent group sandbox-host >/dev/null || groupadd --system sandbox-host
id sandbox-api >/dev/null 2>&1 || useradd --system --gid sandbox-host --home-dir /nonexistent --shell /usr/sbin/nologin sandbox-api
id sandbox-egress >/dev/null 2>&1 || useradd --system --gid sandbox-host --home-dir /nonexistent --shell /usr/sbin/nologin sandbox-egress
group_gid=$(getent group sandbox-host | cut -d: -f3)

install -d -m 0755 /usr/lib/sandbox-host /etc/sandbox-host /srv/jailer
install -d -o root -g sandbox-host -m 0770 /run/sandbox-host
install -m 0755 "$STAGE/sandbox-api" /usr/lib/sandbox-host/sandbox-api
install -m 0755 "$STAGE/sandboxd" /usr/lib/sandbox-host/sandboxd
install -m 0755 "$STAGE/egressd" /usr/lib/sandbox-host/egressd
install -m 0755 "$STAGE/guest-agent" /usr/lib/sandbox-host/guest-agent
install -m 0644 "$STAGE/vmlinux-6.1.155" /usr/lib/sandbox-host/vmlinux

tmp=$(mktemp -d)
tar -xzf "$STAGE/firecracker-v1.15.1-x86_64.tgz" -C "$tmp"
release_dir="$tmp/release-v1.15.1-x86_64"
(cd "$release_dir" && sha256sum -c SHA256SUMS --ignore-missing)
install -m 0755 "$release_dir/firecracker-v1.15.1-x86_64" /usr/bin/firecracker
install -m 0755 "$release_dir/jailer-v1.15.1-x86_64" /usr/bin/jailer
rm -rf "$tmp"

if ! findmnt -rn "$DATA_DIR" >/dev/null; then
  if [[ ! -f "$STORE_FILE" ]]; then
    truncate -s 500G "$STORE_FILE"
    mkfs.btrfs -q -L sandbox-host "$STORE_FILE"
  fi
  mkdir -p "$DATA_DIR"
  store_uuid=$(blkid -s UUID -o value "$STORE_FILE")
  grep -q "UUID=$store_uuid" /etc/fstab || echo "UUID=$store_uuid $DATA_DIR btrfs loop,noatime,compress=zstd:1,space_cache=v2,nofail 0 0" >> /etc/fstab
  mount "$DATA_DIR"
fi
install -d -o sandbox-api -g sandbox-host -m 0770 "$DATA_DIR"
install -d -o root -g sandbox-host -m 0750 "$DATA_DIR/images" "$DATA_DIR/instances" "$DATA_DIR/snapshots" "$DATA_DIR/runtime" "$DATA_DIR/jailer"

install -m 0644 "$STAGE/egress.json" /etc/sandbox-host/egress.json
install -m 0644 "$STAGE/nftables.conf" /etc/sandbox-host/nftables.conf
token=$(openssl rand -base64 36 | tr -d '\n')
token_hash=$(printf '%s' "$token" | sha256sum | cut -d' ' -f1)

cat > /etc/sandbox-host/runtime.json <<EOF
{
  "dataDir": "$DATA_DIR",
  "jailDir": "$DATA_DIR/jailer",
  "firecrackerBinary": "/usr/bin/firecracker",
  "jailerBinary": "/usr/bin/jailer",
  "kernelPath": "/usr/lib/sandbox-host/vmlinux",
  "egressSocket": "/run/sandbox-host/egress.sock",
  "listenSocket": "/run/sandbox-host/sandboxd.sock",
  "region": "local-nyc",
  "hostReserveMiB": 4096,
  "maxVcpus": 8,
  "cpuOvercommit": 1.0,
  "socketGid": $group_gid,
  "allowedRuntimes": ["node22", "node24", "node26", "python3.13"]
}
EOF
cat > /etc/sandbox-host/api.json <<EOF
{
  "listen": "127.0.0.1:8080",
  "previewBindHost": "$TAILSCALE_IP",
  "previewExternalHost": "$TAILSCALE_HOST",
  "previewPortStart": 20000,
  "previewPortEnd": 40000,
  "databasePath": "$DATA_DIR/api.db",
  "runtimeSocket": "/run/sandbox-host/sandboxd.sock",
  "region": "local-nyc",
  "defaultTimeoutMs": 300000,
  "maxTimeoutMs": 2700000,
  "maxPorts": 15,
  "egressCeiling": $(jq '.ceiling' /etc/sandbox-host/egress.json),
  "tokens": [{"id":"initial-admin","sha256":"$token_hash","teamId":"default","projectIds":["default"],"maxSessions":10}]
}
EOF
chmod 0640 /etc/sandbox-host/*.json
chown root:sandbox-host /etc/sandbox-host/*.json

install -m 0755 "$STAGE/build-rootfs.sh" /usr/lib/sandbox-host/build-rootfs.sh
install -m 0644 "$STAGE/systemd"/*.service "$STAGE/systemd"/*.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable sandbox-host-firewall.service sandbox-host-egress.service sandbox-host-runtime.service sandbox-host-api.service sandbox-host-gc.timer

cat > /home/ai-club-pc/sandbox-host-credentials <<EOF
SANDBOX_API_URL=https://$TAILSCALE_HOST/api
SANDBOX_TOKEN=$token
SANDBOX_TEAM_ID=default
SANDBOX_PROJECT_ID=default
EOF
chown ai-club-pc:ai-club-pc /home/ai-club-pc/sandbox-host-credentials
chmod 0600 /home/ai-club-pc/sandbox-host-credentials

echo "Host prerequisites installed. Build the guest image, then start services."
