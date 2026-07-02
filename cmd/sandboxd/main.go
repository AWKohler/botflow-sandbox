//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ai-club/sandbox-host/internal/egress"
	"github.com/ai-club/sandbox-host/internal/httpjson"
	"github.com/ai-club/sandbox-host/internal/runtimeproto"
	"github.com/ai-club/sandbox-host/internal/securetoken"
)

type config struct {
	DataDir           string   `json:"dataDir"`
	JailDir           string   `json:"jailDir"`
	FirecrackerBinary string   `json:"firecrackerBinary"`
	JailerBinary      string   `json:"jailerBinary"`
	KernelPath        string   `json:"kernelPath"`
	EgressSocket      string   `json:"egressSocket"`
	ListenSocket      string   `json:"listenSocket"`
	Region            string   `json:"region"`
	HostReserveMiB    int64    `json:"hostReserveMiB"`
	MaxVCPUs          int      `json:"maxVcpus"`
	CPUOvercommit     float64  `json:"cpuOvercommit"`
	AllowedRuntimes   []string `json:"allowedRuntimes"`
	SocketGID         int      `json:"socketGid"`
}

type daemon struct {
	config   config
	logger   *slog.Logger
	mu       sync.Mutex
	sessions map[string]*runtimeproto.Session
	slots    map[int]string
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,63}$`)

func main() {
	configPath := envOr("SANDBOX_RUNTIME_CONFIG", "/etc/sandbox-host/runtime.json")
	b, err := os.ReadFile(configPath)
	if err != nil {
		panic(fmt.Errorf("read runtime config: %w", err))
	}
	var cfg config
	if err := json.Unmarshal(b, &cfg); err != nil {
		panic(fmt.Errorf("decode runtime config: %w", err))
	}
	setConfigDefaults(&cfg)
	if err := validateConfig(cfg); err != nil {
		panic(err)
	}
	d := &daemon{config: cfg, logger: slog.New(slog.NewJSONHandler(os.Stdout, nil)), sessions: make(map[string]*runtimeproto.Session), slots: make(map[int]string)}
	if err := d.reconcileStartup(); err != nil {
		panic(fmt.Errorf("startup reconciliation: %w", err))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := d.serve(ctx); err != nil {
		panic(err)
	}
}

func setConfigDefaults(c *config) {
	if c.DataDir == "" {
		c.DataDir = "/var/lib/sandbox-host"
	}
	if c.JailDir == "" {
		c.JailDir = "/srv/jailer"
	}
	if c.FirecrackerBinary == "" {
		c.FirecrackerBinary = "/usr/bin/firecracker"
	}
	if c.JailerBinary == "" {
		c.JailerBinary = "/usr/bin/jailer"
	}
	if c.KernelPath == "" {
		c.KernelPath = "/usr/lib/sandbox-host/vmlinux"
	}
	if c.EgressSocket == "" {
		c.EgressSocket = "/run/sandbox-host/egress.sock"
	}
	if c.ListenSocket == "" {
		c.ListenSocket = "/run/sandbox-host/sandboxd.sock"
	}
	if c.Region == "" {
		c.Region = "local-nyc"
	}
	if c.HostReserveMiB == 0 {
		c.HostReserveMiB = 4096
	}
	if c.MaxVCPUs == 0 {
		c.MaxVCPUs = 8
	}
	if c.CPUOvercommit == 0 {
		c.CPUOvercommit = 1.0
	}
	if len(c.AllowedRuntimes) == 0 {
		c.AllowedRuntimes = []string{"node22", "node24", "node26", "python3.13"}
	}
}

func validateConfig(c config) error {
	for name, path := range map[string]string{"dataDir": c.DataDir, "jailDir": c.JailDir, "firecrackerBinary": c.FirecrackerBinary, "jailerBinary": c.JailerBinary, "kernelPath": c.KernelPath} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be absolute", name)
		}
	}
	if c.HostReserveMiB < 2048 || c.CPUOvercommit < 0.5 || c.CPUOvercommit > 4 {
		return errors.New("unsafe reserve or overcommit configuration")
	}
	return nil
}

func (d *daemon) serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(d.config.ListenSocket), 0o750); err != nil {
		return err
	}
	_ = os.Remove(d.config.ListenSocket)
	ln, err := net.Listen("unix", d.config.ListenSocket)
	if err != nil {
		return err
	}
	if err := os.Chmod(d.config.ListenSocket, 0o660); err != nil {
		_ = ln.Close()
		return err
	}
	if d.config.SocketGID > 0 {
		if err := os.Chown(d.config.ListenSocket, 0, d.config.SocketGID); err != nil {
			_ = ln.Close()
			return err
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		httpjson.Write(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/capacity", d.capacityHandler)
	mux.HandleFunc("POST /v1/sessions", d.createSession)
	mux.HandleFunc("POST /v1/sessions/{id}/stop", d.stopSession)
	mux.HandleFunc("PUT /v1/sessions/{id}/network-policy", d.updatePolicy)
	mux.HandleFunc("DELETE /v1/snapshots/{id}", d.deleteSnapshot)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() { <-ctx.Done(); _ = server.Shutdown(context.Background()) }()
	err = server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (d *daemon) createSession(w http.ResponseWriter, r *http.Request) {
	var req runtimeproto.CreateSessionRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	if err := d.validateCreate(req); err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	d.mu.Lock()
	if _, exists := d.sessions[req.SessionID]; exists {
		d.mu.Unlock()
		httpjson.WriteError(w, 409, "session_exists", "session already exists")
		return
	}
	if err := d.admitLocked(req); err != nil {
		d.mu.Unlock()
		httpjson.WriteError(w, 503, "capacity_exhausted", err.Error())
		return
	}
	slot, err := d.allocateSlotLocked(req.SessionID)
	if err != nil {
		d.mu.Unlock()
		httpjson.WriteError(w, 503, "address_capacity_exhausted", err.Error())
		return
	}
	token, err := securetoken.New(32)
	if err != nil {
		delete(d.slots, slot)
		d.mu.Unlock()
		httpjson.WriteError(w, 500, "token_failed", err.Error())
		return
	}
	session := d.newSession(req, slot, token)
	d.sessions[req.SessionID] = session
	d.mu.Unlock()

	if err := d.createSessionResources(r.Context(), session, req); err != nil {
		d.logger.Error("session creation failed", "session", req.SessionID, "error", err)
		_ = d.cleanupSession(context.Background(), session, true)
		d.mu.Lock()
		delete(d.sessions, req.SessionID)
		delete(d.slots, slot)
		d.mu.Unlock()
		httpjson.WriteError(w, 500, "runtime_create_failed", err.Error())
		return
	}
	if err := d.saveSession(session); err != nil {
		_ = d.cleanupSession(context.Background(), session, true)
		d.mu.Lock()
		delete(d.sessions, req.SessionID)
		delete(d.slots, slot)
		d.mu.Unlock()
		httpjson.WriteError(w, 500, "state_persist_failed", err.Error())
		return
	}
	d.logger.Info("session_created", "session", session.SessionID, "guestIp", session.GuestIP, "vcpus", session.VCPUs, "memoryMiB", session.MemoryMiB)
	httpjson.Write(w, 201, session)
}

func (d *daemon) validateCreate(req runtimeproto.CreateSessionRequest) error {
	if !safeID.MatchString(req.SessionID) {
		return errors.New("invalid session id")
	}
	if !contains(d.config.AllowedRuntimes, req.Runtime) {
		return errors.New("runtime is not allowed")
	}
	if req.VCPUs != 1 && (req.VCPUs < 2 || req.VCPUs%2 != 0) {
		return errors.New("vcpus must be 1 or an even number")
	}
	if req.VCPUs > d.config.MaxVCPUs {
		return errors.New("vcpus exceed host maximum")
	}
	if req.MemoryMiB != req.VCPUs*2048 {
		return errors.New("memory must equal 2048 MiB per vCPU")
	}
	if err := req.NetworkPolicy.Validate(); err != nil {
		return err
	}
	if req.SourceSnapshot != "" && !safeID.MatchString(req.SourceSnapshot) {
		return errors.New("invalid snapshot id")
	}
	return nil
}

func (d *daemon) admitLocked(req runtimeproto.CreateSessionRequest) error {
	capacity := d.capacityLocked()
	if capacity.AvailableMemoryMiB-int64(req.MemoryMiB) < d.config.HostReserveMiB {
		return errors.New("host memory reserve would be violated")
	}
	limit := int(float64(capacity.LogicalCPUs) * d.config.CPUOvercommit)
	if capacity.AllocatedVCPUs+req.VCPUs > limit {
		return errors.New("host CPU allocation ceiling reached")
	}
	return nil
}

func (d *daemon) capacityHandler(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	capacity := d.capacityLocked()
	d.mu.Unlock()
	httpjson.Write(w, 200, capacity)
}

func (d *daemon) capacityLocked() runtimeproto.Capacity {
	total, available := memInfo()
	allocated := 0
	for _, session := range d.sessions {
		allocated += session.VCPUs
	}
	return runtimeproto.Capacity{TotalMemoryMiB: total, AvailableMemoryMiB: available, ReservedMemoryMiB: d.config.HostReserveMiB, LogicalCPUs: runtime.NumCPU(), AllocatedVCPUs: allocated, ActiveSessions: len(d.sessions)}
}

func (d *daemon) allocateSlotLocked(id string) (int, error) {
	for slot := 1; slot < 16383; slot++ {
		if _, used := d.slots[slot]; !used {
			d.slots[slot] = id
			return slot, nil
		}
	}
	return 0, errors.New("no free guest subnet")
}

func (d *daemon) newSession(req runtimeproto.CreateSessionRequest, slot int, token string) *runtimeproto.Session {
	base := uint32(10)<<24 | uint32(200)<<16 | uint32(slot*4)
	gw := netip.AddrFrom4([4]byte{byte(base >> 24), byte(base >> 16), byte(base >> 8), byte(base + 1)})
	guest := netip.AddrFrom4([4]byte{byte(base >> 24), byte(base >> 16), byte(base >> 8), byte(base + 2)})
	short := shortID(req.SessionID)
	uid := 200000 + slot
	return &runtimeproto.Session{
		SessionID: req.SessionID, Runtime: req.Runtime, VCPUs: req.VCPUs, MemoryMiB: req.MemoryMiB,
		GuestIP: guest.String(), GatewayIP: gw.String(), AgentToken: token, Slot: slot, UID: uid,
		NetNS: "sh-" + short, HostVeth: "sv" + short,
		DiskPath:      filepath.Join(d.config.DataDir, "instances", req.SessionID, "rootfs.ext4"),
		JailRoot:      filepath.Join(d.config.JailDir, filepath.Base(d.config.FirecrackerBinary), req.SessionID, "root"),
		NetworkPolicy: req.NetworkPolicy,
	}
}

func (d *daemon) createSessionResources(ctx context.Context, s *runtimeproto.Session, req runtimeproto.CreateSessionRequest) error {
	if err := d.prepareDisk(ctx, s, req.SourceSnapshot); err != nil {
		return err
	}
	if err := d.setupNetwork(ctx, s); err != nil {
		return err
	}
	if err := d.setEgressPolicy(ctx, s, req.NetworkPolicy); err != nil {
		return err
	}
	if err := d.startFirecracker(ctx, s); err != nil {
		return err
	}
	if err := d.waitForAgent(ctx, s, 30*time.Second); err != nil {
		return err
	}
	return nil
}

func (d *daemon) prepareDisk(ctx context.Context, s *runtimeproto.Session, snapshot string) error {
	if err := os.MkdirAll(filepath.Dir(s.DiskPath), 0o700); err != nil {
		return err
	}
	source := filepath.Join(d.config.DataDir, "images", s.Runtime+".ext4")
	if snapshot != "" {
		source = filepath.Join(d.config.DataDir, "snapshots", snapshot, "rootfs.ext4")
	}
	if info, err := os.Stat(source); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("trusted source image unavailable: %s", source)
	}
	return run(ctx, "cp", "--reflink=always", "--sparse=auto", "--", source, s.DiskPath)
}

func (d *daemon) setupNetwork(ctx context.Context, s *runtimeproto.Session) error {
	peer := "p0"
	commands := [][]string{
		{"netns", "add", s.NetNS},
		{"link", "add", s.HostVeth, "type", "veth", "peer", "name", peer},
		{"link", "set", peer, "netns", s.NetNS},
		{"addr", "add", s.GatewayIP + "/30", "dev", s.HostVeth},
		{"link", "set", s.HostVeth, "up"},
	}
	for _, args := range commands {
		if err := run(ctx, "ip", args...); err != nil {
			return err
		}
	}
	nsCommands := [][]string{
		{"link", "set", "lo", "up"},
		{"link", "add", "br0", "type", "bridge"},
		{"tuntap", "add", "dev", "tap0", "mode", "tap", "user", strconv.Itoa(s.UID)},
		{"link", "set", peer, "master", "br0"}, {"link", "set", "tap0", "master", "br0"},
		{"link", "set", peer, "up"}, {"link", "set", "tap0", "up"}, {"link", "set", "br0", "up"},
	}
	for _, args := range nsCommands {
		full := append([]string{"netns", "exec", s.NetNS, "ip"}, args...)
		if err := run(ctx, "ip", full...); err != nil {
			return err
		}
	}
	if err := run(ctx, "nft", "add", "element", "inet", "sandbox_host", "guest_ifaces", "{", s.HostVeth, "}"); err != nil {
		return err
	}
	return nil
}

func (d *daemon) setEgressPolicy(ctx context.Context, s *runtimeproto.Session, policy egress.Policy) error {
	payload := egress.SessionPolicy{SessionID: s.SessionID, GuestIP: s.GuestIP, Policy: policy}
	return unixJSON(ctx, d.config.EgressSocket, http.MethodPut, "/v1/sessions/"+s.SessionID, payload, nil)
}

func (d *daemon) startFirecracker(ctx context.Context, s *runtimeproto.Session) error {
	if err := os.MkdirAll(s.JailRoot, 0o700); err != nil {
		return err
	}
	kernelPath := filepath.Join(s.JailRoot, "vmlinux")
	if err := run(ctx, "cp", "--", d.config.KernelPath, kernelPath); err != nil {
		return fmt.Errorf("copy kernel: %w", err)
	}
	if err := os.Chown(kernelPath, s.UID, s.UID); err != nil {
		return fmt.Errorf("chown kernel: %w", err)
	}
	if err := os.Chmod(kernelPath, 0o400); err != nil {
		return fmt.Errorf("chmod kernel: %w", err)
	}
	if err := os.Link(s.DiskPath, filepath.Join(s.JailRoot, "rootfs.ext4")); err != nil {
		return fmt.Errorf("link disk: %w", err)
	}
	if err := os.Chown(filepath.Join(s.JailRoot, "rootfs.ext4"), s.UID, s.UID); err != nil {
		return err
	}
	quota := strconv.Itoa(s.VCPUs*100000) + " 100000"
	args := []string{
		"--id", s.SessionID, "--exec-file", d.config.FirecrackerBinary, "--uid", strconv.Itoa(s.UID), "--gid", strconv.Itoa(s.UID),
		"--chroot-base-dir", d.config.JailDir, "--netns", filepath.Join("/run/netns", s.NetNS), "--new-pid-ns",
		"--cgroup-version", "2", "--parent-cgroup", "sandbox-host",
		"--cgroup", "memory.max=" + strconv.Itoa((s.MemoryMiB+256)*1024*1024), "--cgroup", "pids.max=2048", "--cgroup", "cpu.max=" + quota,
		"--resource-limit", "no-file=4096", "--resource-limit", "fsize=" + strconv.FormatInt(64<<30, 10), "--daemonize",
	}
	if err := run(ctx, d.config.JailerBinary, args...); err != nil {
		return fmt.Errorf("start jailer: %w", err)
	}
	socket := filepath.Join(s.JailRoot, "run", "firecracker.socket")
	if err := waitForPath(ctx, socket, 10*time.Second); err != nil {
		return err
	}
	bootArgs := strings.Join([]string{
		"console=ttyS0", "reboot=k", "panic=1", "pci=off", "root=/dev/vda", "rw", "init=/usr/local/bin/sandbox-agent",
		"sandbox_ip=" + s.GuestIP, "sandbox_gw=" + s.GatewayIP, "sandbox_token=" + s.AgentToken,
		"sandbox_runtime=" + s.Runtime,
	}, " ")
	requests := []struct {
		path string
		body any
	}{
		{"/machine-config", map[string]any{"vcpu_count": s.VCPUs, "mem_size_mib": s.MemoryMiB, "smt": false, "track_dirty_pages": false}},
		{"/boot-source", map[string]any{"kernel_image_path": "/vmlinux", "boot_args": bootArgs}},
		{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": false}},
		{"/network-interfaces/eth0", map[string]any{"iface_id": "eth0", "guest_mac": guestMAC(s.Slot), "host_dev_name": "tap0"}},
		{"/actions", map[string]any{"action_type": "InstanceStart"}},
	}
	for _, request := range requests {
		if err := unixJSON(ctx, socket, http.MethodPut, request.path, request.body, nil); err != nil {
			return fmt.Errorf("firecracker %s: %w", request.path, err)
		}
	}
	return nil
}

func (d *daemon) waitForAgent(ctx context.Context, s *runtimeproto.Session, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(s.GuestIP, "1024")+"/health", nil)
		req.Header.Set("Authorization", "Bearer "+s.AgentToken)
		if response, err := client.Do(req); err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("guest agent did not become ready")
}

func (d *daemon) stopSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeID.MatchString(id) {
		httpjson.WriteError(w, 400, "invalid_session", "invalid session id")
		return
	}
	var req runtimeproto.StopSessionRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	if req.SnapshotID != "" && !safeID.MatchString(req.SnapshotID) {
		httpjson.WriteError(w, 400, "invalid_snapshot", "invalid snapshot id")
		return
	}
	d.mu.Lock()
	session := d.sessions[id]
	d.mu.Unlock()
	if session == nil {
		httpjson.WriteError(w, 404, "session_not_found", "session not found")
		return
	}
	result, err := d.stopAndSnapshot(r.Context(), session, req)
	if err != nil {
		httpjson.WriteError(w, 500, "runtime_stop_failed", err.Error())
		return
	}
	d.mu.Lock()
	delete(d.sessions, id)
	delete(d.slots, session.Slot)
	d.mu.Unlock()
	_ = os.Remove(d.sessionStatePath(id))
	httpjson.Write(w, 200, result)
}

func (d *daemon) stopAndSnapshot(ctx context.Context, s *runtimeproto.Session, req runtimeproto.StopSessionRequest) (runtimeproto.StopSessionResponse, error) {
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(shutdownCtx, http.MethodPost, "http://"+net.JoinHostPort(s.GuestIP, "1024")+"/v1/shutdown", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer "+s.AgentToken)
	request.Header.Set("Content-Type", "application/json")
	if response, err := http.DefaultClient.Do(request); err == nil {
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	time.Sleep(500 * time.Millisecond)
	if err := d.cleanupSession(ctx, s, false); err != nil {
		d.logger.Warn("session cleanup incomplete", "session", s.SessionID, "error", err)
	}
	result := runtimeproto.StopSessionResponse{}
	if req.SnapshotID != "" {
		dir := filepath.Join(d.config.DataDir, "snapshots", req.SnapshotID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return result, err
		}
		dest := filepath.Join(dir, "rootfs.ext4")
		if err := run(ctx, "cp", "--reflink=always", "--sparse=auto", "--", s.DiskPath, dest); err != nil {
			return result, err
		}
		info, err := os.Stat(dest)
		if err != nil {
			return result, err
		}
		var stat syscall.Stat_t
		if err := syscall.Stat(dest, &stat); err != nil {
			return result, err
		}
		result = runtimeproto.StopSessionResponse{SnapshotID: req.SnapshotID, SizeBytes: info.Size(), AllocatedBytes: int64(stat.Blocks) * 512}
	}
	if req.DeleteDisk || req.SnapshotID != "" {
		_ = os.RemoveAll(filepath.Dir(s.DiskPath))
	}
	return result, nil
}

func (d *daemon) cleanupSession(ctx context.Context, s *runtimeproto.Session, removeDisk bool) error {
	var errs []error
	// Removing the jail kills access to the API socket only after the cgroup has been killed.
	cgroup := filepath.Join("/sys/fs/cgroup/sandbox-host", s.SessionID)
	if err := os.WriteFile(filepath.Join(cgroup, "cgroup.kill"), []byte("1"), 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := run(ctx, "nft", "delete", "element", "inet", "sandbox_host", "guest_ifaces", "{", s.HostVeth, "}"); err != nil {
		errs = append(errs, err)
	}
	_ = unixJSON(ctx, d.config.EgressSocket, http.MethodDelete, "/v1/sessions/"+s.SessionID, nil, nil)
	if err := run(ctx, "ip", "link", "delete", s.HostVeth); err != nil {
		errs = append(errs, err)
	}
	if err := run(ctx, "ip", "netns", "delete", s.NetNS); err != nil {
		errs = append(errs, err)
	}
	_ = os.RemoveAll(filepath.Dir(s.JailRoot))
	_ = os.RemoveAll(cgroup)
	if removeDisk {
		_ = os.RemoveAll(filepath.Dir(s.DiskPath))
	}
	return errors.Join(errs...)
}

func (d *daemon) updatePolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req runtimeproto.UpdatePolicyRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	d.mu.Lock()
	s := d.sessions[id]
	d.mu.Unlock()
	if s == nil {
		httpjson.WriteError(w, 404, "session_not_found", "session not found")
		return
	}
	if err := d.setEgressPolicy(r.Context(), s, req.NetworkPolicy); err != nil {
		httpjson.WriteError(w, 403, "policy_update_failed", err.Error())
		return
	}
	s.NetworkPolicy = req.NetworkPolicy
	if err := d.saveSession(s); err != nil {
		httpjson.WriteError(w, 500, "state_persist_failed", err.Error())
		return
	}
	httpjson.Write(w, 200, struct{}{})
}

func (d *daemon) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeID.MatchString(id) {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_snapshot", "invalid snapshot id")
		return
	}
	path := filepath.Join(d.config.DataDir, "snapshots", id)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		httpjson.WriteError(w, http.StatusNotFound, "snapshot_not_found", "snapshot not found")
		return
	}
	if err := os.RemoveAll(path); err != nil {
		httpjson.WriteError(w, http.StatusInternalServerError, "snapshot_delete_failed", err.Error())
		return
	}
	httpjson.Write(w, http.StatusOK, struct{}{})
}

func (d *daemon) saveSession(s *runtimeproto.Session) error {
	path := d.sessionStatePath(s.SessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (d *daemon) sessionStatePath(id string) string {
	return filepath.Join(d.config.DataDir, "runtime", "sessions", id+".json")
}

func (d *daemon) reconcileStartup() error {
	dir := filepath.Join(d.config.DataDir, "runtime", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		var s runtimeproto.Session
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if d.sessionAlive(&s) {
			if _, exists := d.slots[s.Slot]; exists {
				return fmt.Errorf("duplicate persisted slot %d", s.Slot)
			}
			d.sessions[s.SessionID] = &s
			d.slots[s.Slot] = s.SessionID
			if len(s.NetworkPolicy.Rules) > 0 {
				if err := d.setEgressPolicy(context.Background(), &s, s.NetworkPolicy); err != nil {
					return fmt.Errorf("restore egress policy for %s: %w", s.SessionID, err)
				}
			}
			d.logger.Info("session_adopted", "session", s.SessionID, "guestIp", s.GuestIP)
			continue
		}
		_ = d.cleanupSession(context.Background(), &s, false)
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
	return nil
}

func (d *daemon) sessionAlive(s *runtimeproto.Session) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "http://"+net.JoinHostPort(s.GuestIP, "1024")+"/health", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+s.AgentToken)
	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode == http.StatusOK
}

func unixJSON(ctx context.Context, socket, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("%s %s returned %d: %s", method, path, response.StatusCode, payload)
	}
	if out != nil {
		return json.NewDecoder(response.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

func waitForPath(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func guestMAC(slot int) string {
	return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", (slot>>24)&255, (slot>>16)&255, (slot>>8)&255, slot&255)
}
func shortID(id string) string {
	clean := strings.ToLower(strings.ReplaceAll(id, "-", ""))
	if len(clean) > 11 {
		clean = clean[:11]
	}
	return clean
}
func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func memInfo() (int64, int64) {
	b, _ := os.ReadFile("/proc/meminfo")
	var total, available int64
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, _ := strconv.ParseInt(fields[1], 10, 64)
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = value / 1024
		case "MemAvailable":
			available = value / 1024
		}
	}
	return total, available
}
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
