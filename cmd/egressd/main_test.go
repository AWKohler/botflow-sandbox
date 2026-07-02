package main

import (
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"

	"github.com/ai-club/sandbox-host/internal/egress"
)

func TestParseSNIFromTLSClientHello(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer clientSide.Close()
	defer serverSide.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		client := tls.Client(clientSide, &tls.Config{ServerName: "registry.npmjs.org", MinVersion: tls.VersionTLS12})
		_ = client.Handshake()
	}()
	header := make([]byte, 5)
	if _, err := io.ReadFull(serverSide, header); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(binary.BigEndian.Uint16(header[3:5])))
	if _, err := io.ReadFull(serverSide, body); err != nil {
		t.Fatal(err)
	}
	name, ok := parseSNI(append(header, body...))
	if !ok || name != "registry.npmjs.org" {
		t.Fatalf("parseSNI() = %q, %v", name, ok)
	}
}

func TestPolicyStateRoundTrip(t *testing.T) {
	rule := egress.Rule{Domain: "registry.npmjs.org"}
	path := filepath.Join(t.TempDir(), "state.json")
	first := &server{
		ceiling: egress.Policy{Rules: []egress.Rule{rule}},
		policies: map[string]egress.SessionPolicy{
			"10.200.0.2": {SessionID: "session", GuestIP: "10.200.0.2", Policy: egress.Policy{Rules: []egress.Rule{rule}}},
		},
		statePath: path,
	}
	if err := first.savePoliciesLocked(); err != nil {
		t.Fatal(err)
	}
	second := &server{ceiling: first.ceiling, policies: make(map[string]egress.SessionPolicy), statePath: path}
	if err := second.loadPolicies(); err != nil {
		t.Fatal(err)
	}
	if got := second.policies["10.200.0.2"]; got.SessionID != "session" || !got.Policy.Allows("registry.npmjs.org", "", "") {
		t.Fatalf("unexpected restored policy: %#v", got)
	}
}

func TestDNSQuestionName(t *testing.T) {
	packet := []byte{0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0,
		8, 'r', 'e', 'g', 'i', 's', 't', 'r', 'y', 5, 'n', 'p', 'm', 'j', 's', 3, 'o', 'r', 'g', 0,
		0, 1, 0, 1}
	name, err := dnsQuestionName(packet)
	if err != nil || name != "registry.npmjs.org" {
		t.Fatalf("dnsQuestionName() = %q, %v", name, err)
	}
}
