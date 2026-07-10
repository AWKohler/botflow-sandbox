package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/ai-club/sandbox-host/internal/apimodel"
	"github.com/ai-club/sandbox-host/internal/runtimeproto"
	"github.com/ai-club/sandbox-host/internal/store"
)

type api struct {
	config    apiConfig
	store     *store.Store
	runtime   runtimeClient
	guest     *guestClient
	previews  *previewManager
	logger    *slog.Logger
	ctx       context.Context
	lifecycle sync.Mutex

	nameMu    sync.Mutex
	nameLocks map[string]*sync.Mutex
}

// lockName returns a function that releases a per-(team,project,name) lock,
// serializing lifecycle transitions (create/resume) for a single sandbox so
// concurrent requests cannot each spawn a runtime VM for the same name.
func (a *api) lockName(team, project, name string) func() {
	key := team + "\x00" + project + "\x00" + name
	a.nameMu.Lock()
	if a.nameLocks == nil {
		a.nameLocks = make(map[string]*sync.Mutex)
	}
	m, ok := a.nameLocks[key]
	if !ok {
		m = &sync.Mutex{}
		a.nameLocks[key] = m
	}
	a.nameMu.Unlock()
	m.Lock()
	return m.Unlock
}

func main() {
	configPath := envOr("SANDBOX_API_CONFIG", "/etc/sandbox-host/api.json")
	cfg, err := loadConfig(configPath)
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o700); err != nil {
		panic(err)
	}
	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		panic(err)
	}
	defer db.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	a := &api{config: cfg, store: db, runtime: runtimeClient{socket: cfg.RuntimeSocket}, guest: newGuestClient(), logger: logger, ctx: ctx}
	a.previews = newPreviewManager(cfg, logger)
	a.reconcileActiveSessions(ctx)
	go a.timeoutLoop(ctx)
	go a.gcLoop(ctx)
	server := &http.Server{Addr: cfg.Listen, Handler: a.routes(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if cfg.PreviewDomain != "" {
		// Subdomain preview router: plain HTTP on localhost, fronted for TLS
		// by the Cloudflare tunnel (see docs/preview-tunnel.md).
		router := &http.Server{Addr: cfg.PreviewRouterListen, Handler: a.previewRouterHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = router.Shutdown(shutdownCtx)
		}()
		go func() {
			logger.Info("preview_router_listening", "address", cfg.PreviewRouterListen, "domain", cfg.PreviewDomain, "signedTokens", cfg.PreviewSigningSecret != "")
			if err := router.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				panic(err)
			}
		}()
	}
	logger.Info("api_listening", "address", cfg.Listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		panic(err)
	}
}

func (a *api) reconcileActiveSessions(ctx context.Context) {
	// Ask the runtime which VMs actually exist. Any DB record marked running
	// whose VM is gone (e.g. after a host reboot) is transitioned to stopped so
	// quota slots are freed and callers see accurate state; if it has a snapshot
	// it remains resumable.
	live, liveErr := a.runtime.sessions(ctx)
	if liveErr != nil {
		a.logger.Error("startup runtime session listing failed; skipping liveness prune", "error", liveErr)
	}
	seen := make(map[string]bool)
	for _, token := range a.config.Tokens {
		for _, project := range token.ProjectIDs {
			key := token.TeamID + "\x00" + project
			if seen[key] {
				continue
			}
			seen[key] = true
			records, err := a.store.ListSandboxes(token.TeamID, project)
			if err != nil {
				a.logger.Error("startup sandbox listing failed", "team", token.TeamID, "project", project, "error", err)
				continue
			}
			for _, record := range records {
				if record.Sandbox.Status != "running" {
					continue
				}
				if liveErr == nil && !live[record.Session.ID] {
					now := time.Now().UnixMilli()
					a.previews.close(record.Routes)
					record.Routes = []apimodel.Route{}
					record.Session.Status = "stopped"
					record.Session.StoppedAt = now
					record.Session.UpdatedAt = now
					record.Sandbox.Status = "stopped"
					record.Sandbox.StatusUpdatedAt = now
					record.Sandbox.UpdatedAt = now
					record.RuntimeGuestIP = ""
					record.RuntimeToken = ""
					if err := a.store.PutSandbox(record); err != nil {
						a.logger.Error("startup stale session prune failed", "session", record.Session.ID, "error", err)
					} else {
						a.logger.Info("startup pruned dead session", "session", record.Session.ID, "name", record.Sandbox.Name)
					}
					continue
				}
				if err := a.runtime.updatePolicy(ctx, record.Session.ID, runtimeproto.UpdatePolicyRequest{NetworkPolicy: record.EffectivePolicy}); err != nil {
					a.logger.Error("startup session reconciliation failed", "session", record.Session.ID, "error", err)
					continue
				}
				record.Routes = make([]apimodel.Route, 0, len(record.Ports))
				for _, port := range record.Ports {
					route, err := a.previews.open(a.ctx, record.Session.ID, record.RuntimeGuestIP, port)
					if err != nil {
						a.logger.Error("startup preview reconciliation failed", "session", record.Session.ID, "port", port, "error", err)
						continue
					}
					record.Routes = append(record.Routes, route)
				}
				if err := a.store.PutSandbox(record); err != nil {
					a.logger.Error("startup sandbox persistence failed", "session", record.Session.ID, "error", err)
				}
			}
		}
	}
}

func (a *api) timeoutLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.stopExpiredSessions()
		}
	}
}

func (a *api) stopExpiredSessions() {
	now := time.Now().UnixMilli()
	for _, token := range a.config.Tokens {
		for _, project := range token.ProjectIDs {
			records, err := a.store.ListSandboxes(token.TeamID, project)
			if err != nil {
				continue
			}
			for _, record := range records {
				if record.Sandbox.Status == "running" && record.Session.StartedAt+record.Session.Timeout <= now {
					if _, _, err := a.stopRecord(context.Background(), record, record.Sandbox.Persistent, "automatic"); err != nil {
						a.logger.Error("timeout stop failed", "session", record.Session.ID, "error", err)
					}
				}
			}
		}
	}
}

// gcLoop periodically enforces snapshot expiration and per-sandbox retention so
// stopped sandboxes and their snapshots cannot silently accumulate and fill the
// backing store. It complements sandboxd's free-space admission ceiling.
func (a *api) gcLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	a.collectGarbage(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.collectGarbage(ctx)
		}
	}
}

func (a *api) collectGarbage(ctx context.Context) {
	now := time.Now().UnixMilli()
	seen := make(map[string]bool)
	for _, token := range a.config.Tokens {
		for _, project := range token.ProjectIDs {
			key := token.TeamID + "\x00" + project
			if seen[key] {
				continue
			}
			seen[key] = true
			records, err := a.store.ListSandboxes(token.TeamID, project)
			if err != nil {
				continue
			}
			for _, rec := range records {
				snaps, err := a.store.ListSnapshots(token.TeamID, project, rec.Sandbox.Name)
				if err != nil {
					continue
				}
				sort.Slice(snaps, func(i, j int) bool {
					return snaps[i].Snapshot.CreatedAt > snaps[j].Snapshot.CreatedAt
				})
				retention := 0
				if rec.Sandbox.KeepLastSnapshots != nil {
					retention = rec.Sandbox.KeepLastSnapshots.Count
				}
				kept := 0
				for _, sr := range snaps {
					s := sr.Snapshot
					// Never delete the sandbox's current resume point, even if
					// it is expired: that would strand the sandbox.
					if s.ID == rec.Sandbox.CurrentSnapshotID {
						kept++
						continue
					}
					expired := s.ExpiresAt > 0 && s.ExpiresAt <= now
					overRetention := retention > 0 && kept >= retention
					if expired || overRetention {
						if err := a.runtime.deleteSnapshot(ctx, s.ID); err != nil {
							a.logger.Warn("gc snapshot runtime delete failed", "snapshot", s.ID, "error", err)
							continue
						}
						if err := a.store.DeleteSnapshot(s.ID); err != nil {
							a.logger.Warn("gc snapshot record delete failed", "snapshot", s.ID, "error", err)
							continue
						}
						a.logger.Info("gc deleted snapshot", "snapshot", s.ID, "name", rec.Sandbox.Name, "expired", expired, "overRetention", overRetention)
						continue
					}
					kept++
				}
			}
		}
	}
}

func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /api/v2/sandboxes", a.auth(a.createSandbox))
	mux.HandleFunc("GET /api/v2/sandboxes", a.auth(a.listSandboxes))
	mux.HandleFunc("GET /api/v2/sandboxes/{name}", a.auth(a.getSandbox))
	mux.HandleFunc("PATCH /api/v2/sandboxes/{name}", a.auth(a.updateSandbox))
	mux.HandleFunc("DELETE /api/v2/sandboxes/{name}", a.auth(a.deleteSandbox))
	mux.HandleFunc("GET /api/v2/sandboxes/sessions", a.auth(a.listSessions))
	mux.HandleFunc("GET /api/v2/sandboxes/sessions/{id}", a.auth(a.getSession))
	mux.HandleFunc("POST /api/v2/sandboxes/sessions/{id}/cmd", a.auth(a.runCommand))
	mux.HandleFunc("GET /api/v2/sandboxes/sessions/{id}/cmd/{cmd}", a.auth(a.getCommand))
	mux.HandleFunc("GET /api/v2/sandboxes/sessions/{id}/cmd/{cmd}/logs", a.auth(a.commandLogs))
	mux.HandleFunc("POST /api/v2/sandboxes/sessions/{id}/cmd/{cmd}/kill", a.auth(a.killCommand))
	mux.HandleFunc("POST /api/v2/sandboxes/sessions/{id}/fs/mkdir", a.auth(a.mkdir))
	mux.HandleFunc("POST /api/v2/sandboxes/sessions/{id}/fs/read", a.auth(a.readFile))
	mux.HandleFunc("POST /api/v2/sandboxes/sessions/{id}/fs/write", a.auth(a.writeFiles))
	mux.HandleFunc("POST /api/v2/sandboxes/sessions/{id}/interactive", a.auth(a.interactive))
	mux.HandleFunc("POST /api/v2/sandboxes/sessions/{id}/network-policy", a.auth(a.updateSessionPolicy))
	mux.HandleFunc("POST /api/v2/sandboxes/sessions/{id}/extend-timeout", a.auth(a.extendTimeout))
	mux.HandleFunc("POST /api/v2/sandboxes/sessions/{id}/snapshot", a.auth(a.snapshotSession))
	mux.HandleFunc("POST /api/v2/sandboxes/sessions/{id}/stop", a.auth(a.stopSession))
	mux.HandleFunc("GET /api/v2/sandboxes/snapshots", a.auth(a.listSnapshots))
	mux.HandleFunc("GET /api/v2/sandboxes/snapshots/tree", a.auth(a.snapshotTree))
	mux.HandleFunc("GET /api/v2/sandboxes/snapshots/{id}", a.auth(a.getSnapshot))
	mux.HandleFunc("DELETE /api/v2/sandboxes/snapshots/{id}", a.auth(a.deleteSnapshot))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
