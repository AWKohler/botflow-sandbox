# Research and API contract

Research freeze: 2026-06-30. The upstream SDK source was inspected at Vercel `sandbox` commit `d9f59836d063f0efb173bc9344cba8ac3c1ac066` in addition to the current product documentation.

## What Vercel Sandbox is

Vercel Sandbox is an untrusted-code execution primitive based on one Firecracker microVM per running session. Its current model distinguishes a long-lived named **sandbox** from a running **session**. A persistent sandbox automatically captures its filesystem when a session stops and creates a new session from the latest snapshot on resume. Processes do not persist across sessions.

The stock environment is Amazon Linux 2023, runs ordinary commands as `vercel-sandbox`, exposes `/vercel/sandbox` as the default writable working directory, and permits full root execution through the SDK's `sudo` option. Current stock runtimes are `node22`, `node24`, `node26`, and `python3.13`; `node24` is the default.

Vercel exposes ports through per-session routes, supports command output as NDJSON streams, has file upload/download operations, and lets clients change egress policy while a session is running. The default Vercel policy is `allow-all`.

Primary references:

- [Vercel Sandbox overview](https://vercel.com/docs/sandbox)
- [Vercel JavaScript SDK reference](https://vercel.com/docs/sandbox/sdk-reference)
- [Vercel persistence model](https://vercel.com/docs/sandbox/concepts/persistent-sandboxes)
- [Vercel snapshots](https://vercel.com/docs/sandbox/concepts/snapshots)
- [Vercel firewall](https://vercel.com/docs/sandbox/concepts/firewall)
- [Vercel pricing and limits](https://vercel.com/docs/sandbox/pricing)
- [Vercel SDK and CLI source](https://github.com/vercel/sandbox)

## Current limits that shape compatibility

Vercel assigns 2 GiB RAM per requested vCPU. Valid CPU allocations are 1 or even values from 2 upward; the default is 2 vCPUs. It allows up to 15 exposed ports in current documentation. The default timeout is five minutes. Current hosted limits vary by plan (45 minutes or 24 hours) and hosted concurrency varies from 10 to 2,000.

This service preserves request and response fields but applies a local admission controller. Hardware availability, the configured per-tenant quota, and the host reserve are authoritative; a compatible error is returned when capacity is unavailable.

## Wire API

The official SDK defaults to `https://vercel.com/api` and appends `teamId` to requests. This service implements the same resource paths under its own `/api` base.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v2/sandboxes` | Create sandbox and first session |
| `GET` | `/api/v2/sandboxes` | List sandboxes with cursor pagination |
| `GET` | `/api/v2/sandboxes/{name}` | Get and optionally resume a sandbox |
| `PATCH` | `/api/v2/sandboxes/{name}` | Update persistent configuration |
| `DELETE` | `/api/v2/sandboxes/{name}` | Delete sandbox and owned snapshots |
| `GET` | `/api/v2/sandboxes/sessions` | List sessions |
| `GET` | `/api/v2/sandboxes/sessions/{id}` | Get session and port routes |
| `POST` | `/api/v2/sandboxes/sessions/{id}/cmd` | Start command; optionally wait and stream NDJSON |
| `GET` | `/api/v2/sandboxes/sessions/{id}/cmd/{cmd}` | Get or wait for a command |
| `POST` | `/api/v2/sandboxes/sessions/{id}/cmd/{cmd}/kill` | Signal a command |
| `GET` | `/api/v2/sandboxes/sessions/{id}/cmd/{cmd}/logs` | Stream retained NDJSON logs |
| `POST` | `/api/v2/sandboxes/sessions/{id}/fs/mkdir` | Create directory |
| `POST` | `/api/v2/sandboxes/sessions/{id}/fs/write` | Upload gzip-compressed tar stream |
| `POST` | `/api/v2/sandboxes/sessions/{id}/fs/read` | Download an octet stream |
| `POST` | `/api/v2/sandboxes/sessions/{id}/interactive` | Create interactive connection metadata |
| `POST` | `/api/v2/sandboxes/sessions/{id}/network-policy` | Live session policy update |
| `POST` | `/api/v2/sandboxes/sessions/{id}/extend-timeout` | Extend expiration |
| `POST` | `/api/v2/sandboxes/sessions/{id}/snapshot` | Stop and snapshot a session |
| `POST` | `/api/v2/sandboxes/sessions/{id}/stop` | Stop session; auto-snapshot if persistent |
| `GET` | `/api/v2/sandboxes/snapshots` | List snapshots |
| `GET` | `/api/v2/sandboxes/snapshots/tree` | Walk snapshot ancestry |
| `GET` | `/api/v2/sandboxes/snapshots/{id}` | Get snapshot |
| `DELETE` | `/api/v2/sandboxes/snapshots/{id}` | Delete snapshot |

### Core create request

`POST /api/v2/sandboxes` accepts:

- `projectId`, `name`, `ports`, `timeout`, `persistent`
- `source`: `git`, `tarball`, or `snapshot`
- `resources.vcpus`
- `runtime` or `image` (mutually exclusive)
- `networkPolicy`, `env`, `tags`
- `snapshotExpiration`, `keepLastSnapshots`

The response contains `{ sandbox, session, routes, resumed? }`.

### Lifecycle states

Session states are `pending`, `running`, `stopping`, `stopped`, `failed`, `aborted`, and `snapshotting`. Snapshot states are `created`, `deleted`, and `failed`.

### Command protocol

Commands have `id`, `name`, `args`, `cwd`, `sessionId`, `exitCode`, and `startedAt`. A waiting command request returns `application/x-ndjson`: first a `{command}` object, then `{stream:"stdout"|"stderr",data}` records, and finally a `{command}` object whose `exitCode` is an integer. Detached commands return the first `{command}` object immediately. Logs are retained with a configured byte ceiling and stream using the same record format.

### Network policy shape

The SDK accepts `allow-all`, `deny-all`, or:

```json
{
  "allow": ["registry.npmjs.org", "*.convex.cloud"],
  "subnets": {
    "allow": [],
    "deny": ["0.0.0.0/0"]
  }
}
```

The record form maps a domain to ordered rules. Rules may match method, path, query entries, and headers and may transform headers or forward a request. Vercel matches HTTPS domains using TLS SNI; plain HTTP cannot be securely filtered by domain in Vercel's documented implementation. Vercel terminates TLS only when transformations/forwarding require it and installs a per-sandbox CA in that case.

## Firecracker findings

Firecracker is purpose-built for multi-tenant workloads and minimizes the emulated device surface. It does not filter network traffic; the operator must enforce egress on the host. Production guidance requires the matching `jailer`, a dedicated unprivileged UID/GID, root-owned jail inputs, namespaces, cgroups/resource limits, and the production seccomp filter.

Version `v1.15.1` is the selected floor because it is the current release and includes the fix for CVE-2026-5747 plus an unbounded virtio-rng allocation fix. Deployment must verify the release checksum and never silently downgrade.

Primary references:

- [Firecracker project and design](https://github.com/firecracker-microvm/firecracker)
- [Firecracker jailer](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md)
- [Production host setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md)
- [Firecracker snapshot semantics](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md)
- [Firecracker v1.15.1 release](https://github.com/firecracker-microvm/firecracker/releases/tag/v1.15.1)

## Audited host

The target is Ubuntu 24.04.4 LTS, kernel 6.17.0-22, Ryzen 7 5800X (8 cores/16 threads), 62 GiB RAM, 8 GiB swap, and a 931 GiB NVMe. At audit time about 60 GiB RAM and 713 GiB storage were available. AMD-V, `/dev/kvm`, cgroup v2 (`cpu`, `cpuset`, `io`, `memory`, and `pids` controllers), AppArmor, and Secure Boot are enabled. The user belongs to `sudo`, `libvirt`, and `kvm`.

The current host firewall has permissive input/forward policies and legacy libvirt DNAT entries. Host hardening must replace these with a separately named nftables table without flushing Tailscale's tables until verified. SSH and the API remain reachable over Tailscale throughout the transition.

