//go:build linux

package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ai-club/sandbox-host/internal/guestproto"
	"github.com/ai-club/sandbox-host/internal/httpjson"
)

const (
	defaultCwd       = "/vercel/sandbox"
	commandLogDir    = "/var/log/sandbox-agent/commands"
	maxCommandLog    = int64(32 << 20)
	maxFileUpload    = int64(256 << 20)
	defaultListen    = ":1024"
	commandChunkSize = 32 << 10
)

type commandState struct {
	mu        sync.Mutex
	meta      guestproto.Command
	cmd       *exec.Cmd
	done      chan struct{}
	logPath   string
	logFile   *os.File
	logBytes  int64
	logWG     sync.WaitGroup
	truncated bool
}

type agent struct {
	token     string
	startedAt time.Time
	mu        sync.RWMutex
	commands  map[string]*commandState
}

func main() {
	if os.Getpid() == 1 {
		mountPseudoFilesystems()
		go reapChildren()
	}
	config := parseKernelCommandLine()
	if runtimeName := config["sandbox_runtime"]; runtimeName != "" {
		runtimePath := "/vercel/runtimes/" + runtimeName + "/bin"
		if runtimeName == "python3.13" {
			runtimePath = "/vercel/runtimes/python/bin"
		}
		_ = os.Setenv("PATH", runtimePath+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if err := configureNetwork(config); err != nil {
		log.Fatalf("configure network: %v", err)
	}
	token := os.Getenv("SANDBOX_AGENT_TOKEN")
	if token == "" {
		token = config["sandbox_token"]
	}
	if len(token) < 20 {
		log.Fatal("sandbox agent token is missing or too short")
	}
	if err := os.MkdirAll(defaultCwd, 0o755); err != nil {
		log.Fatal(err)
	}
	_ = os.Chown(defaultCwd, 1000, 1000)
	if err := os.MkdirAll(commandLogDir, 0o700); err != nil {
		log.Fatal(err)
	}
	a := &agent{token: token, startedAt: time.Now().UTC(), commands: make(map[string]*commandState)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.auth(a.health))
	mux.HandleFunc("POST /v1/commands", a.auth(a.startCommand))
	mux.HandleFunc("GET /v1/commands/{id}", a.auth(a.getCommand))
	mux.HandleFunc("GET /v1/commands/{id}/logs", a.auth(a.commandLogs))
	mux.HandleFunc("POST /v1/commands/{id}/kill", a.auth(a.killCommand))
	mux.HandleFunc("POST /v1/fs/mkdir", a.auth(a.mkdir))
	mux.HandleFunc("POST /v1/fs/read", a.auth(a.readFile))
	mux.HandleFunc("POST /v1/fs/write", a.auth(a.writeFiles))
	mux.HandleFunc("POST /v1/shutdown", a.auth(a.shutdown))

	server := &http.Server{
		Addr:              envOr("SANDBOX_AGENT_LISTEN", defaultListen),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	log.Printf("guest agent listening on %s", server.Addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (a *agent) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(a.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) != 1 {
			httpjson.WriteError(w, http.StatusUnauthorized, "unauthorized", "invalid agent token")
			return
		}
		next(w, r)
	}
}

func (a *agent) health(w http.ResponseWriter, _ *http.Request) {
	httpjson.Write(w, http.StatusOK, guestproto.Health{Status: "ok", StartedAt: a.startedAt})
}

func (a *agent) startCommand(w http.ResponseWriter, r *http.Request) {
	var req guestproto.StartCommandRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.ID == "" || len(req.ID) > 128 || req.Command == "" || strings.ContainsRune(req.ID, '/') {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_command", "id and command are required")
		return
	}
	if len(req.Args) > 4096 || len(req.Env) > 256 {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_command", "command arguments or environment exceed limits")
		return
	}
	a.mu.Lock()
	if len(a.commands) >= 256 {
		a.mu.Unlock()
		httpjson.WriteError(w, http.StatusTooManyRequests, "command_limit", "session command limit reached")
		return
	}
	if _, exists := a.commands[req.ID]; exists {
		a.mu.Unlock()
		httpjson.WriteError(w, http.StatusConflict, "command_exists", "command id already exists")
		return
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = defaultCwd
	}
	logPath := filepath.Join(commandLogDir, req.ID+".ndjson")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		a.mu.Unlock()
		httpjson.WriteError(w, http.StatusInternalServerError, "command_log_failed", err.Error())
		return
	}
	cmd := exec.Command(req.Command, req.Args...)
	cmd.Dir = cwd
	cmd.Env = commandEnvironment(req.Env, req.Sudo)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if !req.Sudo {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000}}
	}
	state := &commandState{
		meta: guestproto.Command{ID: req.ID, Name: req.Command, Args: append([]string(nil), req.Args...), Cwd: cwd, StartedAt: time.Now().UnixMilli()},
		cmd:  cmd, done: make(chan struct{}), logPath: logPath, logFile: logFile,
	}
	a.commands[req.ID] = state
	a.mu.Unlock()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		a.failStart(state, err)
		httpjson.WriteError(w, http.StatusInternalServerError, "command_start_failed", err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		a.failStart(state, err)
		httpjson.WriteError(w, http.StatusInternalServerError, "command_start_failed", err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		a.failStart(state, err)
		httpjson.WriteError(w, http.StatusUnprocessableEntity, "command_start_failed", err.Error())
		return
	}
	state.logWG.Add(2)
	go state.copyLogs("stdout", stdout)
	go state.copyLogs("stderr", stderr)
	go state.wait()
	httpjson.Write(w, http.StatusCreated, guestproto.CommandResponse{Command: state.snapshot()})
}

func (a *agent) failStart(state *commandState, err error) {
	exit := 127
	state.mu.Lock()
	state.meta.ExitCode = &exit
	_ = state.appendLogLocked(guestproto.LogRecord{Stream: "stderr", Data: err.Error() + "\n"})
	_ = state.logFile.Close()
	state.mu.Unlock()
	close(state.done)
}

func (s *commandState) copyLogs(stream string, src io.Reader) {
	defer s.logWG.Done()
	buf := make([]byte, commandChunkSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			s.mu.Lock()
			_ = s.appendLogLocked(guestproto.LogRecord{Stream: stream, Data: strings.ToValidUTF8(string(buf[:n]), "\uFFFD")})
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *commandState) appendLogLocked(record guestproto.LogRecord) error {
	if s.logBytes >= maxCommandLog {
		return nil
	}
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	remaining := maxCommandLog - s.logBytes
	if int64(len(b)) > remaining {
		if s.truncated {
			return nil
		}
		s.truncated = true
		b, err = json.Marshal(guestproto.LogRecord{Stream: "stderr", Data: "\n[sandbox-host: command output truncated]\n"})
		if err != nil {
			return err
		}
		b = append(b, '\n')
		if int64(len(b)) > remaining {
			return nil
		}
	}
	n, err := s.logFile.Write(b)
	s.logBytes += int64(n)
	return err
}

func (s *commandState) wait() {
	err := s.cmd.Wait()
	s.logWG.Wait()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = 126
		}
	}
	s.mu.Lock()
	s.meta.ExitCode = &exit
	_ = s.logFile.Sync()
	_ = s.logFile.Close()
	s.mu.Unlock()
	close(s.done)
}

func (s *commandState) snapshot() guestproto.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.meta
	m.Args = append([]string(nil), s.meta.Args...)
	if s.meta.ExitCode != nil {
		e := *s.meta.ExitCode
		m.ExitCode = &e
	}
	return m
}

func (a *agent) lookupCommand(w http.ResponseWriter, id string) *commandState {
	a.mu.RLock()
	state := a.commands[id]
	a.mu.RUnlock()
	if state == nil {
		httpjson.WriteError(w, http.StatusNotFound, "command_not_found", "command not found")
	}
	return state
}

func (a *agent) getCommand(w http.ResponseWriter, r *http.Request) {
	state := a.lookupCommand(w, r.PathValue("id"))
	if state == nil {
		return
	}
	if r.URL.Query().Get("wait") == "true" {
		select {
		case <-state.done:
		case <-r.Context().Done():
			return
		}
	}
	httpjson.Write(w, http.StatusOK, guestproto.CommandResponse{Command: state.snapshot()})
}

func (a *agent) commandLogs(w http.ResponseWriter, r *http.Request) {
	state := a.lookupCommand(w, r.PathValue("id"))
	if state == nil {
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)
	var offset int64
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		f, err := os.Open(state.logPath)
		if err != nil {
			return
		}
		_, _ = f.Seek(offset, io.SeekStart)
		n, _ := io.Copy(w, f)
		offset += n
		_ = f.Close()
		if flusher != nil && n > 0 {
			flusher.Flush()
		}
		select {
		case <-state.done:
			f, err := os.Open(state.logPath)
			if err == nil {
				_, _ = f.Seek(offset, io.SeekStart)
				_, _ = io.Copy(w, f)
				_ = f.Close()
			}
			return
		default:
		}
		select {
		case <-ticker.C:
		case <-r.Context().Done():
			return
		}
	}
}

func (a *agent) killCommand(w http.ResponseWriter, r *http.Request) {
	state := a.lookupCommand(w, r.PathValue("id"))
	if state == nil {
		return
	}
	var req guestproto.KillRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Signal <= 0 || req.Signal > 64 {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_signal", "signal must be between 1 and 64")
		return
	}
	state.mu.Lock()
	pid := 0
	if state.cmd.Process != nil && state.meta.ExitCode == nil {
		pid = state.cmd.Process.Pid
	}
	state.mu.Unlock()
	if pid != 0 {
		if err := syscall.Kill(-pid, syscall.Signal(req.Signal)); err != nil && !errors.Is(err, syscall.ESRCH) {
			httpjson.WriteError(w, http.StatusInternalServerError, "signal_failed", err.Error())
			return
		}
	}
	httpjson.Write(w, http.StatusOK, guestproto.CommandResponse{Command: state.snapshot()})
}

func (a *agent) mkdir(w http.ResponseWriter, r *http.Request) {
	var req guestproto.MkdirRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	p, err := resolveGuestPath(req.Cwd, req.Path)
	if err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		httpjson.WriteError(w, statusForFSError(err), "mkdir_failed", err.Error())
		return
	}
	httpjson.Write(w, http.StatusOK, struct{}{})
}

func (a *agent) readFile(w http.ResponseWriter, r *http.Request) {
	var req guestproto.ReadFileRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	p, err := resolveGuestPath(req.Cwd, req.Path)
	if err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		httpjson.WriteError(w, http.StatusNotFound, "file_not_found", "file not found")
		return
	}
	if err != nil {
		httpjson.WriteError(w, http.StatusForbidden, "read_failed", err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		httpjson.WriteError(w, http.StatusBadRequest, "not_regular_file", "path is not a regular file")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, f)
}

func (a *agent) writeFiles(w http.ResponseWriter, r *http.Request) {
	base := r.Header.Get("X-Cwd")
	if base == "" {
		base = defaultCwd
	}
	base, err := filepath.Abs(base)
	if err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_path", err.Error())
		return
	}
	limited := http.MaxBytesReader(w, r.Body, maxFileUpload)
	gz, err := gzip.NewReader(limited)
	if err != nil {
		httpjson.WriteError(w, http.StatusBadRequest, "invalid_archive", err.Error())
		return
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			httpjson.WriteError(w, http.StatusBadRequest, "invalid_archive", err.Error())
			return
		}
		target, err := safeArchiveTarget(base, h.Name)
		if err != nil {
			httpjson.WriteError(w, http.StatusBadRequest, "invalid_archive_path", err.Error())
			return
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(h.Mode)&0o777); err != nil {
				httpjson.WriteError(w, http.StatusInternalServerError, "write_failed", err.Error())
				return
			}
			if err := os.Chown(target, 1000, 1000); err != nil {
				httpjson.WriteError(w, http.StatusInternalServerError, "write_failed", err.Error())
				return
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				httpjson.WriteError(w, http.StatusInternalServerError, "write_failed", err.Error())
				return
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, os.FileMode(h.Mode)&0o777)
			if err != nil {
				httpjson.WriteError(w, http.StatusForbidden, "write_failed", err.Error())
				return
			}
			if err := f.Chown(1000, 1000); err != nil {
				_ = f.Close()
				httpjson.WriteError(w, http.StatusInternalServerError, "write_failed", err.Error())
				return
			}
			_, copyErr := io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if copyErr != nil || closeErr != nil {
				httpjson.WriteError(w, http.StatusInternalServerError, "write_failed", errors.Join(copyErr, closeErr).Error())
				return
			}
		default:
			httpjson.WriteError(w, http.StatusBadRequest, "unsupported_archive_entry", "only regular files and directories are accepted")
			return
		}
	}
	httpjson.Write(w, http.StatusOK, struct{}{})
}

func (a *agent) shutdown(w http.ResponseWriter, _ *http.Request) {
	httpjson.Write(w, http.StatusAccepted, struct{}{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		syscall.Sync()
		_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	}()
}

func resolveGuestPath(cwd, p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	if cwd == "" {
		cwd = defaultCwd
	}
	if !filepath.IsAbs(cwd) {
		return "", errors.New("cwd must be absolute")
	}
	return filepath.Join(cwd, p), nil
}

func safeArchiveTarget(base, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", errors.New("archive path must be non-empty and relative")
	}
	target := filepath.Join(base, filepath.Clean(name))
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("archive path escapes extraction directory")
	}
	return target, nil
}

func commandEnvironment(extra map[string]string, sudo bool) []string {
	home := "/home/vercel-sandbox"
	if sudo {
		home = "/root"
	}
	base := map[string]string{
		"HOME": home, "USER": map[bool]string{true: "root", false: "vercel-sandbox"}[sudo],
		"LOGNAME": map[bool]string{true: "root", false: "vercel-sandbox"}[sudo],
		"SHELL":   "/bin/bash", "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
		"PATH": os.Getenv("PATH"),
	}
	if base["PATH"] == "" {
		base["PATH"] = "/vercel/runtimes/node24/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	for k, v := range extra {
		if strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, '\x00') {
			continue
		}
		base[k] = v
	}
	out := make([]string, 0, len(base))
	for k, v := range base {
		out = append(out, k+"="+v)
	}
	return out
}

func parseKernelCommandLine() map[string]string {
	b, _ := os.ReadFile("/proc/cmdline")
	result := make(map[string]string)
	for _, field := range strings.Fields(string(b)) {
		k, v, ok := strings.Cut(field, "=")
		if ok {
			result[k] = v
		}
	}
	return result
}

func configureNetwork(config map[string]string) error {
	ip := config["sandbox_ip"]
	gw := config["sandbox_gw"]
	if ip == "" || gw == "" {
		return nil
	}
	commands := [][]string{
		{"link", "set", "lo", "up"},
		{"link", "set", "eth0", "up"},
		{"addr", "add", ip + "/30", "dev", "eth0"},
		{"route", "add", "default", "via", gw},
	}
	for _, args := range commands {
		if out, err := exec.Command("/sbin/ip", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, out)
		}
	}
	return os.WriteFile("/etc/resolv.conf", []byte("nameserver "+gw+"\noptions attempts:1 timeout:2\n"), 0o644)
}

func mountPseudoFilesystems() {
	for _, item := range []struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
	}{
		{"proc", "/proc", "proc", 0, ""},
		{"sysfs", "/sys", "sysfs", syscall.MS_RDONLY, ""},
		{"devtmpfs", "/dev", "devtmpfs", 0, "mode=0755"},
		{"tmpfs", "/run", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=0755,size=64m"},
		{"tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777,size=512m"},
	} {
		_ = os.MkdirAll(item.target, 0o755)
		_ = syscall.Mount(item.source, item.target, item.fstype, item.flags, item.data)
	}
}

func reapChildren() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	for range ch {
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if err != nil || pid <= 0 {
				break
			}
		}
	}
}

func statusForFSError(err error) int {
	if errors.Is(err, os.ErrNotExist) {
		return http.StatusNotFound
	}
	if errors.Is(err, os.ErrPermission) {
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
