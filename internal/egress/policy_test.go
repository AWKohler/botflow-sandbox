package egress

import (
	"net/netip"
	"testing"
)

func TestPolicyAllows(t *testing.T) {
	p := Policy{Rules: []Rule{
		{Domain: "registry.npmjs.org", Methods: []string{"GET", "HEAD"}},
		{Domain: "*.convex.cloud"},
		{Domain: "accounts.google.com", PathPrefixes: []string{"/.well-known/", "/o/oauth2/"}},
	}}
	tests := []struct {
		host, method, path string
		want               bool
	}{
		{"registry.npmjs.org", "GET", "/react", true},
		{"registry.npmjs.org", "PUT", "/evil", false},
		{"demo.convex.cloud", "POST", "/api", true},
		{"convex.cloud", "POST", "/api", false},
		{"accounts.google.com", "GET", "/.well-known/openid-configuration", true},
		{"accounts.google.com", "GET", "/search", false},
		{"evilregistry.npmjs.org", "GET", "/", false},
	}
	for _, test := range tests {
		if got := p.Allows(test.host, test.method, test.path); got != test.want {
			t.Errorf("Allows(%q,%q,%q)=%v want %v", test.host, test.method, test.path, got, test.want)
		}
	}
}

func TestPublicAddress(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "100.93.37.55", "169.254.169.254", "192.168.1.1", "::1", "fc00::1"} {
		if IsPublicAddress(netip.MustParseAddr(value)) {
			t.Errorf("expected %s to be denied", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !IsPublicAddress(netip.MustParseAddr(value)) {
			t.Errorf("expected %s to be public", value)
		}
	}
}

func TestIntersect(t *testing.T) {
	ceiling := Policy{Rules: []Rule{{Domain: "registry.npmjs.org", Methods: []string{"GET", "HEAD"}}, {Domain: "*.convex.cloud"}}}
	requested := Policy{Rules: []Rule{{Domain: "registry.npmjs.org"}, {Domain: "a.convex.cloud"}, {Domain: "evil.example"}}}
	got := Intersect(requested, ceiling)
	if len(got.Rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(got.Rules))
	}
	if len(got.Rules[0].Methods) != 2 {
		t.Fatalf("ceiling method constraints were not retained: %#v", got.Rules[0])
	}
}
