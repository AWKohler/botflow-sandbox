# Sandbox Host — Service Specification

A self-hosted, drop-in replacement for [Vercel Sandbox](https://vercel.com/docs/vercel-sandbox)
that runs untrusted user code inside hardware-isolated Firecracker microVMs on a
single Linux host, exposes the Vercel Sandbox HTTP/SDK contract, and permits
egress only to an administrator-curated allowlist of services.

This document is the authoritative overview. Deeper treatments live in
[architecture.md](architecture.md), [network-policy.md](network-policy.md),
[compatibility.md](compatibility.md), [operations.md](operations.md), and
[research.md](research.md).

---

## 1. What it is

- **Goal:** let a web platform that currently calls `@vercel/sandbox` point at
  private hardware with (ideally) a one-line change, while keeping the host
  provably safe from hostile guest code and constraining what the network can
  reach.
- **Deployment:** one Ubuntu 24.04 host (AMD Ryzen 5800X, 64 GB RAM), reachable
  **only over Tailscale**. There is no public listener. The API is bound to
  `127.0.0.1:8080` and published on the tailnet via Tailscale Serve (TLS).
- **Workload target:** pnpm-based Vite + React + Tailwind apps that integrate
  Stripe, Convex, Claude Code, and Tier-1 OAuth providers. The runtime image
  ships Node 22/24/26, Python 3.13, and pnpm 10.
- **Tier:** intended for a free tier — admission control, quotas, and a strict
  egress ceiling are first-class, not afterthoughts.

## 2. Threat model & security objective

The **guest is fully hostile**: assume an attacker controls all code in the
microVM including root. The two hard guarantees are:

1. **Host isolation** — guest code cannot read/modify host state, reach host or
   Tailnet/LAN/metadata services, reach another sandbox, or exceed its assigned
   CPU/memory/PID/disk limits.
2. **Allowlist-only egress** — guest code cannot open any network destination
   outside the configured ceiling (no arbitrary exfiltration or lateral
   movement).

The **API caller is semi-trusted** (it is the operator's own platform holding a
single admin bearer token). Tenancy checks exist and are enforced, but the
system is not designed to defend one paying customer against another; it is
designed to defend the *host* against *guests*.

Defense is layered — no single mechanism is the boundary:

| Layer | Control |
|---|---|
| Hardware | AMD-V/KVM virtualization; one Firecracker microVM per session |
| VMM jail | `jailer`: dedicated UID/GID, chroot, mount/PID/net namespaces, seccomp, resource-limits, `--new-pid-ns` |
| cgroup v2 | per-VM `memory.max`, `pids.max`, `cpu.max` |
| Network ns | unique netns + /30 veth per VM; guests never share an L2 segment |
| Host firewall | nftables default-drop for all guest-originated traffic; only DNS/80/443 from guest interfaces are redirected to the gateway |
| Egress gateway | protocol-aware; validates SNI/Host, re-resolves names itself, denies direct-IP, private/special/host-assigned addresses, non-80 ports, and non-TCP/QUIC |
| Privilege split | network-facing API (`sandbox-api`) is separated from root `sandboxd` by a root-owned socket reachable only by the API's group; the egress control socket is reachable only by root and the egress user |
| Tenancy | bearer-token auth, per-token project/team scoping, ownership checks, quotas, timeouts, bounded logs |

See [architecture.md](architecture.md) for the full component and trust-boundary
description.

## 3. Components

- **`sandbox-api`** (unprivileged, `sandbox-api` user) — the Vercel-compatible
  HTTP surface. Authenticates tokens, validates schemas, enforces quotas,
  persists metadata in a BoltDB file, proxies command/file traffic to guests,
  runs the preview reverse-proxies, and owns snapshot GC + reboot
  reconciliation. Cannot touch KVM, nftables, TAP devices, or the data root
  beyond its own DB subdirectory.
- **`sandboxd`** (root) — the runtime daemon behind
  `/run/sandbox-host/sandboxd.sock`. Typed RPC only (create/stop/snapshot/
  policy/delete-snapshot/list-sessions); never accepts a host shell string. It
  creates namespaces/veth/tap, prepares the jail, launches the Firecracker
  jailer, applies cgroups, and enforces host admission (memory, CPU, storage).
- **guest agent** — a static Linux binary running as PID 1 inside the VM. Mounts
  pseudo-filesystems, configures the fixed guest IP, reaps children, runs
  commands with no shell (UID 1000 by default, root only on `sudo:true`), keeps
  bounded per-command logs, streams files, and performs an orderly sync +
  power-off.
- **`egressd`** (unprivileged, `sandbox-egress` user) — the egress gateway. Host
  nftables transparently redirects guest DNS→`:1053`, HTTP→`:10080`,
  HTTPS→`:10443`. It enforces the allowlist and per-source connection/DNS rate
  ceilings.
- **preview router** — per-port reverse proxies inside `sandbox-api`, bound only
  to the Tailscale interface IP, mapping an external `host:port` to a guest port.
  Never routes to the guest agent port.

## 4. API surface (Vercel-compatible)

Base path `/api/v2/sandboxes`. All routes require `Authorization: Bearer <token>`.

| Method & path | Purpose |
|---|---|
| `POST /api/v2/sandboxes` | Create a sandbox (+first session) |
| `GET /api/v2/sandboxes` | List sandboxes for a project |
| `GET /api/v2/sandboxes/{name}` | Get a sandbox; `?resume=true` (default) auto-resumes a stopped one |
| `PATCH /api/v2/sandboxes/{name}` | Update timeout/tags/resources/runtime/ports/policy/retention |
| `DELETE /api/v2/sandboxes/{name}` | Stop + delete a sandbox and its snapshots |
| `GET /api/v2/sandboxes/sessions` | List sessions |
| `GET /api/v2/sandboxes/sessions/{id}` | Get a session |
| `POST /…/sessions/{id}/cmd` | Start a command (`wait`/`logs` stream NDJSON) |
| `GET /…/sessions/{id}/cmd/{cmd}` | Poll a command; `?wait=true` blocks to completion |
| `GET /…/sessions/{id}/cmd/{cmd}/logs` | Stream command logs (NDJSON) |
| `POST /…/sessions/{id}/cmd/{cmd}/kill` | Signal a command's process group |
| `POST /…/sessions/{id}/fs/mkdir` | Create a directory |
| `POST /…/sessions/{id}/fs/read` | Read a file |
| `POST /…/sessions/{id}/fs/write` | Upload a gzip tar of files (extracted as UID 1000) |
| `POST /…/sessions/{id}/network-policy` | Narrow the session egress policy live |
| `POST /…/sessions/{id}/extend-timeout` | Extend the session timeout |
| `POST /…/sessions/{id}/snapshot` | Snapshot (stop + persist disk) |
| `POST /…/sessions/{id}/stop` | Stop (snapshot if persistent) |
| `GET /…/snapshots`, `/snapshots/{id}`, `/snapshots/tree` | Snapshot listing/detail/tree |
| `DELETE /…/snapshots/{id}` | Delete a snapshot |

Errors return structured `{ error: { code, message } }` with the HTTP statuses
the SDK's retry/lifecycle logic expects: `400` validation, `401` unauthorized,
`403` policy-exceeds-ceiling, `404` not-found, `409` conflict/exists, `410`
stopped, `422` unprocessable, `429`/`503` capacity, `500` internal.

### SDK migration

The service ships a patched build of the official Apache-licensed SDK. Migration
is one of:

```ts
import { Sandbox } from "@sandbox-host/sdk";   // or alias the tarball to @vercel/sandbox
```

```sh
SANDBOX_API_URL=https://ai-club-pc-ms-7c56.taila01548.ts.net/api
SANDBOX_TOKEN=…
SANDBOX_TEAM_ID=default
SANDBOX_PROJECT_ID=default
```

Explicit `{ token, teamId, projectId }` parameters remain supported; Vercel OIDC
is not accepted as local auth. Supported surface and intentional differences
(curated egress vs `allow-all`, constant `local-nyc` region, Tailnet-private
preview URLs, no custom images/drives yet) are enumerated in
[compatibility.md](compatibility.md).

## 5. Network egress policy

Effective policy = `deny special/private/host destinations` **AND**
`administrator ceiling` **AND** `session request`. Omitting a session policy
uses the ceiling; a session policy may only *narrow* it (verified by
intersection; a request that exceeds the ceiling is rejected `403`).

Enforcement facts:

- Only DNS/TCP-80/TCP-443 from a guest interface are redirected to the gateway;
  everything else (other ports, UDP, QUIC, ICMP, direct routing) is dropped by
  nftables before leaving the host.
- HTTPS is filtered by SNI without TLS decryption; missing SNI is denied.
- Plain HTTP is parsed and filtered by Host; an explicit non-`80` port in the
  Host header is rejected (no port smuggling to allowlisted domains).
- The gateway ignores guest-supplied DNS answers and **re-resolves** approved
  names itself, then refuses to connect to any private/special-purpose address
  or any address assigned to a host interface (DNS-rebind-to-host defense).

The default ceiling (version-controlled in `deploy/egress.json`) covers: npm
registry + jsDelivr + GitHub raw/codeload; Google Fonts; Tier-1 OAuth identity
endpoints (Google/Microsoft/Apple/GitHub); Stripe API + JS + Checkout/Connect;
`*.convex.cloud`/`*.convex.site` + Convex control plane; and Anthropic/Claude
endpoints. Full rationale and per-service references are in
[network-policy.md](network-policy.md).

> **Residual risk (documented, not fully closed):** an allowlist of programmable
> SaaS is an egress channel by definition. SNI-based filtering without TLS
> termination cannot prevent domain-fronting on shared CDNs, and a wildcard
> (`*.convex.cloud`) permits attacker-controlled tenants on that SaaS. This
> reduces arbitrary exfiltration; it is **not** a DLP guarantee. High-sensitivity
> jobs must narrow or disable these rules.

## 6. Resource limits & dynamic capacity

Guest RAM follows Vercel's **2 GiB per vCPU** contract. Each VM gets a cgroup
`memory.max = (RAM + 256 MiB overhead)`, `pids.max = 2048`, and
`cpu.max = vcpus × 100%`.

Admission (in `sandboxd`) rejects a create/resume unless **all** hold:

- **Instantaneous memory:** current `MemAvailable − request ≥ HostReserveMiB`
  (default 4096).
- **Committed memory:** `MemTotal − Σ(committed guest RAM + overhead) − request
  ≥ HostReserveMiB`. This is the important one — it prevents rapid creation from
  oversubscribing physical RAM before guests fault their pages in.
- **CPU:** `Σ allocated vCPUs + request ≤ LogicalCPUs × CPUOvercommit` (default
  overcommit 1.0).
- **Storage:** Btrfs free space `≥ StorageReserveMiB` (default 20480).

Over the ceiling, callers get `503 capacity_exhausted` with a retry hint;
existing sessions are never disturbed.

## 7. Storage, snapshots & lifecycle

- A bounded sparse loopback file formatted **Btrfs** is mounted at
  `/var/lib/sandbox-host` (the host root ext4 lacks reflinks). Base runtime
  images and per-VM writable ext4 disks are **reflink** copies — instant, CoW.
- **Snapshots** are crash-consistent-after-quiesce: `sandboxd` asks the guest to
  sync + power off, **waits for the VM's cgroup to drain** (bounded timeout,
  force-kill fallback), then reflinks the disk and records size/parentage. No
  Firecracker *memory* snapshot is taken — this matches Vercel's filesystem
  persistence model.
- **Resume** creates a fresh writable reflink + new session from the sandbox's
  current snapshot.
- **GC:** an API loop enforces snapshot `expiration` and `keepLastSnapshots`
  retention (never deleting a sandbox's current resume point); a systemd timer
  sweeps interrupted `*.tmp` writes.
- **Reboot recovery:** on start, `sandboxd` re-adopts live VMs (recomputing all
  host paths from validated IDs) and the API reconciles its DB against
  `sandboxd`'s authoritative `GET /v1/sessions`, transitioning
  records whose VM is gone to `stopped` (freeing quota; still resumable).

## 8. Operations

Install/build/run and recovery are documented in [operations.md](operations.md).
Summary:

1. **Host install** — `deploy/install-host.sh` (root): installs
   nftables/btrfs/firecracker (checksum-verified), creates the Btrfs store,
   dedicated users/groups, root-owned data dirs, config, and systemd units. It
   **preserves an existing admin token** across re-runs.
2. **Guest image** — `deploy/build-rootfs.sh` (root): builds the 12 GiB base
   ext4 image (Node 22/24/26, Python 3.13, pnpm 10, guest agent, Claude Code
   build-script pre-approval) and clones per-runtime reflink images. Builds
   against a **minimal synthetic `/dev`** rather than the host device tree.
3. **Services** (systemd, start order enforced by dependencies):
   `sandbox-host-firewall` → `-egress` → `-runtime` → `-api`, plus `-gc.timer`.
   The runtime `Requires=` the firewall, so a VM can never boot without the
   guest drop-rules in force.
4. **Publish** — Tailscale Serve fronts `127.0.0.1:8080` with TLS on the tailnet.

Config files (`/etc/sandbox-host/*.json`) are mode `0640`, group-readable only
by the owning service. The admin token hash lives in `api.json`; the plaintext
token is written once to `~ai-club-pc/sandbox-host-credentials` (`0600`).

## 9. Security review dispositions

An adversarial Codex review (12 findings) was run against the tree; all are
resolved or explicitly documented:

| # | Finding | Disposition |
|---|---|---|
| 1 | Firewall could fail open on reload/stop | **Fixed** — idempotent atomic ruleset; no table-deleting stop/reload; runtime `Requires` firewall |
| 2 | API-writable data root → root path deletion via persisted state | **Fixed** — data root now root-owned + API-private DB subdir; `sandboxd` recomputes all paths from validated scalars |
| 3 | Egress checks only start-of-connection (port smuggling, fronting) | **Fixed** the port smuggling (pin `:80`); TLS domain-fronting/keep-alive documented as an SNI-filtering limitation |
| 4 | Shared service group collapsed API/egress/runtime boundary | **Fixed** — dedicated primary groups + private runtime dirs; sockets not cross-reachable |
| 5 | Create/resume admission races orphaned VMs | **Fixed** — per-name lifecycle lock |
| 6 | No storage quota / snapshot GC | **Fixed** — snapshot expiry+retention GC loop; free-space admission ceiling |
| 7 | Egress had no connection/DNS ceilings | **Fixed** — per-source + global conn caps, bounded DNS fan-out, accept backoff, systemd Memory/Tasks/NOFILE limits |
| 8 | Preview routes unauthenticated | **Documented** — bound only to the tailnet IP; guests cannot reach the tailnet; recommend Tailscale ACLs scoping the preview port range to the platform host (full capability-token router deferred to avoid breaking `.domain()` compat) |
| 9 | Rootfs build ran package code as root with host `/dev` | **Mitigated** — minimal synthetic `/dev`; build-time trust assumption documented |
| 10 | `IsPublicAddress` allowed host's own IPs | **Fixed** — deny any host-assigned address on connect |
| 11 | Snapshot not quiesced (fixed 500 ms) | **Fixed** — wait for guest cgroup to drain before copy |
| 12 | Reboot left stale `running` state | **Fixed** — API reconciles against runtime's authoritative session list |

## 10. Known limitations & future hardening

- **TLS domain-fronting / HTTP keep-alive re-use** on shared CDN infrastructure
  is not prevented (would require a TLS-terminating egress CA). Documented in §5.
- **Preview isolation** relies on tailnet trust + recommended ACLs rather than
  per-route capability tokens (§9 #8).
- **Live flow revocation** on policy narrowing is not implemented; narrowed
  policies apply to new connections, and existing flows idle-time out (2 min).
- **Trusted computing base:** KVM, the host kernel, Firecracker, the jailer,
  `sandboxd`, and the host firewall. A VM-escape or host-kernel CVE crosses the
  boundary; keeping Firecracker (currently 1.15.1) and the kernel patched is
  mandatory.
- **Snapshot integrity manifest** (hash/verify on restore) is not yet recorded;
  snapshots rely on quiesce + Btrfs reflink atomicity.
