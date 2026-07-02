package guestproto

import "time"

type StartCommandRequest struct {
	ID      string            `json:"id"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Sudo    bool              `json:"sudo,omitempty"`
}

type Command struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Args      []string `json:"args"`
	Cwd       string   `json:"cwd"`
	ExitCode  *int     `json:"exitCode"`
	StartedAt int64    `json:"startedAt"`
}

type CommandResponse struct {
	Command Command `json:"command"`
}

type KillRequest struct {
	Signal int `json:"signal"`
}

type MkdirRequest struct {
	Path string `json:"path"`
	Cwd  string `json:"cwd,omitempty"`
}

type ReadFileRequest struct {
	Path string `json:"path"`
	Cwd  string `json:"cwd,omitempty"`
}

type LogRecord struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

type Health struct {
	Status    string    `json:"status"`
	StartedAt time.Time `json:"startedAt"`
}
