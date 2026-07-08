package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ai-club/sandbox-host/internal/egress"
)

type apiConfig struct {
	Listen              string        `json:"listen"`
	PreviewBindHost     string        `json:"previewBindHost"`
	PreviewExternalHost string        `json:"previewExternalHost"`
	PreviewPortStart    int           `json:"previewPortStart"`
	PreviewPortEnd      int           `json:"previewPortEnd"`
	// PreviewDomain switches preview routes to public HTTPS subdomain URLs
	// (https://<label>.<PreviewDomain>) served by the preview router, which a
	// Cloudflare tunnel fronts for TLS. Empty = legacy tailnet-only
	// http://<PreviewExternalHost>:<port> URLs.
	PreviewDomain string `json:"previewDomain"`
	// PreviewRouterListen is the plain-HTTP address the subdomain router
	// binds; the tunnel terminates TLS and proxies here (like Tailscale
	// Funnel does for the control plane). Default 127.0.0.1:8090 when
	// PreviewDomain is set.
	PreviewRouterListen string `json:"previewRouterListen"`
	// PreviewSigningSecret (≥32 chars) turns on signed-token auth for the
	// preview router: requests must carry a valid `_bft` query token or
	// `__bf_preview` cookie minted with the same secret (shared with the
	// Botflow control plane). Empty = capability-URL-only (the unguessable
	// subdomain is the credential, same model as Vercel preview URLs).
	PreviewSigningSecret string        `json:"previewSigningSecret"`
	DatabasePath         string        `json:"databasePath"`
	RuntimeSocket        string        `json:"runtimeSocket"`
	Region               string        `json:"region"`
	DefaultTimeoutMs     int64         `json:"defaultTimeoutMs"`
	MaxTimeoutMs         int64         `json:"maxTimeoutMs"`
	MaxPorts             int           `json:"maxPorts"`
	EgressCeiling        egress.Policy `json:"egressCeiling"`
	Tokens               []tokenConfig `json:"tokens"`
}

type tokenConfig struct {
	ID          string   `json:"id"`
	SHA256      string   `json:"sha256"`
	TeamID      string   `json:"teamId"`
	ProjectIDs  []string `json:"projectIds"`
	MaxSessions int      `json:"maxSessions"`
	decodedHash [32]byte
}

func loadConfig(path string) (apiConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return apiConfig{}, err
	}
	var c apiConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.PreviewBindHost == "" {
		c.PreviewBindHost = "127.0.0.1"
	}
	if c.PreviewPortStart == 0 {
		c.PreviewPortStart = 20000
	}
	if c.PreviewPortEnd == 0 {
		c.PreviewPortEnd = 40000
	}
	if c.DatabasePath == "" {
		c.DatabasePath = "/var/lib/sandbox-host/api.db"
	}
	if c.RuntimeSocket == "" {
		c.RuntimeSocket = "/run/sandbox-host/sandboxd.sock"
	}
	if c.Region == "" {
		c.Region = "local-nyc"
	}
	if c.DefaultTimeoutMs == 0 {
		c.DefaultTimeoutMs = 5 * 60 * 1000
	}
	if c.MaxTimeoutMs == 0 {
		c.MaxTimeoutMs = 45 * 60 * 1000
	}
	if c.MaxPorts == 0 {
		c.MaxPorts = 15
	}
	if !filepath.IsAbs(c.DatabasePath) || !filepath.IsAbs(c.RuntimeSocket) {
		return c, errors.New("databasePath and runtimeSocket must be absolute")
	}
	if c.PreviewExternalHost == "" {
		return c, errors.New("previewExternalHost is required")
	}
	if c.PreviewPortStart < 1024 || c.PreviewPortEnd <= c.PreviewPortStart {
		return c, errors.New("invalid preview port range")
	}
	c.PreviewDomain = strings.ToLower(strings.Trim(c.PreviewDomain, "."))
	if c.PreviewDomain != "" {
		if strings.ContainsAny(c.PreviewDomain, " \t/:") {
			return c, errors.New("previewDomain must be a bare domain name")
		}
		if c.PreviewRouterListen == "" {
			c.PreviewRouterListen = "127.0.0.1:8090"
		}
	}
	if c.PreviewSigningSecret != "" {
		if len(c.PreviewSigningSecret) < 32 {
			return c, errors.New("previewSigningSecret must be at least 32 characters")
		}
		if c.PreviewDomain == "" {
			return c, errors.New("previewSigningSecret requires previewDomain")
		}
	}
	if err := c.EgressCeiling.Validate(); err != nil {
		return c, fmt.Errorf("invalid egress ceiling: %w", err)
	}
	if len(c.Tokens) == 0 {
		return c, errors.New("at least one API token is required")
	}
	for i := range c.Tokens {
		raw, err := hex.DecodeString(c.Tokens[i].SHA256)
		if err != nil || len(raw) != sha256.Size {
			return c, fmt.Errorf("token %q has invalid sha256", c.Tokens[i].ID)
		}
		copy(c.Tokens[i].decodedHash[:], raw)
		if c.Tokens[i].TeamID == "" || len(c.Tokens[i].ProjectIDs) == 0 {
			return c, fmt.Errorf("token %q lacks team/projects", c.Tokens[i].ID)
		}
		if c.Tokens[i].MaxSessions == 0 {
			c.Tokens[i].MaxSessions = 10
		}
	}
	return c, nil
}

func (c apiConfig) authenticate(token string) (*tokenConfig, bool) {
	hash := sha256.Sum256([]byte(token))
	found := -1
	for i := range c.Tokens {
		if subtle.ConstantTimeCompare(hash[:], c.Tokens[i].decodedHash[:]) == 1 {
			found = i
		}
	}
	if found < 0 {
		return nil, false
	}
	return &c.Tokens[found], true
}

func (t *tokenConfig) allowsProject(project string) bool {
	for _, candidate := range t.ProjectIDs {
		if candidate == project {
			return true
		}
	}
	return false
}
