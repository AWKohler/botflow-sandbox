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

func TestHTTPHostPort80(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantOK   bool
	}{
		{"registry.npmjs.org", "registry.npmjs.org", true},
		{"registry.npmjs.org:80", "registry.npmjs.org", true},
		{"registry.npmjs.org:22", "registry.npmjs.org:22", false},
		{"registry.npmjs.org:443", "registry.npmjs.org:443", false},
		{"github.com:8080", "github.com:8080", false},
	}
	for _, tc := range cases {
		host, ok := httpHostPort80(tc.in)
		if ok != tc.wantOK || (ok && host != tc.wantHost) {
			t.Fatalf("httpHostPort80(%q) = %q, %v; want %q, %v", tc.in, host, ok, tc.wantHost, tc.wantOK)
		}
	}
}

func TestConnAdmission(t *testing.T) {
	s := &server{connBySrc: make(map[string]int)}
	for i := 0; i < maxConnsPerSource; i++ {
		if !s.admitConn("10.200.0.2") {
			t.Fatalf("admit %d should have succeeded", i)
		}
	}
	if s.admitConn("10.200.0.2") {
		t.Fatal("per-source ceiling should reject the next connection")
	}
	if !s.admitConn("10.200.0.6") {
		t.Fatal("a different source must not be blocked by another source's count")
	}
	s.releaseConn("10.200.0.2")
	if !s.admitConn("10.200.0.2") {
		t.Fatal("a released slot should be reusable")
	}
	// Fully drain the first source and confirm the map entry is reclaimed.
	for s.connBySrc["10.200.0.2"] > 0 {
		s.releaseConn("10.200.0.2")
	}
	if _, ok := s.connBySrc["10.200.0.2"]; ok {
		t.Fatal("drained source should be removed from the map")
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
