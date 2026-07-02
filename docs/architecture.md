# Architecture and threat model

## Security objective

Arbitrary guest root code must not read or modify host data, address host/Tailnet/LAN services, communicate with another sandbox, exceed assigned CPU/memory/PID/disk/network limits, or open an unapproved egress destination. A compromised API credential must be constrained to its tenant and quota.

No single mechanism is treated as the boundary. The design uses nested controls:

1. AMD-V/KVM hardware virtualization.
2. One Firecracker process and one microVM per active session.
3. Firecracker `jailer`: dedicated UID/GID, chroot/pivot-root, mount/PID/network namespaces, resource limits, seccomp, and no inherited environment.
4. cgroup v2 CPU, memory, PID, and I/O ceilings around the VMM.
5. A unique network namespace and L2 segment for every VM.
6. Host nftables default-drop rules for all guest-originated traffic.
7. A protocol-aware egress gateway that re-resolves approved names and rejects direct IP, private/special IP, missing SNI, UDP/QUIC, and every unapproved protocol.
8. An unprivileged network-facing API separated from the privileged runtime daemon by a root-owned Unix socket and narrow RPC schema.
9. Per-tenant authentication, ownership checks, rate limits, quotas, timeouts, bounded logs, and audit records.

## Components

### `sandbox-api`

Unprivileged HTTP service. It authenticates bearer tokens, validates Vercel-compatible request schemas, enforces tenant quotas, persists metadata in SQLite, streams command output, and serves the compatibility SDK. It cannot create TAP devices, alter nftables, mount filesystems, or invoke KVM.

### `sandboxd`

Root-only runtime daemon reachable through `/run/sandbox-host/sandboxd.sock`. Its RPC methods are typed operations such as create, stop, snapshot, execute, signal, read, and write. It never accepts a shell command string for host execution. It creates namespaces/TAP/veth devices, prepares jail inputs, starts the matching Firecracker jailer, and applies cgroups.

### guest agent

A small static Linux binary is PID 1 (or supervised directly by PID 1) inside the guest. It mounts the minimal pseudo-filesystems, configures the fixed guest address, reaps children, runs commands without a shell, retains bounded command logs, provides file streaming, and performs an orderly filesystem sync/shutdown. Normal commands run as UID/GID `vercel-sandbox`; `sudo:true` runs as guest root.

### `egressd`

Unprivileged proxy/gateway. Host nftables redirects all guest TCP/80 and TCP/443 traffic to it and all DNS to its controlled resolver. For TLS it parses ClientHello, requires an allowed SNI name, resolves that name itself, rejects non-public addresses, and connects to its own resolution rather than a guest-selected IP. For HTTP it validates the Host header and re-resolves it. UDP other than redirected DNS is dropped; QUIC is intentionally unavailable.

Dynamic per-session policies are passed over an authenticated local control socket and can only narrow the administrator's global ceiling. A session request for `allow-all`, arbitrary CIDRs, or unapproved domains is rejected.

### preview router

Maps opaque, time-limited route tokens to a session and exposed guest port. It never routes to the guest agent port. The API uses bearer authentication and no domain cookies, avoiding credential exposure to untrusted preview origins. The first deployment is Tailnet-private.

## Storage and snapshots

The ext4 host filesystem does not support reflinks. A bounded sparse loopback file formatted as Btrfs is mounted at `/var/lib/sandbox-host` solely for sandbox data. Runtime base images and writable raw ext4 guest disks are Btrfs reflinks. This makes filesystem snapshots and forks fast without repartitioning the remotely managed host.

Snapshot creation is deliberately crash-consistent only after the guest has stopped and synced. The runtime then creates an immutable reflink of the raw disk, computes stored/allocated size, and records parentage. Resuming creates a new writable reflink and a new session. No Firecracker memory snapshot is used; this matches Vercel's documented filesystem persistence rather than preserving processes.

## Admission and dynamic capacity

The admission controller reads cgroup and host pressure for every create/resume. It reserves at least 4 GiB RAM, two logical CPUs worth of scheduler capacity, and configured disk headroom for the host. Guest RAM follows Vercel's 2 GiB/vCPU contract. CPU may be oversubscribed only up to the configured factor; memory is never intentionally overcommitted.

Admission requires all of:

- requested RAM plus VMM overhead below the lower of configured capacity and current available memory minus reserve;
- aggregate CPU allocation below the configured oversubscription ceiling;
- adequate Btrfs free space and per-tenant storage quota;
- global and per-tenant concurrent-session limits;
- acceptable host PSI/load and no active OOM condition.

Existing sessions remain within their cgroups. New sessions receive a capacity error with a retry hint rather than risking the host.

## Host protection rules

Guest interfaces are matched by an exact generated set, never a loose prefix supplied by a caller. Guest traffic is dropped before routing to:

- loopback, link-local, multicast, broadcast, CGNAT/Tailscale, RFC1918, benchmarking, documentation, and reserved ranges;
- the host's LAN, Tailnet, libvirt, and sandbox management ranges;
- metadata endpoints such as `169.254.169.254`;
- other sandbox subnets;
- all protocols except controlled DNS and redirected TCP/80/443.

Ingress to guests is possible only through preview-router mappings and the runtime daemon's agent channel. Guests cannot bind a host port directly.

## Trust limits

KVM, the host kernel, Firecracker, the jailer, `sandboxd`, and the host firewall are in the trusted computing base. A VM escape or host-kernel vulnerability can cross the boundary; timely patching and the current Firecracker release are mandatory.

An approved programmable SaaS is an egress channel by definition. The global allowlist prevents arbitrary destinations and reduces abuse, but it cannot prove that data sent to `api.anthropic.com`, an attacker-controlled Convex deployment, or a shared OAuth provider is non-sensitive. Secrets needed by untrusted workloads should use narrow credential-brokering rules and never be placed in command environment variables.

