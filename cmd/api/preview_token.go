package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// Signed preview tokens (format v1) authorize requests to tunnel-fronted
// preview subdomains. Tokens are minted by the Botflow control plane with the
// shared previewSigningSecret and verified here; the format is:
//
//	v1.<exp>.<sig>
//	  exp = unix seconds (decimal)
//	  sig = base64url-unpadded( HMAC-SHA256( secret, "v1.<exp>.<host>" ) )
//	  host = lowercase preview hostname, no port
//
// Binding the signature to the hostname means a leaked token for one preview
// cannot be replayed against another sandbox's subdomain.

const (
	previewTokenQueryParam = "_bft"
	previewCookieName      = "__bf_preview"
)

// mintPreviewToken creates a v1 token for host expiring at exp (unix
// seconds). Used by tests and operator tooling; production tokens are minted
// by the Botflow side with the same construction.
func mintPreviewToken(secret []byte, host string, exp int64) string {
	payload := "v1." + strconv.FormatInt(exp, 10) + "." + strings.ToLower(host)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return "v1." + strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyPreviewToken checks a v1 token against the request host. Returns the
// token's expiry and whether it is valid. Signature comparison is
// constant-time; malformed tokens fail closed.
func verifyPreviewToken(secret []byte, token, host string, now time.Time) (int64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || exp <= now.Unix() {
		return 0, false
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return 0, false
	}
	payload := "v1." + parts[1] + "." + strings.ToLower(host)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	if !hmac.Equal(gotSig, mac.Sum(nil)) {
		return 0, false
	}
	return exp, true
}
