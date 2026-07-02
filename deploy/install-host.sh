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

# Dedicated per-service users and PRIMARY groups. Separate groups keep the
# egress<->runtime and API<->runtime control paths isolated: only sandbox-api
# can reach the runtime socket, and only root/sandbox-egress the egress socket.
getent group sandbox-api >/dev/null || groupadd --system sandbox-api
getent group sandbox-egress >/dev/null || groupadd --system sandbox-egress
if id sandbox-api >/dev/null 2>&1; then usermod --gid sandbox-api sandbox-api; else useradd --system --gid sandbox-api --home-dir /nonexistent --shell /usr/sbin/nologin sandbox-api; fi
if id sandbox-egress >/dev/null 2>&1; then usermod --gid sandbox-egress sandbox-egress; else useradd --system --gid sandbox-egress --home-dir /nonexistent --shell /usr/sbin/nologin sandbox-egress; fi
api_gid=$(getent group sandbox-api | cut -d: -f3)

install -d -m 0755 /usr/lib/sandbox-host /etc/sandbox-host /srv/jailer
# /run/sandbox-host holds only the runtime socket, created by root sandboxd.
# World-traversable so the API can connect to the socket (whose group is
# sandbox-api); no service other than root may create files here.
install -d -o root -g root -m 0755 /run/sandbox-host
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
# Data root is root-owned and NOT writable by any service user, so a compromised
# API/egress process cannot rename or replace the runtime-state, image, or
# snapshot directories that root sandboxd trusts. The API gets its own private
# subdirectory for the database only.
install -d -o root -g root -m 0755 "$DATA_DIR"
install -d -o root -g root -m 0750 "$DATA_DIR/images" "$DATA_DIR/instances" "$DATA_DIR/snapshots" "$DATA_DIR/runtime" "$DATA_DIR/jailer"
install -d -o sandbox-api -g sandbox-api -m 0700 "$DATA_DIR/api"
# Migrate a database created by an earlier layout into the API-private dir.
if [[ -f "$DATA_DIR/api.db" && ! -f "$DATA_DIR/api/api.db" ]]; then
  mv "$DATA_DIR/api.db" "$DATA_DIR/api/api.db"
  chown sandbox-api:sandbox-api "$DATA_DIR/api/api.db"
fi

install -m 0644 "$STAGE/egress.json" /etc/sandbox-host/egress.json
install -m 0644 "$STAGE/nftables.conf" /etc/sandbox-host/nftables.conf

cat > /etc/sandbox-host/runtime.json <<EOF
{
  "dataDir": "$DATA_DIR",
  "jailDir": "$DATA_DIR/jailer",
  "firecrackerBinary": "/usr/bin/firecracker",
  "jailerBinary": "/usr/bin/jailer",
  "kernelPath": "/usr/lib/sandbox-host/vmlinux",
  "egressSocket": "/run/sandbox-egress/egress.sock",
  "listenSocket": "/run/sandbox-host/sandboxd.sock",
  "region": "local-nyc",
  "hostReserveMiB": 4096,
  "storageReserveMiB": 20480,
  "maxVcpus": 8,
  "cpuOvercommit": 1.0,
  "socketGid": $api_gid,
  "allowedRuntimes": ["node22", "node24", "node26", "python3.13"]
}
EOF

# Preserve an existing admin token across re-runs; only mint one on first install.
if [[ -f /etc/sandbox-host/api.json ]]; then
  token_hash=$(jq -r '.tokens[0].sha256' /etc/sandbox-host/api.json)
  token=""
else
  token=$(openssl rand -base64 36 | tr -d '\n')
  token_hash=$(printf '%s' "$token" | sha256sum | cut -d' ' -f1)
fi
cat > /etc/sandbox-host/api.json <<EOF
{
  "listen": "127.0.0.1:8080",
  "previewBindHost": "$TAILSCALE_IP",
  "previewExternalHost": "$TAILSCALE_HOST",
  "previewPortStart": 20000,
  "previewPortEnd": 40000,
  "databasePath": "$DATA_DIR/api/api.db",
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
chown root:sandbox-api /etc/sandbox-host/api.json
chown root:sandbox-egress /etc/sandbox-host/egress.json
chown root:root /etc/sandbox-host/runtime.json /etc/sandbox-host/nftables.conf

install -m 0755 "$STAGE/build-rootfs.sh" /usr/lib/sandbox-host/build-rootfs.sh
install -m 0755 "$STAGE/scope-previews.sh" /usr/lib/sandbox-host/scope-previews.sh
install -m 0644 "$STAGE/systemd"/*.service "$STAGE/systemd"/*.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable sandbox-host-firewall.service sandbox-host-egress.service sandbox-host-runtime.service sandbox-host-api.service sandbox-host-gc.timer

# Only (re)write the operator credentials file when a fresh token was minted.
if [[ -n "$token" ]]; then
  cat > /home/ai-club-pc/sandbox-host-credentials <<EOF
SANDBOX_API_URL=https://$TAILSCALE_HOST/api
SANDBOX_TOKEN=$token
SANDBOX_TEAM_ID=default
SANDBOX_PROJECT_ID=default
EOF
  chown ai-club-pc:ai-club-pc /home/ai-club-pc/sandbox-host-credentials
  chmod 0600 /home/ai-club-pc/sandbox-host-credentials
fi

echo "Host prerequisites installed. Build the guest image, then start services."
