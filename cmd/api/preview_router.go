package main

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The preview router serves every tunnel-fronted preview subdomain
// (https://<label>.<previewDomain>) from a single plain-HTTP listener; the
// Cloudflare tunnel terminates TLS and proxies here, mirroring how Tailscale
// Funnel fronts the control plane. Per request it:
//
//  1. extracts the subdomain label from the Host header,
//  2. resolves it to an open route via previewManager.lookup (in-memory,
//     maintained by open/close, rebuilt on restart by reconciliation),
//  3. when previewSigningSecret is set, requires a valid signed token — the
//     `_bft` query param on the first document request, or the
//     `__bf_preview` cookie it sets for every follow-up request
//     (subresources, Vite HMR websocket),
//  4. reverse-proxies to the guest (WebSocket upgrades included).
//
// Failure responses are deliberately uniform and cache-suppressed so the
// router doesn't oracle which subdomains exist.

func (a *api) previewRouterHandler() http.Handler {
	secret := []byte(a.config.PreviewSigningSecret)
	suffix := "." + a.config.PreviewDomain
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every response from the router carries these, success or failure.
		// no-referrer keeps tokened URLs out of cross-origin Referer headers;
		// noindex keeps transient preview hosts out of search indexes.
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Robots-Tag", "noindex")

		host := strings.ToLower(hostWithoutPort(r.Host))
		if !strings.HasSuffix(host, suffix) {
			previewRouterReject(w, http.StatusNotFound)
			return
		}
		label := strings.TrimSuffix(host, suffix)
		if label == "" || strings.Contains(label, ".") {
			previewRouterReject(w, http.StatusNotFound)
			return
		}
		target, ok := a.previews.lookup(label)
		if !ok {
			previewRouterReject(w, http.StatusNotFound)
			return
		}

		if len(secret) > 0 {
			now := time.Now()
			authorized := false
			if c, err := r.Cookie(previewCookieName); err == nil {
				_, authorized = verifyPreviewToken(secret, c.Value, host, now)
			}
			if !authorized {
				token := r.URL.Query().Get(previewTokenQueryParam)
				exp, ok := verifyPreviewToken(secret, token, host, now)
				if !ok {
					previewRouterReject(w, http.StatusForbidden)
					return
				}
				// First tokened request: persist authorization in a host-only
				// cookie so subresource and websocket requests (which never
				// carry the query param) pass. SameSite=None + Partitioned is
				// required for a third-party iframe context under CHIPS.
				maxAge := exp - now.Unix()
				if maxAge > 0 {
					h.Add("Set-Cookie",
						previewCookieName+"="+token+
							"; Path=/; Max-Age="+strconv.FormatInt(maxAge, 10)+
							"; Secure; HttpOnly; SameSite=None; Partitioned")
				}
			}
		}

		target.proxy.ServeHTTP(w, r)
	})
}

// previewRouterReject writes a uniform, cache-suppressed error page.
func previewRouterReject(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	switch status {
	case http.StatusForbidden:
		_, _ = w.Write([]byte("preview access denied\n"))
	default:
		_, _ = w.Write([]byte("preview not found\n"))
	}
}

func hostWithoutPort(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}
