#!/usr/bin/env bash
#
# Host-local preview-port scoping for Sandbox Host.
#
# Restricts inbound connections to the preview port range (default 20000-40000)
# on the Tailscale interface so only an explicit allowlist of source tailnet IPs
# can reach any sandbox's preview. Enforced in the existing `sandbox_host`
# nftables table and persisted by editing the loaded ruleset file, so it
# survives firewall reloads and reboots. SSH (22) and the API (443) are never
# touched.
#
# Usage (run as root):
#   scope-previews.sh enable 100.84.228.113 [100.69.123.122 ...]
#   scope-previews.sh disable
#   scope-previews.sh status
#
# NOTE: with no allowlist configured (the default), previews are reachable by
# any tailnet peer — that is the intended "not yet scoped" state. Enabling with
# an EMPTY list would drop all preview traffic, so `enable` requires >=1 IP.

set -euo pipefail

CONF=${SANDBOX_NFT_CONF:-/etc/sandbox-host/nftables.conf}
IFACE=${PREVIEW_IFACE:-tailscale0}
LO=${PREVIEW_PORT_START:-20000}
HI=${PREVIEW_PORT_END:-40000}
BEGIN='# >>> sandbox-host preview-scope (managed by scope-previews.sh) >>>'
END='# <<< sandbox-host preview-scope <<<'

if [[ $EUID -ne 0 ]]; then
  echo "scope-previews.sh must run as root" >&2
  exit 1
fi

strip_block() {
  # Remove any existing managed block from the ruleset file (in place).
  if grep -qF "$BEGIN" "$CONF"; then
    sed -i "/$(printf '%s' "$BEGIN" | sed 's/[.[\*^$/]/\\&/g')/,/$(printf '%s' "$END" | sed 's/[.[\*^$/]/\\&/g')/d" "$CONF"
    # Drop a trailing blank line left behind, if any.
    sed -i -e :a -e '/^\n*$/{$d;N;ba}' "$CONF"
  fi
}

reload() {
  # Prefer the unit so state stays consistent with systemd; fall back to nft.
  if systemctl is-enabled sandbox-host-firewall >/dev/null 2>&1; then
    systemctl reload sandbox-host-firewall
  else
    nft -f "$CONF"
  fi
}

case "${1:-}" in
  enable)
    shift
    [[ $# -ge 1 ]] || { echo "enable requires at least one source IP" >&2; exit 1; }
    elements=$(printf '%s, ' "$@"); elements=${elements%, }
    strip_block
    {
      echo ""
      echo "$BEGIN"
      echo "add set inet sandbox_host preview_allow { type ipv4_addr; }"
      echo "flush set inet sandbox_host preview_allow"
      echo "add element inet sandbox_host preview_allow { $elements }"
      echo "add rule inet sandbox_host input iifname \"$IFACE\" tcp dport $LO-$HI ip saddr != @preview_allow drop"
      echo "$END"
    } >> "$CONF"
    reload
    echo "Preview ports $LO-$HI on $IFACE now restricted to: $*"
    ;;
  disable)
    strip_block
    reload
    echo "Preview-port scoping removed; previews reachable by any tailnet peer."
    ;;
  status)
    if grep -qF "$BEGIN" "$CONF"; then
      echo "scoped (config):"; sed -n "/$(printf '%s' "$BEGIN" | sed 's/[.[\*^$/]/\\&/g')/,/$(printf '%s' "$END" | sed 's/[.[\*^$/]/\\&/g')/p" "$CONF"
    else
      echo "not scoped (previews reachable by any tailnet peer)"
    fi
    echo "--- live set ---"
    nft list set inet sandbox_host preview_allow 2>/dev/null || echo "(no preview_allow set in live ruleset)"
    ;;
  *)
    echo "usage: scope-previews.sh {enable <ip> [ip...]|disable|status}" >&2
    exit 1
    ;;
esac
