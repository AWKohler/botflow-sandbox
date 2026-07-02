package main

import (
	"strings"
	"testing"
)

func TestRandomIDUsesMinimumSecureEntropy(t *testing.T) {
	id := randomID("sess", 1)
	if !strings.HasPrefix(id, "sess-") {
		t.Fatalf("unexpected id prefix: %q", id)
	}
	if len(strings.TrimPrefix(id, "sess-")) < 32 {
		t.Fatalf("resource id has less than 128 bits of encoded entropy: %q", id)
	}
}
