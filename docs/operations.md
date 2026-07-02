# Operations specification

## Host layout

- `/usr/lib/sandbox-host/` — versioned, root-owned binaries and guest kernel
- `/etc/sandbox-host/` — configuration, mode `0640`, each file group-readable
  only by its owning service; the operator token file is `0600`
- `/var/lib/sandbox-host/` — dedicated Btrfs loopback mount. **Root-owned**
  (`0755`); `images/instances/snapshots/runtime/jailer` are root-only `0750`;
  `api/` is the API's private `0700` directory holding the BoltDB metadata store
  (`api.db`). No service user can rename or replace the runtime-state dirs.
- `/run/sandbox-host/` — root-owned; holds only `sandboxd.sock`
  (`root:sandbox-api 0660`)
- `/run/sandbox-egress/` — the egress service's private runtime dir (`0700`);
  holds `egress.sock` and the persisted policy state, reachable only by the
  egress user and root

## Network exposure

The initial API and preview router bind only to loopback and/or the Tailscale address. Tailscale HTTPS serves the API at the supplied machine name. The host's physical-LAN interface does not expose the API. SSH remains available over Tailscale and is verified with a key before password authentication is changed.

## Service units

- `sandbox-host-firewall.service` — oneshot; installs the idempotent atomic
  nftables ruleset. No `ExecStop` (rules stay in force even if the unit is
  stopped) so it can never fail open while guests run.
- `sandbox-host-egress.service` — unprivileged; private `RuntimeDirectory`;
  `MemoryMax`/`TasksMax`/`LimitNOFILE` ceilings; `Requires` the firewall
- `sandbox-host-runtime.service` — privileged; **`Requires` the firewall** so a
  VM cannot boot without guest drop-rules in force
- `sandbox-host-api.service` — unprivileged, dedicated group, writable only in
  its DB subdir, `MemoryMax`/`TasksMax`/`LimitNOFILE` ceilings
- `sandbox-host-gc.timer` — sweeps interrupted `*.tmp` writes (snapshot
  expiry/retention is enforced by the API GC loop; orphaned instances by
  `sandboxd` reconciliation)

Startup order is `firewall → egress → runtime → api`, enforced by unit
dependencies.

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

