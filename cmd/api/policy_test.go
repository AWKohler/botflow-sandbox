package main

import (
	"encoding/json"
	"testing"

	"github.com/ai-club/sandbox-host/internal/egress"
)

func TestParsePolicyEnforcesCeiling(t *testing.T) {
	ceiling := egress.Policy{Rules: []egress.Rule{{Domain: "registry.npmjs.org"}, {Domain: "*.convex.cloud"}}}
	if _, _, err := parsePolicy(json.RawMessage(`"allow-all"`), ceiling); err == nil {
		t.Fatal("allow-all should be rejected")
	}
	policy, _, err := parsePolicy(json.RawMessage(`{"allow":["registry.npmjs.org","demo.convex.cloud"]}`), ceiling)
	if err != nil || len(policy.Rules) != 2 {
		t.Fatalf("expected narrowed policy, got %#v, %v", policy, err)
	}
	if _, _, err := parsePolicy(json.RawMessage(`{"allow":["evil.example"]}`), ceiling); err == nil {
		t.Fatal("domain outside ceiling should be rejected")
	}
}
