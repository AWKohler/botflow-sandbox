package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ai-club/sandbox-host/internal/egress"
	"github.com/ai-club/sandbox-host/internal/httpjson"
)

const (
	maxInitialBytes   = 64 << 10
	connectTimeout    = 10 * time.Second
	idleTimeout       = 2 * time.Minute
	maxConnsPerSource = 256
	maxTotalConns     = 8192
	maxInflightDNS    = 2048
)

type server struct {
	logger    *slog.Logger
	ceiling   egress.Policy
	mu        sync.RWMutex
	policies  map[string]egress.SessionPolicy
	upstream  string
	statePath string

	connMu    sync.Mutex
	connBySrc map[string]int
	connTotal int
	dnsTokens chan struct{}
}

func main() {
	configPath := envOr("SANDBOX_EGRESS_CONFIG", "/etc/sandbox-host/egress.json")
	b, err := os.ReadFile(configPath)
	if err != nil {
		panic(fmt.Errorf("read egress config: %w", err))
	}
	var config struct {
		Ceiling     egress.Policy `json:"ceiling"`
		DNSUpstream string        `json:"dnsUpstream"`
		StatePath   string        `json:"statePath"`
	}
	if err := json.Unmarshal(b, &config); err != nil {
		panic(fmt.Errorf("decode egress config: %w", err))
	}
	if err := config.Ceiling.Validate(); err != nil {
		panic(fmt.Errorf("invalid egress ceiling: %w", err))
	}
	if config.DNSUpstream == "" {
		config.DNSUpstream = "1.1.1.1:53"
	}
	if config.StatePath == "" {
		config.StatePath = "/run/sandbox-host/egress-state.json"
	}
	s := &server{
		logger: slog.New(slog.NewJSONHandler(os.Stdout, nil)), ceiling: config.Ceiling,
		policies: make(map[string]egress.SessionPolicy), upstream: config.DNSUpstream, statePath: config.StatePath,
		connBySrc: make(map[string]int), dnsTokens: make(chan struct{}, maxInflightDNS),
	}
	if err := s.loadPolicies(); err != nil {
		panic(fmt.Errorf("load egress state: %w", err))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	errCh := make(chan error, 4)
	go func() { errCh <- s.serveTCP(ctx, envOr("SANDBOX_EGRESS_TLS", ":10443"), s.handleTLS) }()
	go func() { errCh <- s.serveTCP(ctx, envOr("SANDBOX_EGRESS_HTTP", ":10080"), s.handleHTTP) }()
	go func() { errCh <- s.serveDNSUDP(ctx, envOr("SANDBOX_EGRESS_DNS", ":1053")) }()
	go func() { errCh <- s.serveControl(ctx, envOr("SANDBOX_EGRESS_SOCKET", "/run/sandbox-host/egress.sock")) }()
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			s.logger.Error("egress service failed", "error", err)
			os.Exit(1)
		}
	}
}

func (s *server) serveControl(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/sessions/{id}", s.putPolicy)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.deletePolicy)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		httpjson.Write(w, 200, map[string]string{"status": "ok"})
	})
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() { <-ctx.Done(); _ = httpServer.Shutdown(context.Background()) }()
	err = httpServer.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *server) putPolicy(w http.ResponseWriter, r *http.Request) {
	var policy egress.SessionPolicy
	if err := httpjson.Decode(w, r, &policy); err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	if policy.SessionID != r.PathValue("id") {
		httpjson.WriteError(w, 400, "session_mismatch", "session id does not match path")
		return
	}
	addr, err := netip.ParseAddr(policy.GuestIP)
	if err != nil || !addr.Is4() {
		httpjson.WriteError(w, 400, "invalid_guest_ip", "guestIp must be IPv4")
		return
	}
	if err := policy.Policy.Validate(); err != nil {
		httpjson.WriteError(w, 400, "invalid_policy", err.Error())
		return
	}
	effective := egress.Intersect(policy.Policy, s.ceiling)
	if len(effective.Rules) != len(policy.Policy.Rules) {
		httpjson.WriteError(w, 403, "policy_exceeds_ceiling", "one or more domains exceed the administrator ceiling")
		return
	}
	policy.Policy = effective
	s.mu.Lock()
	s.policies[addr.String()] = policy
	err = s.savePoliciesLocked()
	s.mu.Unlock()
	if err != nil {
		httpjson.WriteError(w, 500, "state_persist_failed", err.Error())
		return
	}
	httpjson.Write(w, 200, policy)
}

func (s *server) deletePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	for ip, policy := range s.policies {
		if policy.SessionID == id {
			delete(s.policies, ip)
		}
	}
	err := s.savePoliciesLocked()
	s.mu.Unlock()
	if err != nil {
		httpjson.WriteError(w, 500, "state_persist_failed", err.Error())
		return
	}
	httpjson.Write(w, 200, struct{}{})
}

func (s *server) loadPolicies() error {
	b, err := os.ReadFile(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var policies map[string]egress.SessionPolicy
	if err := json.Unmarshal(b, &policies); err != nil {
		return err
	}
	for ip, policy := range policies {
		addr, err := netip.ParseAddr(ip)
		if err != nil || !addr.Is4() || policy.GuestIP != ip || policy.SessionID == "" {
			return fmt.Errorf("invalid persisted policy for %q", ip)
		}
		if err := policy.Policy.Validate(); err != nil {
			return err
		}
		if effective := egress.Intersect(policy.Policy, s.ceiling); len(effective.Rules) != len(policy.Policy.Rules) {
			return fmt.Errorf("persisted policy for %q exceeds ceiling", ip)
		}
	}
	s.policies = policies
	return nil
}

func (s *server) savePoliciesLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0o750); err != nil {
		return err
	}
	b, err := json.Marshal(s.policies)
	if err != nil {
		return err
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath)
}

func (s *server) policyFor(remote net.Addr) (egress.SessionPolicy, bool) {
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		return egress.SessionPolicy{}, false
	}
	s.mu.RLock()
	policy, ok := s.policies[host]
	s.mu.RUnlock()
	return policy, ok
}

// admitConn enforces a per-guest and global concurrent-connection ceiling so a
// hostile guest cannot exhaust host memory or file descriptors by opening a
// flood of allowed connections. releaseConn must be called for each admission.
func (s *server) admitConn(src string) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.connTotal >= maxTotalConns || s.connBySrc[src] >= maxConnsPerSource {
		return false
	}
	s.connBySrc[src]++
	s.connTotal++
	return true
}

func (s *server) releaseConn(src string) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.connBySrc[src] > 0 {
		s.connBySrc[src]--
		if s.connBySrc[src] == 0 {
			delete(s.connBySrc, src)
		}
		s.connTotal--
	}
}

func sourceHost(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

func (s *server) serveTCP(ctx context.Context, address string, handler func(net.Conn)) error {
	ln, err := net.Listen("tcp4", address)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = ln.Close() }()
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Back off on transient accept errors (e.g. EMFILE) instead of
			// spinning in a tight retry loop that pins a CPU.
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff < time.Second {
				backoff *= 2
			}
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		src := sourceHost(conn.RemoteAddr())
		if !s.admitConn(src) {
			s.deny("tcp", conn.RemoteAddr(), "", "connection_limit")
			_ = conn.Close()
			continue
		}
		go func() {
			defer s.releaseConn(src)
			handler(conn)
		}()
	}
}

func (s *server) handleTLS(client net.Conn) {
	defer client.Close()
	policy, ok := s.policyFor(client.RemoteAddr())
	if !ok {
		s.deny("tls", client.RemoteAddr(), "", "missing_session_policy")
		return
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	initial, serverName, err := readClientHello(client)
	if err != nil || !policy.Policy.Allows(serverName, "", "") {
		s.deny("tls", client.RemoteAddr(), serverName, "domain_denied")
		return
	}
	upstream, err := dialPublic(serverName, "443")
	if err != nil {
		s.deny("tls", client.RemoteAddr(), serverName, "resolution_or_connect_failed")
		return
	}
	defer upstream.Close()
	_ = client.SetReadDeadline(time.Time{})
	if _, err := upstream.Write(initial); err != nil {
		return
	}
	s.logger.Info("egress_allowed", "session", policy.SessionID, "protocol", "tls", "host", serverName)
	proxy(client, upstream)
}

func (s *server) handleHTTP(client net.Conn) {
	defer client.Close()
	policy, ok := s.policyFor(client.RemoteAddr())
	if !ok {
		s.deny("http", client.RemoteAddr(), "", "missing_session_policy")
		return
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	initial, method, path, host, err := readHTTPHeader(client)
	if err != nil {
		s.deny("http", client.RemoteAddr(), host, "request_denied")
		return
	}
	// Pin plaintext egress to port 80. Without this a guest could smuggle an
	// arbitrary port into the Host header (e.g. "registry.npmjs.org:22") and
	// open a raw TCP tunnel to any port on an otherwise-allowlisted domain.
	host, portOK := httpHostPort80(host)
	if !portOK {
		s.deny("http", client.RemoteAddr(), host, "port_denied")
		return
	}
	if !policy.Policy.Allows(host, method, path) {
		s.deny("http", client.RemoteAddr(), host, "request_denied")
		return
	}
	upstream, err := dialPublic(host, "80")
	if err != nil {
		s.deny("http", client.RemoteAddr(), host, "resolution_or_connect_failed")
		return
	}
	defer upstream.Close()
	_ = client.SetReadDeadline(time.Time{})
	if _, err := upstream.Write(initial); err != nil {
		return
	}
	s.logger.Info("egress_allowed", "session", policy.SessionID, "protocol", "http", "host", host, "method", method, "path", path)
	proxy(client, upstream)
}

func (s *server) serveDNSUDP(ctx context.Context, address string) error {
	conn, err := net.ListenPacket("udp4", address)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = conn.Close() }()
	buf := make([]byte, 4096)
	for {
		n, remote, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		packet := append([]byte(nil), buf[:n]...)
		// Bound concurrent DNS handlers so a UDP flood cannot spawn unbounded
		// goroutines; excess queries are dropped rather than amplified.
		select {
		case s.dnsTokens <- struct{}{}:
		default:
			s.deny("dns", remote, "", "dns_rate_limit")
			continue
		}
		go func() {
			defer func() { <-s.dnsTokens }()
			s.handleDNSPacket(conn, remote, packet)
		}()
	}
}

func (s *server) handleDNSPacket(listener net.PacketConn, remote net.Addr, packet []byte) {
	policy, ok := s.policyFor(remote)
	name, err := dnsQuestionName(packet)
	if !ok || err != nil || !policy.Policy.Allows(name, "", "") {
		if response := dnsRefused(packet); response != nil {
			_, _ = listener.WriteTo(response, remote)
		}
		s.deny("dns", remote, name, "domain_denied")
		return
	}
	upstream, err := net.DialTimeout("udp4", s.upstream, 3*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	_ = upstream.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err := upstream.Write(packet); err != nil {
		return
	}
	response := make([]byte, 4096)
	n, err := upstream.Read(response)
	if err == nil {
		_, _ = listener.WriteTo(response[:n], remote)
	}
}

func (s *server) deny(protocol string, remote net.Addr, host, reason string) {
	s.logger.Warn("egress_denied", "protocol", protocol, "source", remote.String(), "host", host, "reason", reason)
}

// localHostAddresses is the set of IPs assigned to this host's interfaces,
// snapshotted at startup. dialPublic refuses to connect to any of them so that
// an allowlisted hostname which is rebound (DNS) to one of the host's own
// public/Tailnet addresses cannot be used to reach host-local services.
var localHostAddresses = loadLocalHostAddresses()

func loadLocalHostAddresses() map[netip.Addr]struct{} {
	set := make(map[netip.Addr]struct{})
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return set
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok {
			if addr, ok := netip.AddrFromSlice(ipNet.IP); ok {
				set[addr.Unmap()] = struct{}{}
			}
		}
	}
	return set
}

func isLocalHostAddress(addr netip.Addr) bool {
	_, ok := localHostAddresses[addr.Unmap()]
	return ok
}

// httpHostPort80 strips an explicit :80 from a Host header and rejects any
// other explicit port, so plaintext egress can only ever target port 80 of an
// allowlisted domain.
func httpHostPort80(host string) (string, bool) {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return host, true // no explicit port
	}
	if port != "80" {
		return host, false
	}
	return h, true
}

func dialPublic(host, defaultPort string) (net.Conn, error) {
	host = strings.TrimSpace(strings.ToLower(strings.TrimSuffix(host, ".")))
	if h, port, err := net.SplitHostPort(host); err == nil {
		host, defaultPort = h, port
	}
	if net.ParseIP(host) != nil {
		return nil, errors.New("direct IP destinations are denied")
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	var errs []error
	for _, addr := range addrs {
		if !egress.IsPublicAddress(addr) || isLocalHostAddress(addr) {
			continue
		}
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr.String(), defaultPort))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(append(errs, errors.New("no approved public address"))...)
}

func readHTTPHeader(conn net.Conn) ([]byte, string, string, string, error) {
	reader := bufio.NewReaderSize(conn, maxInitialBytes)
	var data []byte
	for len(data) < maxInitialBytes {
		b, err := reader.ReadByte()
		if err != nil {
			return nil, "", "", "", err
		}
		data = append(data, b)
		if len(data) >= 4 && string(data[len(data)-4:]) == "\r\n\r\n" {
			break
		}
	}
	if len(data) >= maxInitialBytes {
		return nil, "", "", "", errors.New("HTTP header too large")
	}
	lines := strings.Split(string(data), "\r\n")
	parts := strings.Fields(lines[0])
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "HTTP/1.") {
		return nil, "", "", "", errors.New("invalid HTTP request line")
	}
	host := ""
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "host") {
			host = strings.TrimSpace(value)
			break
		}
	}
	if host == "" {
		return nil, "", "", "", errors.New("HTTP Host is required")
	}
	return data, strings.ToUpper(parts[0]), parts[1], host, nil
}

func readClientHello(conn net.Conn) ([]byte, string, error) {
	data := make([]byte, 0, 4096)
	header := make([]byte, 5)
	for len(data) < maxInitialBytes {
		if _, err := io.ReadFull(conn, header); err != nil {
			return nil, "", err
		}
		if header[0] != 22 {
			return nil, "", errors.New("first TLS record is not a handshake")
		}
		length := int(binary.BigEndian.Uint16(header[3:5]))
		if length <= 0 || len(data)+5+length > maxInitialBytes {
			return nil, "", errors.New("invalid TLS record length")
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(conn, body); err != nil {
			return nil, "", err
		}
		data = append(data, header...)
		data = append(data, body...)
		if name, ok := parseSNI(data); ok {
			return data, name, nil
		}
	}
	return nil, "", errors.New("TLS ClientHello has no SNI")
}

func parseSNI(records []byte) (string, bool) {
	var handshake []byte
	for offset := 0; offset+5 <= len(records); {
		length := int(binary.BigEndian.Uint16(records[offset+3 : offset+5]))
		if offset+5+length > len(records) {
			return "", false
		}
		handshake = append(handshake, records[offset+5:offset+5+length]...)
		offset += 5 + length
	}
	if len(handshake) < 4 || handshake[0] != 1 {
		return "", false
	}
	length := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
	if length+4 > len(handshake) {
		return "", false
	}
	p := handshake[4 : 4+length]
	if len(p) < 34 {
		return "", false
	}
	i := 34
	if i >= len(p) {
		return "", false
	}
	i += 1 + int(p[i])
	if i+2 > len(p) {
		return "", false
	}
	i += 2 + int(binary.BigEndian.Uint16(p[i:i+2]))
	if i >= len(p) {
		return "", false
	}
	i += 1 + int(p[i])
	if i+2 > len(p) {
		return "", false
	}
	extLen := int(binary.BigEndian.Uint16(p[i : i+2]))
	i += 2
	if i+extLen > len(p) {
		return "", false
	}
	end := i + extLen
	for i+4 <= end {
		typ := binary.BigEndian.Uint16(p[i : i+2])
		ln := int(binary.BigEndian.Uint16(p[i+2 : i+4]))
		i += 4
		if i+ln > end {
			return "", false
		}
		if typ == 0 && ln >= 5 {
			ext := p[i : i+ln]
			listLen := int(binary.BigEndian.Uint16(ext[:2]))
			for j := 2; j+3 <= len(ext) && j < 2+listLen; {
				nameType := ext[j]
				nameLen := int(binary.BigEndian.Uint16(ext[j+1 : j+3]))
				j += 3
				if j+nameLen > len(ext) {
					return "", false
				}
				if nameType == 0 {
					name := strings.ToLower(strings.TrimSuffix(string(ext[j:j+nameLen]), "."))
					if name != "" && net.ParseIP(name) == nil {
						return name, true
					}
				}
				j += nameLen
			}
		}
		i += ln
	}
	return "", false
}

func proxy(a, b net.Conn) {
	_ = a.SetDeadline(time.Now().Add(idleTimeout))
	_ = b.SetDeadline(time.Now().Add(idleTimeout))
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

func dnsQuestionName(packet []byte) (string, error) {
	if len(packet) < 17 || binary.BigEndian.Uint16(packet[4:6]) != 1 {
		return "", errors.New("DNS request must contain exactly one question")
	}
	var labels []string
	for i := 12; i < len(packet); {
		length := int(packet[i])
		i++
		if length == 0 {
			if i+4 > len(packet) {
				return "", errors.New("truncated DNS question")
			}
			return strings.ToLower(strings.Join(labels, ".")), nil
		}
		if length > 63 || i+length > len(packet) {
			return "", errors.New("invalid DNS label")
		}
		labels = append(labels, string(packet[i:i+length]))
		i += length
	}
	return "", errors.New("truncated DNS request")
}

func dnsRefused(request []byte) []byte {
	if len(request) < 12 {
		return nil
	}
	response := append([]byte(nil), request...)
	flags := binary.BigEndian.Uint16(response[2:4])
	flags = (flags | 0x8000 | 0x0080) &^ 0x000f
	flags |= 0x0005
	binary.BigEndian.PutUint16(response[2:4], flags)
	binary.BigEndian.PutUint16(response[6:8], 0)
	binary.BigEndian.PutUint16(response[8:10], 0)
	binary.BigEndian.PutUint16(response[10:12], 0)
	return response
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
