package model

import "time"

const (
	RunIDEnvironment = "SANDCAMP_E2E_RUN_ID"
	NameEnvironment  = "SANDCAMP_E2E_PROCESS"
	TokenEnvironment = "SANDCAMP_E2E_TOKEN"
	ImageEnvironment = "SANDCAMP_E2E_IMAGE_ENV"
	FilesEnvironment = "SANDCAMP_E2E_EVIDENCE_FILES"
)

// Event is emitted by a fixture process and retained by the observer. Details
// are deliberately restricted to test data; arbitrary process environments
// are never copied into evidence.
type Event struct {
	Time    time.Time      `json:"time"`
	RunID   string         `json:"run_id"`
	Process string         `json:"process"`
	Kind    string         `json:"kind"`
	PID     int            `json:"pid"`
	Details map[string]any `json:"details,omitempty"`
}

// Process is a point-in-time view assembled from procfs. Namespace values are
// kernel namespace links such as "pid:[4026531836]".
type Process struct {
	Name        string                `json:"name"`
	PID         int                   `json:"pid"`
	PPID        int                   `json:"ppid"`
	UID         int                   `json:"uid"`
	GID         int                   `json:"gid"`
	Groups      []int                 `json:"groups"`
	Status      map[string]string     `json:"status"`
	Environment map[string]string     `json:"environment,omitempty"`
	Cwd         string                `json:"cwd,omitempty"`
	Executable  string                `json:"executable,omitempty"`
	Command     []string              `json:"command,omitempty"`
	Namespaces  map[string]string     `json:"namespaces,omitempty"`
	RootFSType  string                `json:"root_fs_type,omitempty"`
	Files       map[string]FileResult `json:"files,omitempty"`
}

// Snapshot is the observer's complete, bounded view of one scenario.
type Snapshot struct {
	ObservedAt time.Time `json:"observed_at"`
	RunID      string    `json:"run_id"`
	Events     []Event   `json:"events"`
	Processes  []Process `json:"processes"`
}

// FileResult records a file read through /proc/<pid>/root. This lets the
// runner compare independent sidecar roots without entering their mount
// namespaces.
type FileResult struct {
	Process string `json:"process"`
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Mode    string `json:"mode,omitempty"`
	UID     int    `json:"uid,omitempty"`
	GID     int    `json:"gid,omitempty"`
	Error   string `json:"error,omitempty"`
}

type FetchResult struct {
	URL        string            `json:"url"`
	StatusCode int               `json:"status_code,omitempty"`
	Header     map[string]string `json:"header,omitempty"`
	Body       string            `json:"body,omitempty"`
	Error      string            `json:"error,omitempty"`
}
