# Public HTTPS previews + control-plane via Cloudflare Tunnel

> **Ingress note (2026-07-17):** the same Cloudflare tunnel now also fronts the
> **control-plane API**. Two ingress rules, most-specific first:
>
> ```yaml
> ingress:
>   - hostname: "api.botflow-site.app"   # control plane
>     service: http://localhost:8080
>   - hostname: "*.botflow-site.app"     # preview router (labels are p-<hex>-<port>, no collision)
>     service: http://localhost:8090
>   - service: http_status:404
> ```
>
> Botflow's `SANDBOX_API_URL` = `https://api.botflow-site.app/api`. `:8080` binds
> `127.0.0.1` only, so cloudflared is its sole path in. The **Tailscale Funnel is
> OFF** — Tailscale stays up for tailnet SSH/ops, but there is no public
> `*.ts.net` ingress anymore. Re-enable in a pinch with
> `tailscale funnel --https=443 on` + `tailscale serve --https=443 --bg http://127.0.0.1:8080`.
> Security parity: both were bearer-token-gated public HTTPS; Cloudflare adds
> WAF/DDoS/rate-limiting and hides the tailnet hostname, and collapsing to one
> ingress shrinks the attack surface.

---


Fronts the preview ports with a Cloudflare tunnel so preview URLs become
`https://<label>.<preview domain>` — embeddable in the Botflow workspace
iframe from an HTTPS origin (fixes the mixed-content white iframe). The
control plane stays on Tailscale Funnel; only previews go through Cloudflare.

```
browser ──https──▶ Cloudflare edge ──tunnel──▶ cloudflared (host)
                                                  │ http 127.0.0.1:8090
                                                  ▼
                                          sandbox-api preview router
                                  Host: <label>.<domain> → guestIP:guestPort
```

## What the service does (already implemented)

- When `previewDomain` is set in `/etc/sandbox-host/api.json`, every route's
  `url` becomes `https://<label>.<previewDomain>` where `<label>` is a fresh
  96-bit random `p-<24hex>-<port>` (capability URL; the legacy
  `local-<sessionID>-<port>` form exceeded DNS's 63-char label limit). The
  tailnet `http://<host>:2xxxx` port listeners stay open as an operator
  back-door.
- A second plain-HTTP listener (`previewRouterListen`, default
  `127.0.0.1:8090`) resolves the Host-header label to the open route and
  reverse-proxies to the guest, WebSockets (Vite HMR) included. Unknown or
  malformed hosts get a uniform no-store 404.
- With `previewSigningSecret` set (≥32 chars), requests additionally need a
  signed token: `?_bft=v1.<exp>.<HMAC-SHA256(secret, "v1.<exp>.<host>")>` on
  the first document request; the router then sets a host-only
  `__bf_preview` cookie (`Secure; HttpOnly; SameSite=None; Partitioned`) so
  subresources and the HMR websocket authorize without the query param.
  Botflow mints these tokens (`src/lib/preview-token.ts`) with the same
  secret — see cross-language vector test `TestPreviewTokenCrossLanguageVector`.
  Leave the secret empty to run capability-URL-only (Vercel's model).

## Setup

### 1. Pick the domain (Cloudflare dashboard — the part you do)

The wildcard must be a FIRST-level wildcard on a zone for Cloudflare's free
Universal SSL to cover it:

- ✅ dedicated zone, e.g. `botflowpreview.app` → wildcard `*.botflowpreview.app`
- ⚠️ `*.previews.botflow-site.app` is a SECOND-level wildcard under the
  `botflow-site.app` zone → needs Advanced Certificate Manager (~$10/mo) or
  Cloudflare for SaaS. Works, just not free.

### 2. Create the tunnel (dashboard or CLI, on ai-club-pc)

```bash
# on the host
curl -fsSL https://pkg.cloudflare.com/cloudflared/install.sh | sudo bash   # or dashboard-managed install
cloudflared tunnel login
cloudflared tunnel create sandbox-previews
# note the tunnel UUID it prints
```

`/etc/cloudflared/config.yml`:

```yaml
tunnel: <TUNNEL-UUID>
credentials-file: /etc/cloudflared/<TUNNEL-UUID>.json
ingress:
  - hostname: "*.<PREVIEW-DOMAIN>"        # e.g. "*.botflowpreview.app"
    service: http://127.0.0.1:8090        # previewRouterListen
    originRequest:
      httpHostHeader: ""                  # keep the browser's Host header —
                                          # the router routes on it
  - service: http_status:404
```

DNS (dashboard): add a `*` CNAME on the zone → `<TUNNEL-UUID>.cfargotunnel.com`,
proxied (orange cloud). (`cloudflared tunnel route dns` can't create wildcard
records — do this one in the dashboard.)

Run it as a service:

```bash
sudo cloudflared service install     # installs cloudflared.service using /etc/cloudflared/config.yml
sudo systemctl enable --now cloudflared
```

### 3. Configure sandbox-api

Add to `/etc/sandbox-host/api.json`:

```jsonc
{
  // ... existing fields ...
  "previewDomain": "<PREVIEW-DOMAIN>",
  "previewRouterListen": "127.0.0.1:8090",
  "previewSigningSecret": "<openssl rand -base64 48>"   // optional but recommended
}
```

```bash
sudo systemctl restart sandbox-host-api
journalctl -u sandbox-host-api | grep preview_router_listening   # sanity
```

### 4. Configure Botflow (Vercel env)

```
PREVIEW_SIGNING_SECRET=<same value as previewSigningSecret>
```

Nothing else: the SDK's `sandbox.domain(port)` starts returning the https
URLs, Botflow appends `?_bft=…`, and the workspace's probe logic switches to
external probing automatically (it keys off the URL scheme).

### 5. Verify

```bash
# create a sandbox with a port, read routes[0].url → https://p-…-5173.<domain>
# start a server on the port, then:
curl -sI 'https://<label>.<domain>/?_bft=<token>'      # 200 + Set-Cookie
curl -sI 'https://<label>.<domain>/'                   # 403 (secret set) — token required
curl -sI 'https://p-bogus-5173.<domain>/'              # 404, Cache-Control: no-store
```

Existing running sessions keep their old tailnet URLs until they next stop
and resume (routes are re-minted on resume/restart).

## Notes / caveats

- **Vite HMR through the tunnel:** the wrapper config already sets
  `allowedHosts: true`. If HMR fails to reconnect through Cloudflare, add
  `server.hmr: { protocol: "wss", clientPort: 443 }` to Botflow's
  `buildBotflowViteConfig` — not pre-applied because default behavior usually
  works.
- **Cookie support:** `Partitioned` (CHIPS) cookies work in current
  Chrome/Edge/Firefox; older Safari may drop third-party cookies, which would
  break subresource auth. Fallback: unset `previewSigningSecret` and rely on
  capability URLs (still Vercel-parity security).
- **Previews are now public** (behind the token if the secret is set). The
  96-bit labels are unguessable, error responses don't oracle existing
  subdomains, and `Referrer-Policy: no-referrer` keeps tokened URLs out of
  outbound Referers.
- Router lookups are in-memory; on api restart `reconcileActiveSessions`
  re-opens live routes (with **new** labels/URLs — clients re-fetch them from
  the API, Botflow republishes on next dev-server start).
