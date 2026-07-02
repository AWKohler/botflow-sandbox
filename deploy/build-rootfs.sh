#!/usr/bin/env bash
set -euo pipefail

# SECURITY NOTE: this builder runs apt/dpkg maintainer scripts and `npm install`
# as root inside a chroot. A chroot is not a security boundary, so this script
# must only be run on a trusted host against trusted package sources (all
# artifacts here are pinned and checksum-verified). To reduce exposure we mount a
# minimal synthetic /dev rather than bind-mounting the host's real device tree,
# so build-time code cannot reach host block devices.

DATA_DIR=${DATA_DIR:-/var/lib/sandbox-host}
MOUNT_DIR=/mnt/sandbox-host-rootfs
BASE_IMAGE="$DATA_DIR/images/base.ext4"
AGENT=${AGENT:-/usr/lib/sandbox-host/guest-agent}
NODE22=v22.23.1
NODE24=v24.18.0
NODE26=v26.4.0
PY_RELEASE=20260623
PY_ASSET='cpython-3.13.14+20260623-x86_64-unknown-linux-gnu-install_only_stripped.tar.gz'

cleanup() {
  mountpoint -q "$MOUNT_DIR/dev/pts" && umount "$MOUNT_DIR/dev/pts" || true
  mountpoint -q "$MOUNT_DIR/dev/shm" && umount "$MOUNT_DIR/dev/shm" || true
  mountpoint -q "$MOUNT_DIR/dev" && umount -l "$MOUNT_DIR/dev" || true
  mountpoint -q "$MOUNT_DIR/proc" && umount "$MOUNT_DIR/proc" || true
  mountpoint -q "$MOUNT_DIR/sys" && umount "$MOUNT_DIR/sys" || true
  mountpoint -q "$MOUNT_DIR" && umount "$MOUNT_DIR" || true
}
trap cleanup EXIT

mkdir -p "$DATA_DIR/images" "$MOUNT_DIR"
if [[ -f "$BASE_IMAGE" ]]; then
  if [[ ${RESUME:-0} != 1 ]]; then
    echo "Base image already exists; pass RESUME=1 to continue a verified partial build" >&2
    exit 1
  fi
  mount -o loop "$BASE_IMAGE" "$MOUNT_DIR"
  fresh=0
else
  truncate -s 12G "$BASE_IMAGE"
  mkfs.ext4 -q -F -L sandbox-root "$BASE_IMAGE"
  mount -o loop "$BASE_IMAGE" "$MOUNT_DIR"
  debootstrap --variant=minbase noble "$MOUNT_DIR" http://archive.ubuntu.com/ubuntu
  fresh=1
fi
# Minimal synthetic /dev: a fresh tmpfs with only the character devices apt,
# dpkg, and npm actually need — not a bind of the host's real /dev.
mount -t tmpfs -o mode=0755,nosuid tmpfs "$MOUNT_DIR/dev"
mknod -m 0666 "$MOUNT_DIR/dev/null" c 1 3
mknod -m 0666 "$MOUNT_DIR/dev/zero" c 1 5
mknod -m 0666 "$MOUNT_DIR/dev/full" c 1 7
mknod -m 0666 "$MOUNT_DIR/dev/random" c 1 8
mknod -m 0666 "$MOUNT_DIR/dev/urandom" c 1 9
mknod -m 0666 "$MOUNT_DIR/dev/tty" c 5 0
mkdir -p "$MOUNT_DIR/dev/pts" "$MOUNT_DIR/dev/shm"
mount -t devpts -o nosuid,noexec,newinstance,ptmxmode=0666 devpts "$MOUNT_DIR/dev/pts"
ln -sf /dev/pts/ptmx "$MOUNT_DIR/dev/ptmx"
mount -t tmpfs -o nosuid,nodev tmpfs "$MOUNT_DIR/dev/shm"
mount -t proc proc "$MOUNT_DIR/proc"
mount -t sysfs -o ro sysfs "$MOUNT_DIR/sys"

install -m 0755 "$AGENT" "$MOUNT_DIR/usr/local/bin/sandbox-agent"
if [[ $fresh == 1 ]]; then
  mkdir -p "$MOUNT_DIR/vercel/sandbox" "$MOUNT_DIR/vercel/runtimes" "$MOUNT_DIR/home/vercel-sandbox"
  chroot "$MOUNT_DIR" groupadd -g 1000 vercel-sandbox
  chroot "$MOUNT_DIR" useradd -u 1000 -g 1000 -d /home/vercel-sandbox -s /bin/bash vercel-sandbox
  chroot "$MOUNT_DIR" chown -R 1000:1000 /home/vercel-sandbox /vercel/sandbox

  printf '#!/bin/sh\nexit 101\n' > "$MOUNT_DIR/usr/sbin/policy-rc.d"
  chmod 0755 "$MOUNT_DIR/usr/sbin/policy-rc.d"
  cp --remove-destination /etc/resolv.conf "$MOUNT_DIR/etc/resolv.conf"
  chroot "$MOUNT_DIR" env DEBIAN_FRONTEND=noninteractive apt-get update
  chroot "$MOUNT_DIR" env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    bash ca-certificates curl git gzip iproute2 openssl procps tar unzip xz-utils zstd \
    bind9-dnsutils bzip2 findutils iputils-ping libatomic1 ncurses-bin sudo whois
  chroot "$MOUNT_DIR" apt-get clean
  rm -rf "$MOUNT_DIR/var/lib/apt/lists"/* "$MOUNT_DIR/usr/sbin/policy-rc.d"
fi

if ! chroot "$MOUNT_DIR" dpkg-query -W -f='${Status}' libatomic1 2>/dev/null | grep -q 'install ok installed'; then
  cp --remove-destination /etc/resolv.conf "$MOUNT_DIR/etc/resolv.conf"
  chroot "$MOUNT_DIR" env DEBIAN_FRONTEND=noninteractive apt-get update
  chroot "$MOUNT_DIR" env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libatomic1
  chroot "$MOUNT_DIR" apt-get clean
  rm -rf "$MOUNT_DIR/var/lib/apt/lists"/*
fi

install_node() {
  local version=$1
  local runtime=$2
  local work=/tmp/node-${runtime}
  rm -rf "$work" && mkdir -p "$work"
  curl -fsSLo "$work/SHASUMS256.txt" "https://nodejs.org/dist/${version}/SHASUMS256.txt"
  curl -fsSLo "$work/node.tar.xz" "https://nodejs.org/dist/${version}/node-${version}-linux-x64.tar.xz"
  (cd "$work" && grep " node-${version}-linux-x64.tar.xz\$" SHASUMS256.txt | sed "s#node-${version}-linux-x64.tar.xz#node.tar.xz#" | sha256sum -c -)
  rm -rf "$MOUNT_DIR/vercel/runtimes/${runtime}"
  mkdir -p "$MOUNT_DIR/vercel/runtimes/${runtime}"
  tar -xJf "$work/node.tar.xz" --strip-components=1 -C "$MOUNT_DIR/vercel/runtimes/${runtime}"
  chroot "$MOUNT_DIR" env "PATH=/vercel/runtimes/${runtime}/bin:/usr/bin:/bin" \
    "/vercel/runtimes/${runtime}/bin/npm" install -g pnpm@10
  rm -rf "$work"
}
install_node "$NODE22" node22
install_node "$NODE24" node24
install_node "$NODE26" node26

py_url="https://github.com/astral-sh/python-build-standalone/releases/download/${PY_RELEASE}/${PY_ASSET/+/%2B}"
curl -fsSLo /tmp/python-SHA256SUMS "https://github.com/astral-sh/python-build-standalone/releases/download/${PY_RELEASE}/SHA256SUMS"
curl -fL --retry 3 -o "/tmp/$PY_ASSET" "$py_url"
(cd /tmp && grep "  $PY_ASSET\$" python-SHA256SUMS | sha256sum -c -)
rm -rf "$MOUNT_DIR/vercel/runtimes/python"
mkdir -p "$MOUNT_DIR/vercel/runtimes/python"
tar -xzf "/tmp/$PY_ASSET" --strip-components=1 -C "$MOUNT_DIR/vercel/runtimes/python"
rm -f "/tmp/$PY_ASSET" /tmp/python-SHA256SUMS

ln -sfn /vercel/runtimes/node24/bin/node "$MOUNT_DIR/usr/local/bin/node"
ln -sfn /vercel/runtimes/node24/bin/npm "$MOUNT_DIR/usr/local/bin/npm"
ln -sfn /vercel/runtimes/node24/bin/npx "$MOUNT_DIR/usr/local/bin/npx"
ln -sfn /vercel/runtimes/node24/bin/pnpm "$MOUNT_DIR/usr/local/bin/pnpm"
ln -sfn /vercel/runtimes/python/bin/python3 "$MOUNT_DIR/usr/local/bin/python3.13"

# pnpm 10 refuses dependency build scripts unless approved; pre-approve only
# Claude Code so `pnpm add @anthropic-ai/claude-code` links its native binary.
printf 'only-built-dependencies[]=@anthropic-ai/claude-code\n' > "$MOUNT_DIR/home/vercel-sandbox/.npmrc"
chroot "$MOUNT_DIR" chown 1000:1000 /home/vercel-sandbox/.npmrc
chmod 0644 "$MOUNT_DIR/home/vercel-sandbox/.npmrc"

printf 'sandbox-host\n' > "$MOUNT_DIR/etc/hostname"
printf '127.0.0.1 localhost\n127.0.1.1 sandbox-host\n' > "$MOUNT_DIR/etc/hosts"
sync
umount "$MOUNT_DIR/dev/pts"
umount "$MOUNT_DIR/dev/shm"
umount "$MOUNT_DIR/dev"
umount "$MOUNT_DIR/proc"
umount "$MOUNT_DIR/sys"
umount "$MOUNT_DIR"

for runtime in node22 node24 node26 python3.13; do
  rm -f "$DATA_DIR/images/${runtime}.ext4"
  cp --reflink=always --sparse=auto "$BASE_IMAGE" "$DATA_DIR/images/${runtime}.ext4"
done
chmod 0600 "$DATA_DIR/images"/*.ext4
echo "Guest runtime images built successfully."
