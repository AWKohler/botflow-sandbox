package runtimeproto

import "github.com/ai-club/sandbox-host/internal/egress"

type CreateSessionRequest struct {
	SessionID      string        `json:"sessionId"`
	Runtime        string        `json:"runtime"`
	VCPUs          int           `json:"vcpus"`
	MemoryMiB      int           `json:"memoryMiB"`
	SourceSnapshot string        `json:"sourceSnapshot,omitempty"`
	NetworkPolicy  egress.Policy `json:"networkPolicy"`
}

type Session struct {
	SessionID     string        `json:"sessionId"`
	Runtime       string        `json:"runtime"`
	VCPUs         int           `json:"vcpus"`
	MemoryMiB     int           `json:"memoryMiB"`
	GuestIP       string        `json:"guestIp"`
	GatewayIP     string        `json:"gatewayIp"`
	AgentToken    string        `json:"agentToken"`
	Slot          int           `json:"slot"`
	UID           int           `json:"uid"`
	NetNS         string        `json:"netns"`
	HostVeth      string        `json:"hostVeth"`
	DiskPath      string        `json:"diskPath"`
	JailRoot      string        `json:"jailRoot"`
	NetworkPolicy egress.Policy `json:"networkPolicy,omitempty"`
}

type StopSessionRequest struct {
	SnapshotID string `json:"snapshotId,omitempty"`
	DeleteDisk bool   `json:"deleteDisk,omitempty"`
}

type StopSessionResponse struct {
	SnapshotID     string `json:"snapshotId,omitempty"`
	SizeBytes      int64  `json:"sizeBytes,omitempty"`
	AllocatedBytes int64  `json:"allocatedBytes,omitempty"`
}

type UpdatePolicyRequest struct {
	NetworkPolicy egress.Policy `json:"networkPolicy"`
}

type Capacity struct {
	TotalMemoryMiB     int64 `json:"totalMemoryMiB"`
	AvailableMemoryMiB int64 `json:"availableMemoryMiB"`
	ReservedMemoryMiB  int64 `json:"reservedMemoryMiB"`
	LogicalCPUs        int   `json:"logicalCpus"`
	AllocatedVCPUs     int   `json:"allocatedVcpus"`
	ActiveSessions     int   `json:"activeSessions"`
}
