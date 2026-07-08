package main

import (
	"strings"
	"testing"
	"time"
)

func TestPreviewTokenRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	host := "p-deadbeef-5173.previews.example.com"
	exp := time.Now().Add(time.Hour).Unix()
	token := mintPreviewToken(secret, host, exp)
	gotExp, ok := verifyPreviewToken(secret, token, host, time.Now())
	if !ok {
		t.Fatalf("freshly minted token failed verification: %q", token)
	}
	if gotExp != exp {
		t.Fatalf("expiry mismatch: got %d want %d", gotExp, exp)
	}
}

func TestPreviewTokenHostCaseInsensitive(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	exp := time.Now().Add(time.Hour).Unix()
	token := mintPreviewToken(secret, "P-ABC-5173.Previews.Example.COM", exp)
	if _, ok := verifyPreviewToken(secret, token, "p-abc-5173.previews.example.com", time.Now()); !ok {
		t.Fatal("host comparison must be case-insensitive")
	}
}

func TestPreviewTokenRejectsExpired(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	host := "p-deadbeef-5173.previews.example.com"
	token := mintPreviewToken(secret, host, time.Now().Add(-time.Minute).Unix())
	if _, ok := verifyPreviewToken(secret, token, host, time.Now()); ok {
		t.Fatal("expired token verified")
	}
}

func TestPreviewTokenRejectsWrongHost(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	exp := time.Now().Add(time.Hour).Unix()
	token := mintPreviewToken(secret, "p-aaaa-5173.previews.example.com", exp)
	if _, ok := verifyPreviewToken(secret, token, "p-bbbb-5173.previews.example.com", time.Now()); ok {
		t.Fatal("token bound to one host verified against another")
	}
}

func TestPreviewTokenRejectsTamperedAndMalformed(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	host := "p-deadbeef-5173.previews.example.com"
	token := mintPreviewToken(secret, host, time.Now().Add(time.Hour).Unix())
	cases := []string{
		"",
		"v1",
		"v1.notanumber.sig",
		"v2." + strings.SplitN(token, ".", 2)[1],           // wrong version
		token[:len(token)-2],                               // truncated sig
		strings.Replace(token, ".", "..", 1),               // extra separator
		mintPreviewToken([]byte("wrong-secret-wrong-secret-wrong!"), host, time.Now().Add(time.Hour).Unix()),
	}
	for _, c := range cases {
		if _, ok := verifyPreviewToken(secret, c, host, time.Now()); ok {
			t.Fatalf("invalid token verified: %q", c)
		}
	}
}

// TestPreviewTokenCrossLanguageVector pins the wire format against a token
// minted by the Botflow (Node.js) implementation in
// webcontainer-ide/src/lib/preview-token.ts. If this breaks, the two sides
// have drifted and previews will 403.
//
// Vector generated with:
//
//	secret = "test-secret-0123456789abcdef0123456789abcdef"
//	exp    = 1900000000
//	host   = "p-abcdef0123456789abcdef01-5173.previews.example.com"
func TestPreviewTokenCrossLanguageVector(t *testing.T) {
	secret := []byte("test-secret-0123456789abcdef0123456789abcdef")
	host := "p-abcdef0123456789abcdef01-5173.previews.example.com"
	nodeToken := "v1.1900000000.WqHxdeqYOIWKU1Ml_yJC6HoRzDLTxHyhGVU9nyZkPcM"
	exp, ok := verifyPreviewToken(secret, nodeToken, host, time.Unix(1800000000, 0))
	if !ok {
		t.Fatal("Node-minted vector failed Go verification — token formats have drifted")
	}
	if exp != 1900000000 {
		t.Fatalf("unexpected expiry: %d", exp)
	}
	if got := mintPreviewToken(secret, host, 1900000000); got != nodeToken {
		t.Fatalf("Go mint differs from Node mint:\n  go:   %s\n  node: %s", got, nodeToken)
	}
}
