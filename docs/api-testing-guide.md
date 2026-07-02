# Sandbox Host — API testing guide (preliminary)

A hands-on guide for exercising the service over HTTP. Everything below was run
against the live host; request/response shapes are real. Pair this with
[SPEC.md](SPEC.md) for the design and [compatibility.md](compatibility.md) for
the Vercel SDK mapping.

---

## 1. Connecting

| | |
|---|---|
| **Public base URL** | `https://ai-club-pc-ms-7c56.taila01548.ts.net/api` (Tailscale Funnel) |
| **Tailnet base URL** | same host, reachable from your tailnet even with Funnel off |
| **Auth** | `Authorization: Bearer <token>` on every `/api/...` call |
| **Token** | in `~ai-club-pc/sandbox-host-credentials` on the host (`SANDBOX_TOKEN`). Rotated 2026-07-02; treat as a secret |
| **Tenant** | single token → `teamId=default`, `projectId=default` |

Health check (no auth, no `/api` prefix):

```bash
curl -s https://ai-club-pc-ms-7c56.taila01548.ts.net/health
# {"status":"ok"}
```

Set up a shell for the examples:

```bash
BASE=https://ai-club-pc-ms-7c56.taila01548.ts.net/api
TOKEN=<paste from the host credentials file>
auth=(-H "Authorization: Bearer $TOKEN")
```

> **Scope of the public URL:** only the control-plane API is public. Preview
> ports (20000–40000) are **not** funneled and remain tailnet-only for now.

## 2. How it works (tester's mental model)

- A **sandbox** is a named, persistent record. Creating one boots a **session**
  = one Firecracker microVM. Stopping snapshots the disk (if persistent) and
  frees the VM; getting a stopped sandbox with `?resume=true` boots a fresh
  session from its latest snapshot.
- You run **commands** and **file operations** against a *session id*, not the
  sandbox name. Commands execute inside the VM as the unprivileged
  `vercel-sandbox` user (uid 1000) in `/vercel/sandbox`; pass `"sudo": true` for
  root. There is no shell unless you invoke one (`bash -lc "…"`).
- The VM can only reach the **egress allowlist** (npm, GitHub, Google Fonts,
  Tier-1 OAuth, Stripe, Convex, Anthropic). Everything else is dropped.
- Runtimes: `node22`, `node24` (default), `node26`, `python3.13`. Each session
  is 2048 MiB RAM per vCPU.

Lifecycle: `create → running → (stop|timeout) → stopped → (resume) → running → …`
`delete` tears down the VM and all snapshots.

## 3. Quickstart

### curl

```bash
# create
curl -s "${auth[@]}" -H 'Content-Type: application/json' \
  -d '{"name":"demo","runtime":"node24","resources":{"vcpus":1},"timeout":300000}' \
  "$BASE/v2/sandboxes"

# grab the session id from the response, then run a command
SID=sess-...    # response.session.id
curl -s "${auth[@]}" -H 'Content-Type: application/json' \
  -d '{"cmd":"bash","args":["-lc","node --version"],"wait":true}' \
  "$BASE/v2/sandboxes/sessions/$SID/cmd"

# delete
curl -s "${auth[@]}" -X DELETE "$BASE/v2/sandboxes/demo?projectId=default&teamId=default"
```

### SDK (drop-in for `@vercel/sandbox`)

```bash
export SANDBOX_API_URL=https://ai-club-pc-ms-7c56.taila01548.ts.net/api
export SANDBOX_TOKEN=<token>
export SANDBOX_TEAM_ID=default
export SANDBOX_PROJECT_ID=default
```

```ts
import { Sandbox } from "@sandbox-host/sdk"; // or alias to @vercel/sandbox

const sandbox = await Sandbox.create({ runtime: "node24", resources: { vcpus: 1 } });
const cmd = await sandbox.runCommand({ cmd: "node", args: ["--version"] });
console.log(cmd.exitCode, await cmd.stdout());
await sandbox.stop();
```

## 4. Sandboxes

### Create — `POST /v2/sandboxes`

Request body (all fields optional except that a name is generated if omitted):

```jsonc
{
  "name": "demo",                 // [a-zA-Z0-9-], <=64; auto-generated if omitted
  "runtime": "node24",            // node22|node24|node26|python3.13 (default node24)
  "resources": { "vcpus": 1 },    // 1, or an even number up to 8; memory is fixed at 2048*vcpus
  "timeout": 300000,              // ms until auto-stop; default 300000, max 2700000
  "ports": [4173],                // preview ports to expose (<=15)
  "persistent": true,             // default true (snapshot on stop)
  "env": { "FOO": "bar" },        // env injected into every command
  "networkPolicy": { … },         // see §8; omit for the default allowlist
  "source": { "type": "git", "url": "https://github.com/owner/repo", "depth": 1 },
  "tags": { "owner": "qa" },
  "snapshotExpiration": 86400000, // ms; auto-expire snapshots
  "keepLastSnapshots": { "count": 3 }
}
```

Response `201`:

```jsonc
{
  "sandbox": {
    "name": "demo", "persistent": true, "region": "local-nyc",
    "vcpus": 1, "memory": 2048, "runtime": "node24", "timeout": 300000,
    "networkPolicy": { "mode": "custom", "allowedDomains": ["registry.npmjs.org", …] },
    "createdAt": 1783001880000, "updatedAt": 1783001880000,
    "currentSessionId": "sess-…", "status": "running",
    "statusUpdatedAt": 1783001880000, "cwd": "/vercel/sandbox"
  },
  "session": { "id": "sess-…", "memory": 2048, "vcpus": 1, "runtime": "node24",
               "status": "running", "startedAt": 1783001880000, "cwd": "/vercel/sandbox", … },
  "routes": [ { "url": "http://…ts.net:20004", "subdomain": "local-sess-…-4173", "port": 4173 } ]
}
```

> Guest IP and the internal agent token are intentionally **not** in any
> response.

### List / Get / Update / Delete

```bash
# list (query params required for tenant scoping)
curl -s "${auth[@]}" "$BASE/v2/sandboxes?projectId=default&teamId=default"

# get; auto-resumes a stopped sandbox unless resume=false
curl -s "${auth[@]}" "$BASE/v2/sandboxes/demo?projectId=default&teamId=default&resume=true"

# update (timeout/tags/ports/runtime/resources/policy/retention; live resource or
# runtime changes require the sandbox to be stopped first -> 409 otherwise)
curl -s "${auth[@]}" -X PATCH -H 'Content-Type: application/json' \
  -d '{"timeout":600000,"tags":{"phase":"qa"}}' \
  "$BASE/v2/sandboxes/demo?projectId=default&teamId=default"

# delete (stops the VM and removes all its snapshots)
curl -s "${auth[@]}" -X DELETE "$BASE/v2/sandboxes/demo?projectId=default&teamId=default"
```

## 5. Sessions & lifecycle

```bash
# list sessions (optionally ?name=demo)
curl -s "${auth[@]}" "$BASE/v2/sandboxes/sessions?projectId=default&teamId=default"

# get one session
curl -s "${auth[@]}" "$BASE/v2/sandboxes/sessions/$SID"

# stop (snapshots if the sandbox is persistent), returns {session, sandbox, snapshot?}
curl -s "${auth[@]}" -X POST "$BASE/v2/sandboxes/sessions/$SID/stop"

# explicit snapshot (stop + always persist); optional {"expiration": ms}
curl -s "${auth[@]}" -X POST -H 'Content-Type: application/json' -d '{}' \
  "$BASE/v2/sandboxes/sessions/$SID/snapshot"

# extend the running timeout
curl -s "${auth[@]}" -X POST -H 'Content-Type: application/json' \
  -d '{"duration":300000}' "$BASE/v2/sandboxes/sessions/$SID/extend-timeout"
```

A stopped session returns `410 sandbox_stopped` on command/file calls — resume
via `GET /v2/sandboxes/{name}` (which mints a new session id) first.

## 6. Commands

### Run — `POST /v2/sandboxes/sessions/{id}/cmd`

```jsonc
{
  "cmd": "bash",                       // or "command"; the binary to exec (no implicit shell)
  "args": ["-lc", "pnpm install"],
  "cwd": "/vercel/sandbox",            // optional
  "env": { "CI": "1" },                // merged over the sandbox env
  "sudo": false,                        // true => run as root instead of uid 1000
  "wait": true,                          // stream to completion (see below)
  "logs": false                          // include log lines in the streamed response
}
```

- **`wait: false`** → `201 { "command": { "id", "name", "args", "cwd",
  "sessionId", "exitCode": null, "startedAt" } }`. Poll it, or stream its logs.
- **`wait: true`** → an `application/x-ndjson` stream: a first line with the
  command, then (if `logs:true`) `{"stream":"stdout|stderr","data":"…"}` lines,
  then a final line with the completed command including `exitCode`.

```bash
# fire-and-forget, then poll to completion
CID=$(curl -s "${auth[@]}" -H 'Content-Type: application/json' \
  -d '{"cmd":"bash","args":["-lc","sleep 2; echo done"]}' \
  "$BASE/v2/sandboxes/sessions/$SID/cmd" | jq -r .command.id)

curl -s "${auth[@]}" "$BASE/v2/sandboxes/sessions/$SID/cmd/$CID?wait=true"      # blocks until exit
curl -s "${auth[@]}" "$BASE/v2/sandboxes/sessions/$SID/cmd/$CID/logs"          # NDJSON logs
curl -s "${auth[@]}" -X POST -H 'Content-Type: application/json' \
  -d '{"signal":15}' "$BASE/v2/sandboxes/sessions/$SID/cmd/$CID/kill"          # signal the process group
```

Guest facts worth testing: commands run as uid 1000 in `/vercel/sandbox`; `node`,
`npm`, `pnpm`, `python3.13` are on `PATH`; per-command output is capped (~32 MiB,
then truncated); `@anthropic-ai/claude-code` installs via pnpm out of the box.

## 7. Filesystem

```bash
# mkdir
curl -s "${auth[@]}" -X POST -H 'Content-Type: application/json' \
  -d '{"path":"src","cwd":"/vercel/sandbox"}' "$BASE/v2/sandboxes/sessions/$SID/fs/mkdir"

# read (returns the raw file bytes as application/octet-stream; 404 if missing)
curl -s "${auth[@]}" -X POST -H 'Content-Type: application/json' \
  -d '{"path":"package.json","cwd":"/vercel/sandbox"}' \
  "$BASE/v2/sandboxes/sessions/$SID/fs/read"

# write: POST a gzipped tar of the files; X-Cwd sets the extraction base.
tar -czf - -C ./local package.json src | curl -s "${auth[@]}" \
  -H 'Content-Type: application/gzip' -H 'X-Cwd: /vercel/sandbox' \
  --data-binary @- "$BASE/v2/sandboxes/sessions/$SID/fs/write"
```

Uploaded files are owned by uid 1000 (so normal commands can read/write them).
The SDK's `writeFiles([{ path, content }])` builds the tar for you.

## 8. Network egress policy

Every session's egress = `deny private/host/special` **AND** the admin ceiling
**AND** your session request. Omit `networkPolicy` to get the full ceiling. You
can only *narrow* it.

```jsonc
// default (omit networkPolicy) → the full allowlist
// tighten to a subset (must be within the ceiling, else 403):
{ "networkPolicy": { "mode": "custom", "allow": ["registry.npmjs.org", "*.convex.cloud"] } }
// legacy form also accepted:
{ "networkPolicy": { "allowedDomains": ["registry.npmjs.org"] } }
// kill all egress:
{ "networkPolicy": "deny-all" }
// rejected (403 policy_exceeds_ceiling):
{ "networkPolicy": "allow-all" }
```

Update a running session's policy live:

```bash
curl -s "${auth[@]}" -X POST -H 'Content-Type: application/json' \
  -d '{"mode":"custom","allow":["registry.npmjs.org"]}' \
  "$BASE/v2/sandboxes/sessions/$SID/network-policy"
```

**Default ceiling (allowed):** `registry.npmjs.org`, `cdn.jsdelivr.net`, GitHub
(`github.com`, `api.github.com`, `codeload.github.com`, `raw.githubusercontent.com`,
`objects.githubusercontent.com`), Google Fonts, Tier-1 OAuth (Google/Microsoft/
Apple/GitHub identity endpoints), Stripe (`api.stripe.com`, `js.stripe.com`,
Checkout/Connect…), `*.convex.cloud`/`*.convex.site` + Convex control plane, and
Anthropic/Claude (`api.anthropic.com`, `console.anthropic.com`, `claude.ai`).
Anything else (e.g. `example.com`, Google Search) is denied. Quick check inside a
session:

```bash
curl -s "${auth[@]}" -H 'Content-Type: application/json' -d '{"cmd":"bash","args":["-lc",
  "curl -s -o /dev/null -w allowed=%{http_code} https://registry.npmjs.org/react; echo; \
   curl -s --max-time 5 https://example.com >/dev/null && echo REACHED || echo blocked"],
  "wait":true,"logs":true}' "$BASE/v2/sandboxes/sessions/$SID/cmd"
```

## 9. Previews

Create with `ports`, then read `routes[].url`. Each route reverse-proxies the
host URL to that guest port.

```bash
# inside the session, start a server on 4173 in the background. With wait:false
# (the default) the POST returns 201 immediately and the process keeps running.
curl -s "${auth[@]}" -H 'Content-Type: application/json' -d '{"cmd":"node","args":
  ["-e","require(\"http\").createServer((_,r)=>r.end(\"ok\")).listen(4173,\"0.0.0.0\")"]}' \
  "$BASE/v2/sandboxes/sessions/$SID/cmd"
# then GET the sandbox and read routes[].url. (SDK equivalent: runCommand({ …, detached: true }))
```

> Preview URLs are currently `http://…ts.net:<port>` and only reachable **from
> your tailnet** (Funnel doesn't cover 20000–40000). Public previews need the
> subdomain router + a real tunnel — see SPEC §10.

## 10. Limits & runtimes

| Limit | Value |
|---|---|
| Runtimes | `node22`, `node24`, `node26`, `python3.13` |
| vCPUs | 1, or an even number up to 8 |
| Memory | fixed at 2048 MiB × vCPUs |
| Timeout | default 300000 ms, max 2700000 ms |
| Preview ports | ≤ 15 per sandbox |
| Concurrent sandboxes | 10 per token |
| Command output | ~32 MiB/command, then truncated |
| File upload | ≤ 256 MiB per write |

## 11. Error codes

| HTTP | `error.code` | Meaning |
|---|---|---|
| 400 | `invalid_*` | malformed request / bad field |
| 401 | `unauthorized` | missing/invalid bearer token |
| 403 | `project_forbidden`, `policy_exceeds_ceiling` | token not scoped to project, or policy too broad |
| 404 | `sandbox_not_found`, `session_not_found`, `snapshot_not_found` | unknown resource |
| 409 | `sandbox_exists`, `live_resources_unsupported`, `live_runtime_unsupported` | name taken, or a change that needs a stopped sandbox |
| 410 | `sandbox_stopped` | session is stopped; resume first |
| 422 | `command_start_failed`, `source_clone_failed` | request valid but couldn't be carried out |
| 429 | `concurrency_limit` | per-token sandbox cap reached |
| 500 / 503 | `store_failed`, `runtime_create_failed`, `capacity_exhausted` | internal error / host at capacity (retry) |

Errors are `{ "error": { "code": "...", "message": "..." } }`.

## 12. Testing checklist

1. `GET /health` → `{"status":"ok"}`; unauthenticated `/api/...` → `401`.
2. Create → run `node --version` / `python3.13 --version` → delete.
3. `pnpm create vite` + `pnpm add tailwindcss @tailwindcss/vite` + `vite build`.
4. Egress: `registry.npmjs.org` reachable; `example.com` / Google Search blocked.
5. Install `convex`, `stripe`, `@anthropic-ai/claude-code`; `claude --version`.
6. Stop → confirm `410` on commands → `GET ?resume=true` → new session id → files persisted.
7. Snapshot, then create a new sandbox from `{"source":{"type":"snapshot","snapshotId":"…"}}`.
8. Concurrency: create 11 sandboxes on one token → 11th returns `429`.
9. Isolation (from inside a session, expect all to fail): reach `169.254.169.254`,
   a raw public IP over TLS, another guest's `10.200.0.x`, or a host tailnet IP.
