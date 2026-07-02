# Operations specification

## Host layout

- `/usr/lib/sandbox-host/` — versioned, root-owned binaries and guest kernel
- `/etc/sandbox-host/` — root-owned configuration; secrets mode `0600`
- `/var/lib/sandbox-host/` — dedicated Btrfs loopback mount for images, instances, snapshots, logs, and SQLite backup
- `/run/sandbox-host/` — runtime sockets and namespace metadata
- `/var/log/sandbox-host/` — structured service/audit logs with rotation

## Network exposure

The initial API and preview router bind only to loopback and/or the Tailscale address. Tailscale HTTPS serves the API at the supplied machine name. The host's physical-LAN interface does not expose the API. SSH remains available over Tailscale and is verified with a key before password authentication is changed.

## Service units

- `sandbox-host-api.service` — unprivileged, restart-on-failure, filesystem and syscall hardening
- `sandbox-host-runtime.service` — privileged but capability-bounded where practical
- `sandbox-host-egress.service` — unprivileged; no write access outside its runtime directory
- `sandbox-host-gc.timer` — expired sessions/snapshots and orphan jail cleanup
- `sandbox-host-health.timer` — image/kernel version, disk, cgroup, KVM, nftables, and API checks

## Recovery invariants

- Firewall installation is additive in a dedicated nftables table and tested before persistence.
- Tailscale and SSH rules are explicit and installed before any default-drop host input policy.
- Existing SSH sessions are kept open while a second connection verifies each access change.
- Schema migrations and image updates are transactional; the previous binary/image remains available for rollback.
- Startup reconciles database state with jail processes, network namespaces, cgroups, and disk images.
- A failed egress proxy causes fail-closed guest networking, not direct Internet access.

## Required verification

1. API schema and SDK contract tests against recorded upstream-shaped fixtures.
2. Vite + React + Tailwind create/install/build/dev/preview flow.
3. Stripe test-mode request, Convex dev/deploy client flow, and Claude Code authentication/API flow where credentials are supplied by the operator.
4. Positive OAuth discovery/token-path tests and negative Google Search/Drive/general API tests.
5. Negative egress: raw IP, custom DNS, DNS rebinding, alternate ports, IPv6, QUIC, SSH, ICMP, private/LAN/Tailnet/metadata/other-guest addresses, absent SNI, disallowed Host, redirect to disallowed name.
6. Guest root attempts against disks, `/dev/kvm`, namespaces, host processes, Unix sockets, and jail paths.
7. Fork bomb, memory pressure, disk fill, log flood, connection flood, and timeout enforcement.
8. Stop/resume, auto-snapshot, manual snapshot, fork, retention, expiry, and crash recovery.
9. Host reboot recovery with SSH/Tailscale/API availability.
10. Codex code review followed by remediation of all critical/high findings and documented disposition for lower findings.

