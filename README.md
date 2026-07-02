# Sandbox Host

Self-hosted, Vercel-Sandbox-compatible execution service for a single Ubuntu/KVM host.

The security boundary is a jailed Firecracker microVM per active session. The public API follows Vercel's current `/api/v2/sandboxes` wire format, and the compatibility SDK preserves the `Sandbox`, `Session`, `Command`, `Snapshot`, and `FileSystem` programming model while allowing a non-Vercel API base URL.

Status: architecture and compatibility contract frozen; implementation follows the specifications in `docs/`.

## Documents

- [Research and API contract](docs/research.md)
- [Architecture and threat model](docs/architecture.md)
- [Compatibility contract](docs/compatibility.md)
- [Network policy](docs/network-policy.md)
- [Operations specification](docs/operations.md)

