package apimodel

import (
	"encoding/json"

	"github.com/ai-club/sandbox-host/internal/egress"
)

type Sandbox struct {
	Name                     string            `json:"name"`
	Persistent               bool              `json:"persistent"`
	Region                   string            `json:"region,omitempty"`
	VCPUs                    int               `json:"vcpus,omitempty"`
	Memory                   int               `json:"memory,omitempty"`
	Runtime                  string            `json:"runtime,omitempty"`
	Timeout                  int64             `json:"timeout,omitempty"`
	NetworkPolicy            any               `json:"networkPolicy,omitempty"`
	TotalEgressBytes         int64             `json:"totalEgressBytes,omitempty"`
	TotalIngressBytes        int64             `json:"totalIngressBytes,omitempty"`
	TotalActiveCPUdurationMs int64             `json:"totalActiveCpuDurationMs,omitempty"`
	TotalDurationMs          int64             `json:"totalDurationMs,omitempty"`
	CreatedAt                int64             `json:"createdAt"`
	UpdatedAt                int64             `json:"updatedAt"`
	CurrentSessionID         string            `json:"currentSessionId"`
	CurrentSnapshotID        string            `json:"currentSnapshotId,omitempty"`
	Status                   string            `json:"status"`
	StatusUpdatedAt          int64             `json:"statusUpdatedAt,omitempty"`
	Cwd                      string            `json:"cwd,omitempty"`
	Tags                     map[string]string `json:"tags,omitempty"`
	SnapshotExpiration       *int64            `json:"snapshotExpiration,omitempty"`
	KeepLastSnapshots        *Retention        `json:"keepLastSnapshots,omitempty"`
}

type Session struct {
	ID                  string        `json:"id"`
	Memory              int           `json:"memory"`
	VCPUs               int           `json:"vcpus"`
	Region              string        `json:"region"`
	Runtime             string        `json:"runtime"`
	Timeout             int64         `json:"timeout"`
	Status              string        `json:"status"`
	RequestedAt         int64         `json:"requestedAt"`
	StartedAt           int64         `json:"startedAt,omitempty"`
	RequestedStopAt     int64         `json:"requestedStopAt,omitempty"`
	StoppedAt           int64         `json:"stoppedAt,omitempty"`
	AbortedAt           int64         `json:"abortedAt,omitempty"`
	Duration            int64         `json:"duration,omitempty"`
	SourceSnapshotID    string        `json:"sourceSnapshotId,omitempty"`
	SnapshottedAt       int64         `json:"snapshottedAt,omitempty"`
	CreatedAt           int64         `json:"createdAt"`
	Cwd                 string        `json:"cwd"`
	UpdatedAt           int64         `json:"updatedAt"`
	NetworkPolicy       any           `json:"networkPolicy,omitempty"`
	ActiveCPUdurationMs int64         `json:"activeCpuDurationMs,omitempty"`
	NetworkTransfer     *NetworkBytes `json:"networkTransfer,omitempty"`
	GuestIP             string        `json:"-"`
	AgentToken          string        `json:"-"`
}

type Route struct {
	URL       string `json:"url"`
	Subdomain string `json:"subdomain"`
	Port      int    `json:"port"`
	HostPort  int    `json:"-"`
}

type Snapshot struct {
	ID              string `json:"id"`
	SourceSessionID string `json:"sourceSessionId"`
	Region          string `json:"region"`
	Status          string `json:"status"`
	SizeBytes       int64  `json:"sizeBytes"`
	ExpiresAt       int64  `json:"expiresAt,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	LastUsedAt      int64  `json:"lastUsedAt,omitempty"`
	CreationMethod  string `json:"creationMethod,omitempty"`
	ParentID        string `json:"parentId,omitempty"`
}

type Retention struct {
	Count         int    `json:"count"`
	Expiration    *int64 `json:"expiration,omitempty"`
	DeleteEvicted *bool  `json:"deleteEvicted,omitempty"`
}

type NetworkBytes struct {
	Ingress int64 `json:"ingress"`
	Egress  int64 `json:"egress"`
}
type Pagination struct {
	Count int     `json:"count"`
	Next  *string `json:"next"`
}

type SandboxRecord struct {
	OwnerID         string            `json:"ownerId"`
	ProjectID       string            `json:"projectId"`
	RuntimeGuestIP  string            `json:"runtimeGuestIp,omitempty"`
	RuntimeToken    string            `json:"runtimeToken,omitempty"`
	Sandbox         Sandbox           `json:"sandbox"`
	Session         Session           `json:"session"`
	Routes          []Route           `json:"routes"`
	Ports           []int             `json:"ports"`
	EffectivePolicy egress.Policy     `json:"effectivePolicy"`
	Environment     map[string]string `json:"environment,omitempty"`
}

type SnapshotRecord struct {
	OwnerID   string   `json:"ownerId"`
	ProjectID string   `json:"projectId"`
	Name      string   `json:"name"`
	Snapshot  Snapshot `json:"snapshot"`
}

type CreateRequest struct {
	ProjectID          string            `json:"projectId"`
	Name               string            `json:"name,omitempty"`
	Ports              []int             `json:"ports,omitempty"`
	Source             *Source           `json:"source,omitempty"`
	Timeout            *int64            `json:"timeout,omitempty"`
	Resources          *Resources        `json:"resources,omitempty"`
	Persistent         *bool             `json:"persistent,omitempty"`
	Runtime            string            `json:"runtime,omitempty"`
	Image              string            `json:"image,omitempty"`
	NetworkPolicy      json.RawMessage   `json:"networkPolicy,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	SnapshotExpiration *int64            `json:"snapshotExpiration,omitempty"`
	KeepLastSnapshots  *Retention        `json:"keepLastSnapshots,omitempty"`
}

type Source struct {
	Type       string `json:"type"`
	URL        string `json:"url,omitempty"`
	Depth      int    `json:"depth,omitempty"`
	Revision   string `json:"revision,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	SnapshotID string `json:"snapshotId,omitempty"`
}

type Resources struct {
	VCPUs  int `json:"vcpus"`
	Memory int `json:"memory,omitempty"`
}

type UpdateRequest struct {
	Persistent         *bool             `json:"persistent,omitempty"`
	Resources          *Resources        `json:"resources,omitempty"`
	Runtime            string            `json:"runtime,omitempty"`
	Timeout            *int64            `json:"timeout,omitempty"`
	NetworkPolicy      json.RawMessage   `json:"networkPolicy,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	Ports              []int             `json:"ports,omitempty"`
	SnapshotExpiration *int64            `json:"snapshotExpiration,omitempty"`
	KeepLastSnapshots  *Retention        `json:"keepLastSnapshots,omitempty"`
	CurrentSnapshotID  string            `json:"currentSnapshotId,omitempty"`
}
