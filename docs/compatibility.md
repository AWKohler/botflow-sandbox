# Compatibility contract

## SDK migration

The compatibility package preserves the upstream exports and class behavior. Applications use either:

```ts
import { Sandbox } from "@sandbox-host/sdk";
```

or install the service-provided compatibility tarball under the existing dependency name, allowing the import to remain `@vercel/sandbox`.

Configuration uses:

```sh
SANDBOX_API_URL=https://ai-club-pc-ms-7c56.taila01548.ts.net/api
SANDBOX_TOKEN=...
SANDBOX_TEAM_ID=default
SANDBOX_PROJECT_ID=default
```

Explicit `token`, `teamId`, and `projectId` parameters remain supported. Vercel OIDC is not accepted as local authentication.

## Supported surface

- `Sandbox.create`, `list`, `get`, `getOrCreate`, `fork`
- `Sandbox.runCommand`, `getCommand`, `stop`, `delete`, `update`, `extendTimeout`, `snapshot`, `domain`
- `Sandbox.readFile`, `readFileToBuffer`, `downloadFile`, `writeFiles`, `mkDir`
- `sandbox.fs` methods implemented by upstream through commands and file primitives
- `Session` accessors and command/file/lifecycle methods
- `Command`, `CommandFinished`, retained logs, wait, output, stdout, stderr, and kill
- `Snapshot.get`, `list`, `tree`, and delete
- cursor pagination and workflow serialization metadata
- live network policy updates within the administrator ceiling

## Intentional differences

| Area | Vercel | This service |
|---|---|---|
| Default egress | `allow-all` | administrator curated allowlist |
| `allow-all` request | accepted | rejected for ordinary tenants |
| Arbitrary allowed domain/CIDR | accepted by policy | intersected with or rejected by global ceiling |
| Region | Vercel region | constant `local-nyc` |
| Runtime filesystem | Amazon Linux 2023 | Amazon Linux 2023-compatible local image; exact package set documented per image |
| Resource capacity | cloud plan limits | live single-host admission control |
| Preview URL | Vercel HTTPS subdomain | Tailnet-private opaque route; exact URL form is deployment-specific |
| Interactive shell | Vercel websocket endpoint | compatibility endpoint; terminal support may be staged after command API |
| Custom VCR images | Vercel Container Registry | rejected until an audited image-import pipeline exists |
| Drives | Vercel beta drives | not in initial compatibility target |
| Credential transforms | hosted TLS interception | supported only for explicitly configured administrator broker rules |

The service returns structured `error.code` values and HTTP statuses expected by SDK retry/lifecycle logic, including not-found, stopped (`410`), stopping/snapshotting (`422`), validation (`400`), unauthorized (`401`), forbidden policy (`403`), conflict (`409`), capacity (`429`/`503`), and internal runtime failure (`500`).

