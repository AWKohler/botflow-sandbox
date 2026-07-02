package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ai-club/sandbox-host/internal/apimodel"
	"github.com/ai-club/sandbox-host/internal/guestproto"
	"github.com/ai-club/sandbox-host/internal/httpjson"
	"github.com/ai-club/sandbox-host/internal/runtimeproto"
	"github.com/ai-club/sandbox-host/internal/store"
)

func writeJSON(w http.ResponseWriter, status int, value any) { httpjson.Write(w, status, value) }

func (a *api) createSandbox(w http.ResponseWriter, r *http.Request) {
	principal := principalFrom(r)
	var req apimodel.CreateRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	if req.ProjectID == "" {
		if len(principal.ProjectIDs) == 1 {
			req.ProjectID = principal.ProjectIDs[0]
		} else {
			httpjson.WriteError(w, 400, "project_required", "projectId is required")
			return
		}
	}
	if !principal.allowsProject(req.ProjectID) {
		httpjson.WriteError(w, 403, "project_forbidden", "token is not authorized for this project")
		return
	}
	name := req.Name
	if name == "" {
		name = randomID("sbx", 9)
	}
	if !validName(name) {
		httpjson.WriteError(w, 400, "invalid_name", "sandbox name must contain letters, numbers, or hyphens")
		return
	}
	if _, err := a.store.GetSandbox(principal.TeamID, req.ProjectID, name); err == nil {
		httpjson.WriteError(w, 409, "sandbox_exists", "sandbox name already exists")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		httpjson.WriteError(w, 500, "store_failed", err.Error())
		return
	}
	records, _ := a.store.ListSandboxes(principal.TeamID, req.ProjectID)
	running := 0
	for _, record := range records {
		if record.Sandbox.Status == "running" || record.Sandbox.Status == "pending" {
			running++
		}
	}
	if running >= principal.MaxSessions {
		httpjson.WriteError(w, 429, "concurrency_limit", "per-token concurrent sandbox limit reached")
		return
	}
	runtimeName := req.Runtime
	if runtimeName == "" {
		runtimeName = "node24"
	}
	if req.Image != "" {
		httpjson.WriteError(w, 400, "custom_images_unsupported", "custom images are not enabled")
		return
	}
	vcpus := 2
	if req.Resources != nil && req.Resources.VCPUs != 0 {
		vcpus = req.Resources.VCPUs
	}
	timeout := a.config.DefaultTimeoutMs
	if req.Timeout != nil {
		timeout = *req.Timeout
	}
	if timeout <= 0 || timeout > a.config.MaxTimeoutMs {
		httpjson.WriteError(w, 400, "invalid_timeout", "timeout exceeds deployment maximum")
		return
	}
	if len(req.Ports) > a.config.MaxPorts || !validPorts(req.Ports) {
		httpjson.WriteError(w, 400, "invalid_ports", "ports must be unique values from 1 through 65535")
		return
	}
	persistent := true
	if req.Persistent != nil {
		persistent = *req.Persistent
	}
	policy, policyWire, err := parsePolicy(req.NetworkPolicy, a.config.EgressCeiling)
	if err != nil {
		httpjson.WriteError(w, 403, "policy_exceeds_ceiling", err.Error())
		return
	}
	sourceSnapshot := ""
	if req.Source != nil {
		switch req.Source.Type {
		case "snapshot":
			sourceSnapshot = req.Source.SnapshotID
			sr, err := a.store.GetSnapshot(sourceSnapshot)
			if err != nil || sr.OwnerID != principal.TeamID || sr.ProjectID != req.ProjectID {
				httpjson.WriteError(w, 404, "snapshot_not_found", "snapshot not found")
				return
			}
		case "git":
			if req.Source.URL == "" {
				httpjson.WriteError(w, 400, "invalid_source", "git URL required")
				return
			}
		case "tarball":
			httpjson.WriteError(w, 400, "tarball_source_unsupported", "tarball sources are not enabled in the initial hardened profile")
			return
		default:
			httpjson.WriteError(w, 400, "invalid_source", "unknown source type")
			return
		}
	}
	sessionID := randomID("sess", 12)
	runtimeSession, err := a.runtime.create(r.Context(), runtimeproto.CreateSessionRequest{SessionID: sessionID, Runtime: runtimeName, VCPUs: vcpus, MemoryMiB: vcpus * 2048, SourceSnapshot: sourceSnapshot, NetworkPolicy: policy})
	if err != nil {
		httpjson.WriteError(w, 503, "runtime_create_failed", err.Error())
		return
	}
	now := time.Now().UnixMilli()
	record := apimodel.SandboxRecord{OwnerID: principal.TeamID, ProjectID: req.ProjectID, RuntimeGuestIP: runtimeSession.GuestIP, RuntimeToken: runtimeSession.AgentToken, Routes: make([]apimodel.Route, 0, len(req.Ports)), Ports: append([]int(nil), req.Ports...), EffectivePolicy: policy, Environment: req.Env}
	record.Sandbox = apimodel.Sandbox{Name: name, Persistent: persistent, Region: a.config.Region, VCPUs: vcpus, Memory: vcpus * 2048, Runtime: runtimeName, Timeout: timeout, NetworkPolicy: policyWire, CreatedAt: now, UpdatedAt: now, CurrentSessionID: sessionID, Status: "running", StatusUpdatedAt: now, Cwd: "/vercel/sandbox", Tags: req.Tags, SnapshotExpiration: req.SnapshotExpiration, KeepLastSnapshots: req.KeepLastSnapshots}
	record.Session = apimodel.Session{ID: sessionID, Memory: vcpus * 2048, VCPUs: vcpus, Region: a.config.Region, Runtime: runtimeName, Timeout: timeout, Status: "running", RequestedAt: now, StartedAt: now, CreatedAt: now, UpdatedAt: now, Cwd: "/vercel/sandbox", NetworkPolicy: policyWire, GuestIP: runtimeSession.GuestIP, AgentToken: runtimeSession.AgentToken, SourceSnapshotID: sourceSnapshot}
	for _, port := range req.Ports {
		route, err := a.previews.open(a.ctx, sessionID, runtimeSession.GuestIP, port)
		if err != nil {
			a.previews.close(record.Routes)
			_, _ = a.runtime.stop(context.Background(), sessionID, runtimeproto.StopSessionRequest{DeleteDisk: true})
			httpjson.WriteError(w, 500, "preview_create_failed", err.Error())
			return
		}
		record.Routes = append(record.Routes, route)
	}
	if req.Source != nil && req.Source.Type == "git" {
		if err := a.cloneSource(r.Context(), record, req.Source); err != nil {
			a.previews.close(record.Routes)
			_, _ = a.runtime.stop(context.Background(), sessionID, runtimeproto.StopSessionRequest{DeleteDisk: true})
			httpjson.WriteError(w, 422, "source_clone_failed", err.Error())
			return
		}
	}
	if err := a.store.PutSandbox(record); err != nil {
		a.previews.close(record.Routes)
		_, _ = a.runtime.stop(context.Background(), sessionID, runtimeproto.StopSessionRequest{DeleteDisk: true})
		httpjson.WriteError(w, 500, "store_failed", err.Error())
		return
	}
	a.logger.Info("sandbox_created", "team", principal.TeamID, "project", req.ProjectID, "name", name, "session", sessionID)
	writeJSON(w, 201, map[string]any{"sandbox": record.Sandbox, "session": record.Session, "routes": routesOrEmpty(record.Routes)})
}

func (a *api) cloneSource(ctx context.Context, record apimodel.SandboxRecord, source *apimodel.Source) error {
	if source.Username != "" || source.Password != "" {
		return errors.New("credentialed git sources require administrator credential brokering")
	}
	u, err := url.Parse(source.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("git source must be an HTTPS URL")
	}
	args := []string{"clone"}
	if source.Depth > 0 {
		args = append(args, "--depth", strconv.Itoa(source.Depth))
	}
	args = append(args, "--", source.URL, ".")
	cmd, err := a.guest.start(ctx, record.Session.GuestIP, record.Session.AgentToken, guestproto.StartCommandRequest{ID: randomID("cmd", 10), Command: "git", Args: args, Cwd: "/vercel/sandbox", Env: record.Environment})
	if err != nil {
		return err
	}
	finished, err := a.guest.get(ctx, record.Session.GuestIP, record.Session.AgentToken, cmd.ID, true)
	if err != nil {
		return err
	}
	if finished.ExitCode == nil || *finished.ExitCode != 0 {
		return errors.New("git clone exited unsuccessfully")
	}
	if source.Revision != "" {
		cmd, err = a.guest.start(ctx, record.Session.GuestIP, record.Session.AgentToken, guestproto.StartCommandRequest{ID: randomID("cmd", 10), Command: "git", Args: []string{"checkout", "--", source.Revision}, Cwd: "/vercel/sandbox", Env: record.Environment})
		if err != nil {
			return err
		}
		finished, err = a.guest.get(ctx, record.Session.GuestIP, record.Session.AgentToken, cmd.ID, true)
		if err != nil {
			return err
		}
		if finished.ExitCode == nil || *finished.ExitCode != 0 {
			return errors.New("git checkout exited unsuccessfully")
		}
	}
	return nil
}

func (a *api) listSandboxes(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	project, ok := a.projectFromQuery(w, r, p)
	if !ok {
		return
	}
	records, err := a.store.ListSandboxes(p.TeamID, project)
	if err != nil {
		httpjson.WriteError(w, 500, "store_failed", err.Error())
		return
	}
	items := make([]apimodel.Sandbox, 0, len(records))
	for _, record := range records {
		items = append(items, record.Sandbox)
	}
	writeJSON(w, 200, map[string]any{"sandboxes": items, "pagination": apimodel.Pagination{Count: len(items)}})
}

func (a *api) getSandbox(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	project, ok := a.projectFromQuery(w, r, p)
	if !ok {
		return
	}
	record, err := a.store.GetSandbox(p.TeamID, project, r.PathValue("name"))
	if err != nil {
		notFound(w, "sandbox_not_found")
		return
	}
	resume := r.URL.Query().Get("resume") != "false"
	resumed := false
	if resume && record.Sandbox.Status == "stopped" {
		record, err = a.resumeSandbox(r.Context(), record)
		if err != nil {
			httpjson.WriteError(w, 503, "sandbox_resume_failed", err.Error())
			return
		}
		resumed = true
	}
	writeJSON(w, 200, map[string]any{"sandbox": record.Sandbox, "session": record.Session, "routes": routesOrEmpty(record.Routes), "resumed": resumed})
}

func (a *api) resumeSandbox(ctx context.Context, record apimodel.SandboxRecord) (apimodel.SandboxRecord, error) {
	if record.Sandbox.CurrentSnapshotID == "" {
		return record, errors.New("sandbox has no resumable snapshot")
	}
	sessionID := randomID("sess", 12)
	rs, err := a.runtime.create(ctx, runtimeproto.CreateSessionRequest{SessionID: sessionID, Runtime: record.Sandbox.Runtime, VCPUs: record.Sandbox.VCPUs, MemoryMiB: record.Sandbox.Memory, SourceSnapshot: record.Sandbox.CurrentSnapshotID, NetworkPolicy: record.EffectivePolicy})
	if err != nil {
		return record, err
	}
	now := time.Now().UnixMilli()
	record.Routes = make([]apimodel.Route, 0, len(record.Ports))
	for _, port := range record.Ports {
		route, err := a.previews.open(a.ctx, sessionID, rs.GuestIP, port)
		if err != nil {
			a.previews.close(record.Routes)
			_, _ = a.runtime.stop(context.Background(), sessionID, runtimeproto.StopSessionRequest{DeleteDisk: true})
			return record, err
		}
		record.Routes = append(record.Routes, route)
	}
	record.Session = apimodel.Session{ID: sessionID, Memory: record.Sandbox.Memory, VCPUs: record.Sandbox.VCPUs, Region: a.config.Region, Runtime: record.Sandbox.Runtime, Timeout: record.Sandbox.Timeout, Status: "running", RequestedAt: now, StartedAt: now, CreatedAt: now, UpdatedAt: now, Cwd: "/vercel/sandbox", NetworkPolicy: record.Sandbox.NetworkPolicy, GuestIP: rs.GuestIP, AgentToken: rs.AgentToken, SourceSnapshotID: record.Sandbox.CurrentSnapshotID}
	record.RuntimeGuestIP = rs.GuestIP
	record.RuntimeToken = rs.AgentToken
	record.Sandbox.CurrentSessionID = sessionID
	record.Sandbox.Status = "running"
	record.Sandbox.StatusUpdatedAt = now
	record.Sandbox.UpdatedAt = now
	return record, a.store.PutSandbox(record)
}

func (a *api) updateSandbox(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	project, ok := a.projectFromQuery(w, r, p)
	if !ok {
		return
	}
	record, err := a.store.GetSandbox(p.TeamID, project, r.PathValue("name"))
	if err != nil {
		notFound(w, "sandbox_not_found")
		return
	}
	var req apimodel.UpdateRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	if req.Persistent != nil {
		record.Sandbox.Persistent = *req.Persistent
	}
	if req.Timeout != nil {
		if *req.Timeout <= 0 || *req.Timeout > a.config.MaxTimeoutMs {
			httpjson.WriteError(w, 400, "invalid_timeout", "timeout exceeds deployment maximum")
			return
		}
		record.Sandbox.Timeout = *req.Timeout
		record.Session.Timeout = *req.Timeout
	}
	if req.Tags != nil {
		record.Sandbox.Tags = req.Tags
	}
	if req.Resources != nil {
		vcpus := req.Resources.VCPUs
		if vcpus == 0 {
			vcpus = record.Sandbox.VCPUs
		}
		if vcpus < 1 || vcpus > 8 || vcpus != 1 && vcpus%2 != 0 || req.Resources.Memory != 0 && req.Resources.Memory != vcpus*2048 {
			httpjson.WriteError(w, 400, "invalid_resources", "vcpus must be 1 or an even value through 8; memory is fixed at 2048 MiB per vCPU")
			return
		}
		if record.Sandbox.Status == "running" && vcpus != record.Sandbox.VCPUs {
			httpjson.WriteError(w, 409, "live_resources_unsupported", "stop the sandbox before changing resources")
			return
		}
		record.Sandbox.VCPUs = vcpus
		record.Sandbox.Memory = vcpus * 2048
		record.Session.VCPUs = vcpus
		record.Session.Memory = vcpus * 2048
	}
	if req.Runtime != "" {
		if !validRuntime(req.Runtime) {
			httpjson.WriteError(w, 400, "invalid_runtime", "runtime must be node22, node24, node26, or python3.13")
			return
		}
		if record.Sandbox.Status == "running" && req.Runtime != record.Sandbox.Runtime {
			httpjson.WriteError(w, 409, "live_runtime_unsupported", "stop the sandbox before changing runtime")
			return
		}
		record.Sandbox.Runtime = req.Runtime
		record.Session.Runtime = req.Runtime
	}
	if req.Ports != nil {
		if len(req.Ports) > a.config.MaxPorts || !validPorts(req.Ports) {
			httpjson.WriteError(w, 400, "invalid_ports", "ports must be unique values from 1 through 65535")
			return
		}
		if record.Sandbox.Status == "running" {
			oldPorts := append([]int(nil), record.Ports...)
			a.previews.close(record.Routes)
			record.Routes = make([]apimodel.Route, 0, len(req.Ports))
			for _, port := range req.Ports {
				route, err := a.previews.open(a.ctx, record.Session.ID, record.RuntimeGuestIP, port)
				if err != nil {
					a.previews.close(record.Routes)
					record.Routes = make([]apimodel.Route, 0, len(oldPorts))
					for _, oldPort := range oldPorts {
						if oldRoute, rollbackErr := a.previews.open(a.ctx, record.Session.ID, record.RuntimeGuestIP, oldPort); rollbackErr == nil {
							record.Routes = append(record.Routes, oldRoute)
						}
					}
					_ = a.store.PutSandbox(record)
					httpjson.WriteError(w, 500, "preview_update_failed", err.Error())
					return
				}
				record.Routes = append(record.Routes, route)
			}
		} else {
			record.Routes = []apimodel.Route{}
		}
		record.Ports = append([]int(nil), req.Ports...)
	}
	if req.SnapshotExpiration != nil {
		if *req.SnapshotExpiration < 0 {
			httpjson.WriteError(w, 400, "invalid_snapshot_expiration", "snapshot expiration cannot be negative")
			return
		}
		record.Sandbox.SnapshotExpiration = req.SnapshotExpiration
	}
	if req.KeepLastSnapshots != nil {
		if req.KeepLastSnapshots.Count < 1 {
			httpjson.WriteError(w, 400, "invalid_snapshot_retention", "snapshot retention count must be positive")
			return
		}
		record.Sandbox.KeepLastSnapshots = req.KeepLastSnapshots
	}
	if req.CurrentSnapshotID != "" {
		snapshot, err := a.store.GetSnapshot(req.CurrentSnapshotID)
		if err != nil || snapshot.OwnerID != p.TeamID || snapshot.ProjectID != project || snapshot.Name != record.Sandbox.Name {
			httpjson.WriteError(w, 404, "snapshot_not_found", "snapshot not found")
			return
		}
		record.Sandbox.CurrentSnapshotID = req.CurrentSnapshotID
	}
	if len(req.NetworkPolicy) > 0 {
		policy, wire, err := parsePolicy(req.NetworkPolicy, a.config.EgressCeiling)
		if err != nil {
			httpjson.WriteError(w, 403, "policy_exceeds_ceiling", err.Error())
			return
		}
		if record.Sandbox.Status == "running" {
			if err := a.runtime.updatePolicy(r.Context(), record.Session.ID, runtimeproto.UpdatePolicyRequest{NetworkPolicy: policy}); err != nil {
				httpjson.WriteError(w, 500, "policy_update_failed", err.Error())
				return
			}
		}
		record.EffectivePolicy = policy
		record.Sandbox.NetworkPolicy = wire
		record.Session.NetworkPolicy = wire
	}
	record.Sandbox.UpdatedAt = time.Now().UnixMilli()
	if err := a.store.PutSandbox(record); err != nil {
		httpjson.WriteError(w, 500, "store_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"sandbox": record.Sandbox, "routes": routesOrEmpty(record.Routes)})
}

func (a *api) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	project, ok := a.projectFromQuery(w, r, p)
	if !ok {
		return
	}
	record, err := a.store.GetSandbox(p.TeamID, project, r.PathValue("name"))
	if err != nil {
		notFound(w, "sandbox_not_found")
		return
	}
	if record.Sandbox.Status == "running" {
		a.previews.close(record.Routes)
		if _, err := a.runtime.stop(r.Context(), record.Session.ID, runtimeproto.StopSessionRequest{DeleteDisk: true}); err != nil {
			httpjson.WriteError(w, 500, "runtime_stop_failed", err.Error())
			return
		}
	}
	snapshots, _ := a.store.ListSnapshots(p.TeamID, project, record.Sandbox.Name)
	for _, snapshot := range snapshots {
		_ = a.runtime.deleteSnapshot(r.Context(), snapshot.Snapshot.ID)
		_ = a.store.DeleteSnapshot(snapshot.Snapshot.ID)
	}
	if err := a.store.DeleteSandbox(p.TeamID, project, record.Sandbox.Name); err != nil {
		httpjson.WriteError(w, 500, "store_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"sandbox": record.Sandbox})
}

func (a *api) getSession(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeSession(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"session": record.Session, "routes": routesOrEmpty(record.Routes)})
}
func (a *api) listSessions(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	project, ok := a.projectFromQuery(w, r, p)
	if !ok {
		return
	}
	records, err := a.store.ListSandboxes(p.TeamID, project)
	if err != nil {
		httpjson.WriteError(w, 500, "store_failed", err.Error())
		return
	}
	name := r.URL.Query().Get("name")
	items := []apimodel.Session{}
	for _, record := range records {
		if name == "" || record.Sandbox.Name == name {
			items = append(items, record.Session)
		}
	}
	writeJSON(w, 200, map[string]any{"sessions": items, "pagination": apimodel.Pagination{Count: len(items)}})
}

func (a *api) stopSession(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeSession(w, r)
	if !ok {
		return
	}
	updated, snapshot, err := a.stopRecord(r.Context(), record, record.Sandbox.Persistent, "automatic")
	if err != nil {
		httpjson.WriteError(w, 500, "runtime_stop_failed", err.Error())
		return
	}
	response := map[string]any{"session": updated.Session, "sandbox": updated.Sandbox}
	if snapshot != nil {
		response["snapshot"] = snapshot
	}
	writeJSON(w, 200, response)
}
func (a *api) snapshotSession(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeSession(w, r)
	if !ok {
		return
	}
	var req struct {
		Expiration *int64 `json:"expiration,omitempty"`
	}
	if r.ContentLength != 0 {
		if err := httpjson.Decode(w, r, &req); err != nil {
			httpjson.WriteError(w, 400, "invalid_request", err.Error())
			return
		}
	}
	updated, snapshot, err := a.stopRecord(r.Context(), record, true, "manual")
	if err != nil {
		httpjson.WriteError(w, 500, "snapshot_failed", err.Error())
		return
	}
	if req.Expiration != nil && snapshot != nil {
		snapshot.ExpiresAt = time.Now().Add(time.Duration(*req.Expiration) * time.Millisecond).UnixMilli()
		sr, _ := a.store.GetSnapshot(snapshot.ID)
		sr.Snapshot = *snapshot
		_ = a.store.PutSnapshot(sr)
	}
	writeJSON(w, 200, map[string]any{"snapshot": snapshot, "session": updated.Session})
}

func (a *api) stopRecord(ctx context.Context, record apimodel.SandboxRecord, makeSnapshot bool, method string) (apimodel.SandboxRecord, *apimodel.Snapshot, error) {
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()
	if current, err := a.store.GetSandbox(record.OwnerID, record.ProjectID, record.Sandbox.Name); err == nil {
		record = current
	}
	if record.Sandbox.Status != "running" {
		return record, nil, nil
	}
	now := time.Now().UnixMilli()
	snapshotID := ""
	if makeSnapshot {
		snapshotID = randomID("snap", 12)
	}
	record.Sandbox.Status = "stopping"
	record.Session.Status = "stopping"
	record.Session.RequestedStopAt = now
	_ = a.store.PutSandbox(record)
	a.previews.close(record.Routes)
	result, err := a.runtime.stop(ctx, record.Session.ID, runtimeproto.StopSessionRequest{SnapshotID: snapshotID, DeleteDisk: !makeSnapshot})
	if err != nil {
		return record, nil, err
	}
	now = time.Now().UnixMilli()
	record.Routes = []apimodel.Route{}
	record.Session.Status = "stopped"
	record.Session.StoppedAt = now
	record.Session.UpdatedAt = now
	record.Session.Duration = now - record.Session.StartedAt
	record.Sandbox.Status = "stopped"
	record.Sandbox.StatusUpdatedAt = now
	record.Sandbox.UpdatedAt = now
	var snapshot *apimodel.Snapshot
	if makeSnapshot {
		expiry := int64(0)
		if record.Sandbox.SnapshotExpiration != nil && *record.Sandbox.SnapshotExpiration > 0 {
			expiry = time.Now().Add(time.Duration(*record.Sandbox.SnapshotExpiration) * time.Millisecond).UnixMilli()
		}
		snapshot = &apimodel.Snapshot{ID: snapshotID, SourceSessionID: record.Session.ID, Region: a.config.Region, Status: "created", SizeBytes: result.AllocatedBytes, ExpiresAt: expiry, CreatedAt: now, UpdatedAt: now, CreationMethod: method, ParentID: record.Sandbox.CurrentSnapshotID}
		record.Sandbox.CurrentSnapshotID = snapshotID
		if err := a.store.PutSnapshot(apimodel.SnapshotRecord{OwnerID: record.OwnerID, ProjectID: record.ProjectID, Name: record.Sandbox.Name, Snapshot: *snapshot}); err != nil {
			return record, nil, err
		}
	}
	if err := a.store.PutSandbox(record); err != nil {
		return record, nil, err
	}
	return record, snapshot, nil
}

func (a *api) runCommand(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeRunningSession(w, r)
	if !ok {
		return
	}
	var req struct {
		Command string            `json:"command"`
		Cmd     string            `json:"cmd"`
		Args    []string          `json:"args"`
		Cwd     string            `json:"cwd"`
		Env     map[string]string `json:"env"`
		Sudo    bool              `json:"sudo"`
		Wait    bool              `json:"wait"`
		Logs    bool              `json:"logs"`
		Timeout int64             `json:"timeout"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	command := req.Command
	if command == "" {
		command = req.Cmd
	}
	if command == "" {
		httpjson.WriteError(w, 400, "invalid_command", "command is required")
		return
	}
	env := mergeEnv(record.Environment, req.Env)
	cmd, err := a.guest.start(r.Context(), record.Session.GuestIP, record.Session.AgentToken, guestproto.StartCommandRequest{ID: randomID("cmd", 10), Command: command, Args: req.Args, Cwd: req.Cwd, Env: env, Sudo: req.Sudo})
	if err != nil {
		httpjson.WriteError(w, 422, "command_start_failed", err.Error())
		return
	}
	wire := wireCommand(cmd, record.Session.ID)
	if !req.Wait {
		writeJSON(w, 201, map[string]any{"command": wire})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	flusher, _ := w.(http.Flusher)
	_ = json.NewEncoder(w).Encode(map[string]any{"command": wire})
	if flusher != nil {
		flusher.Flush()
	}
	logs, err := a.guest.request(r.Context(), record.Session.GuestIP, record.Session.AgentToken, http.MethodGet, "/v1/commands/"+cmd.ID+"/logs", nil, "")
	if err == nil {
		_, _ = io.Copy(w, logs.Body)
		logs.Body.Close()
	}
	finished, err := a.guest.get(r.Context(), record.Session.GuestIP, record.Session.AgentToken, cmd.ID, true)
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"stream": "error", "data": map[string]string{"code": "command_wait_failed", "message": err.Error()}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"command": wireCommand(finished, record.Session.ID)})
}

func (a *api) getCommand(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeRunningSession(w, r)
	if !ok {
		return
	}
	cmd, err := a.guest.get(r.Context(), record.Session.GuestIP, record.Session.AgentToken, r.PathValue("cmd"), r.URL.Query().Get("wait") == "true")
	if err != nil {
		httpjson.WriteError(w, 404, "command_not_found", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"command": wireCommand(cmd, record.Session.ID)})
}
func (a *api) commandLogs(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeRunningSession(w, r)
	if !ok {
		return
	}
	resp, err := a.guest.request(r.Context(), record.Session.GuestIP, record.Session.AgentToken, http.MethodGet, "/v1/commands/"+r.PathValue("cmd")+"/logs", nil, "")
	if err != nil {
		httpjson.WriteError(w, 502, "guest_unavailable", err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
func (a *api) killCommand(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeRunningSession(w, r)
	if !ok {
		return
	}
	var req guestproto.KillRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	var out guestproto.CommandResponse
	if err := a.guest.json(r.Context(), record.Session.GuestIP, record.Session.AgentToken, http.MethodPost, "/v1/commands/"+r.PathValue("cmd")+"/kill", req, &out); err != nil {
		httpjson.WriteError(w, 502, "signal_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"command": wireCommand(out.Command, record.Session.ID)})
}

func (a *api) mkdir(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeRunningSession(w, r)
	if !ok {
		return
	}
	var req guestproto.MkdirRequest
	if err := httpjson.Decode(w, r, &req); err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	if err := a.guest.json(r.Context(), record.Session.GuestIP, record.Session.AgentToken, http.MethodPost, "/v1/fs/mkdir", req, nil); err != nil {
		httpjson.WriteError(w, 502, "mkdir_failed", err.Error())
		return
	}
	writeJSON(w, 200, struct{}{})
}
func (a *api) readFile(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeRunningSession(w, r)
	if !ok {
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	resp, err := a.guest.request(r.Context(), record.Session.GuestIP, record.Session.AgentToken, http.MethodPost, "/v1/fs/read", bytes.NewReader(b), "application/json")
	if err != nil {
		httpjson.WriteError(w, 502, "read_failed", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		httpjson.WriteError(w, 404, "file_not_found", "file not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
func (a *api) writeFiles(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeRunningSession(w, r)
	if !ok {
		return
	}
	resp, err := a.guest.requestHeaders(r.Context(), record.Session.GuestIP, record.Session.AgentToken, http.MethodPost, "/v1/fs/write", io.LimitReader(r.Body, 256<<20), "application/gzip", map[string]string{"X-Cwd": r.Header.Get("X-Cwd")})
	if err != nil {
		httpjson.WriteError(w, 502, "write_failed", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		httpjson.WriteError(w, resp.StatusCode, "write_failed", string(b))
		return
	}
	writeJSON(w, 200, struct{}{})
}

func (a *api) interactive(w http.ResponseWriter, r *http.Request) {
	_, ok := a.authorizeRunningSession(w, r)
	if !ok {
		return
	}
	httpjson.WriteError(w, 501, "interactive_not_enabled", "interactive terminal support is not enabled in this deployment")
}
func (a *api) updateSessionPolicy(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeRunningSession(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpjson.WriteError(w, 400, "invalid_request", err.Error())
		return
	}
	policy, wire, err := parsePolicy(raw, a.config.EgressCeiling)
	if err != nil {
		httpjson.WriteError(w, 403, "policy_exceeds_ceiling", err.Error())
		return
	}
	if err := a.runtime.updatePolicy(r.Context(), record.Session.ID, runtimeproto.UpdatePolicyRequest{NetworkPolicy: policy}); err != nil {
		httpjson.WriteError(w, 500, "policy_update_failed", err.Error())
		return
	}
	record.EffectivePolicy = policy
	record.Sandbox.NetworkPolicy = wire
	record.Session.NetworkPolicy = wire
	_ = a.store.PutSandbox(record)
	writeJSON(w, 200, map[string]any{"session": record.Session})
}
func (a *api) extendTimeout(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeRunningSession(w, r)
	if !ok {
		return
	}
	var req struct {
		Duration int64 `json:"duration"`
	}
	if err := httpjson.Decode(w, r, &req); err != nil || req.Duration <= 0 {
		httpjson.WriteError(w, 400, "invalid_duration", "positive duration is required")
		return
	}
	if record.Session.Timeout+req.Duration > a.config.MaxTimeoutMs {
		httpjson.WriteError(w, 400, "timeout_limit", "extended timeout exceeds deployment maximum")
		return
	}
	record.Session.Timeout += req.Duration
	record.Sandbox.Timeout = record.Session.Timeout
	record.Session.UpdatedAt = time.Now().UnixMilli()
	_ = a.store.PutSandbox(record)
	writeJSON(w, 200, map[string]any{"session": record.Session})
}

func (a *api) listSnapshots(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	project, ok := a.projectFromQuery(w, r, p)
	if !ok {
		return
	}
	records, err := a.store.ListSnapshots(p.TeamID, project, r.URL.Query().Get("name"))
	if err != nil {
		httpjson.WriteError(w, 500, "store_failed", err.Error())
		return
	}
	items := make([]apimodel.Snapshot, 0, len(records))
	for _, record := range records {
		items = append(items, record.Snapshot)
	}
	writeJSON(w, 200, map[string]any{"snapshots": items, "pagination": apimodel.Pagination{Count: len(items)}})
}
func (a *api) getSnapshot(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeSnapshot(w, r)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"snapshot": record.Snapshot})
}
func (a *api) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeSnapshot(w, r)
	if !ok {
		return
	}
	if err := a.runtime.deleteSnapshot(r.Context(), record.Snapshot.ID); err != nil {
		httpjson.WriteError(w, 500, "snapshot_delete_failed", err.Error())
		return
	}
	if err := a.store.DeleteSnapshot(record.Snapshot.ID); err != nil {
		httpjson.WriteError(w, 500, "store_failed", err.Error())
		return
	}
	record.Snapshot.Status = "deleted"
	record.Snapshot.UpdatedAt = time.Now().UnixMilli()
	writeJSON(w, 200, map[string]any{"snapshot": record.Snapshot})
}
func (a *api) snapshotTree(w http.ResponseWriter, r *http.Request) {
	record, ok := a.authorizeSnapshotID(w, r, r.URL.Query().Get("snapshotId"))
	if !ok {
		return
	}
	node := map[string]any{"snapshot": record.Snapshot, "siblings": []any{}, "count": "1"}
	writeJSON(w, 200, map[string]any{"snapshots": []any{node}, "anchor": node, "pagination": apimodel.Pagination{Count: 1}})
}

func (a *api) projectFromQuery(w http.ResponseWriter, r *http.Request, p *tokenConfig) (string, bool) {
	project := r.URL.Query().Get("projectId")
	if project == "" {
		project = r.URL.Query().Get("project")
	}
	if project == "" && len(p.ProjectIDs) == 1 {
		project = p.ProjectIDs[0]
	}
	if !p.allowsProject(project) {
		httpjson.WriteError(w, 403, "project_forbidden", "token is not authorized for project")
		return "", false
	}
	return project, true
}
func (a *api) authorizeSession(w http.ResponseWriter, r *http.Request) (apimodel.SandboxRecord, bool) {
	p := principalFrom(r)
	record, err := a.store.GetBySession(r.PathValue("id"))
	if err != nil || record.OwnerID != p.TeamID || !p.allowsProject(record.ProjectID) {
		notFound(w, "session_not_found")
		return record, false
	}
	return record, true
}
func (a *api) authorizeRunningSession(w http.ResponseWriter, r *http.Request) (apimodel.SandboxRecord, bool) {
	record, ok := a.authorizeSession(w, r)
	if !ok {
		return record, false
	}
	if record.Session.Status != "running" {
		httpjson.WriteError(w, 410, "sandbox_stopped", "sandbox session is stopped")
		return record, false
	}
	record.Session.GuestIP = record.RuntimeGuestIP
	record.Session.AgentToken = record.RuntimeToken
	if record.Session.GuestIP == "" || record.Session.AgentToken == "" {
		httpjson.WriteError(w, 503, "runtime_connection_missing", "sandbox runtime connection metadata is unavailable")
		return record, false
	}
	return record, true
}
func (a *api) authorizeSnapshot(w http.ResponseWriter, r *http.Request) (apimodel.SnapshotRecord, bool) {
	return a.authorizeSnapshotID(w, r, r.PathValue("id"))
}
func (a *api) authorizeSnapshotID(w http.ResponseWriter, r *http.Request, id string) (apimodel.SnapshotRecord, bool) {
	p := principalFrom(r)
	record, err := a.store.GetSnapshot(id)
	if err != nil || record.OwnerID != p.TeamID || !p.allowsProject(record.ProjectID) {
		notFound(w, "snapshot_not_found")
		return record, false
	}
	return record, true
}
func notFound(w http.ResponseWriter, code string) {
	httpjson.WriteError(w, 404, code, "resource not found")
}

func wireCommand(c guestproto.Command, sessionID string) map[string]any {
	return map[string]any{"id": c.ID, "name": c.Name, "args": c.Args, "cwd": c.Cwd, "sessionId": sessionID, "exitCode": c.ExitCode, "startedAt": c.StartedAt}
}
func mergeEnv(base, extra map[string]string) map[string]string {
	result := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range extra {
		result[k] = v
	}
	return result
}

func routesOrEmpty(routes []apimodel.Route) []apimodel.Route {
	if routes == nil {
		return []apimodel.Route{}
	}
	return routes
}
func randomID(prefix string, n int) string {
	if n < 16 {
		n = 16
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("read randomness: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(b)
}
func validName(name string) bool {
	if len(name) < 1 || len(name) > 64 {
		return false
	}
	for i, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') || i == 0 && r == '-' {
			return false
		}
	}
	return true
}
func validPorts(ports []int) bool {
	seen := map[int]bool{}
	for _, port := range ports {
		if port < 1 || port > 65535 || seen[port] {
			return false
		}
		seen[port] = true
	}
	return true
}

func validRuntime(runtime string) bool {
	switch runtime {
	case "node22", "node24", "node26", "python3.13":
		return true
	default:
		return false
	}
}
