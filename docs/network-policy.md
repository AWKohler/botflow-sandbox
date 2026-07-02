# Network policy

## Policy model

The effective policy is:

`deny special/private destinations` AND `administrator ceiling` AND `tenant/session request`.

Omitting a session policy selects the curated administrator ceiling. `deny-all` removes DNS and all egress. A user-defined list narrows the ceiling. Ordinary tenants cannot expand it. Rules are updated atomically and existing flows are terminated when their destination becomes denied.

HTTPS is validated by SNI without TLS decryption when no transformation is needed. Missing SNI and ECH-hidden names are denied. Plain HTTP is parsed at the gateway and validated by Host. Direct IP URLs are denied. DNS answers for names outside the effective policy are NXDOMAIN. The gateway ignores guest-supplied DNS results and resolves approved names itself before connecting.

## Initial global ceiling

The default is intentionally narrow and version-controlled.

### JavaScript/package and source bootstrap

- `registry.npmjs.org` — npm/pnpm package metadata and tarballs
- `cdn.jsdelivr.net` — Tailwind Play CDN and explicitly requested browser packages
- `github.com`, `api.github.com`, `codeload.github.com`
- `raw.githubusercontent.com`, `objects.githubusercontent.com`

NPM's registry also has write APIs. The gateway initially limits unauthenticated registry use to `GET` and `HEAD`; publishing and account mutations are denied. Git-over-SSH and arbitrary package registries are denied.

### Fonts

- `fonts.googleapis.com`
- `fonts.gstatic.com`

### Identity-only endpoints

- Google: `accounts.google.com`, `oauth2.googleapis.com`, `openidconnect.googleapis.com`, and `www.googleapis.com` only for the OAuth key/cert paths required by OIDC. Google Search and general Google APIs are not allowed.
- Microsoft: `login.microsoftonline.com` and `graph.microsoft.com` only for OIDC discovery, keys, token, and `/oidc/userinfo` paths.
- Apple: `appleid.apple.com` only for `/auth/authorize`, `/auth/token`, `/auth/keys`, and revocation endpoints.
- GitHub: `github.com` only for `/login/oauth/*`; `api.github.com` only for identity and approved repository operations.

Interactive authorization normally occurs in the end user's browser and is outside sandbox egress. The allowlist exists for server-side discovery, code exchange, key retrieval, refresh, and userinfo.

### Application services

- Stripe backend: `api.stripe.com`, `files.stripe.com`, and `uploads.stripe.com`. Browser-hosted Stripe UI/CDN domains are not required for sandbox egress but may be fetched by the user's browser.
- Convex: `*.convex.cloud` and `*.convex.site`; CLI control-plane hosts are added only after traffic-capture tests identify and document their exact official purpose.
- Claude Code: `api.anthropic.com`, `statsig.anthropic.com`; `sentry.io` is denied by default because error reporting is optional. Authentication/download hosts are enabled only when verified by an integration test.

Wildcard SaaS domains permit traffic to attacker-controlled tenants on that SaaS. They are compatibility allowances, not DLP controls. A high-sensitivity job must narrow or disable them.

Primary service references:

- [npm registry](https://docs.npmjs.com/using-npm/registry.html/)
- [Tailwind Play CDN](https://tailwindcss.com/docs/installation/play-cdn)
- [Google Fonts API](https://developers.google.com/fonts/docs/getting_started)
- [Google OpenID Connect](https://developers.google.com/identity/openid-connect/openid-connect)
- [Microsoft OpenID Connect](https://learn.microsoft.com/en-us/entra/identity-platform/v2-protocols-oidc)
- [Sign in with Apple token endpoint](https://developer.apple.com/documentation/signinwithapplerestapi/generate-and-validate-tokens)
- [GitHub OAuth](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps)
- [Stripe domains](https://docs.stripe.com/ips)
- [Convex deployment URLs](https://docs.convex.dev/client/react/deployment-urls)
- [Claude Code proxy requirements](https://docs.anthropic.com/en/docs/claude-code/corporate-proxy)

## Abuse controls

- Per-token request and creation rate limits.
- Per-session and per-tenant byte quotas in both directions.
- Connection concurrency and new-connection rate limits.
- DNS query rate limits and response-size caps.
- Maximum request header size and TLS ClientHello size.
- No raw sockets beyond the guest boundary; host blocks non-TCP egress.
- Audit records for policy changes and denied destinations without logging authorization headers or bodies.

