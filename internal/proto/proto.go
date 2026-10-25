// Package proto defines the wire format between the offload CLI and a node.
//
// A run is one HTTPS POST to /v1/run. Request metadata travels in headers and
// the body is an optional git bundle carrying the worktree snapshot. The
// response is a stream of newline-delimited JSON Events.
package proto

const (
	HeaderRepo      = "X-Offload-Repo"      // stable repo id (root commit)
	HeaderWorktree  = "X-Offload-Worktree"  // which local worktree (agent) this is
	HeaderSnapshot  = "X-Offload-Snapshot"  // snapshot commit to run against
	HeaderBase      = "X-Offload-Base"      // snapshot the bundle is relative to ("" = full)
	HeaderCmd       = "X-Offload-Cmd"       // JSON []string argv
	HeaderDir       = "X-Offload-Dir"       // cwd relative to the repo root
	HeaderEnv       = "X-Offload-Env"       // JSON map of extra env vars
	HeaderWorkspace = "X-Offload-Workspace" // response: workspace path on the node
)

// Event types.
const (
	EvOut   = "out"   // D: combined stdout/stderr chunk
	EvPhase = "phase" // Name + Ms: a timed phase on the node
	EvInfo  = "info"  // D: workspace path etc.
	EvExit  = "exit"  // Code: process exit code
	EvError = "error" // D: node-side failure (not the command's)
)

type Event struct {
	T    string `json:"t"`
	D    string `json:"d,omitempty"`
	Name string `json:"name,omitempty"`
	Ms   int64  `json:"ms,omitempty"`
	Code int    `json:"code,omitempty"`
}

// NeedFull is the 409 body when the node lacks the bundle's base snapshot.
type NeedFull struct {
	Need string `json:"need"` // "full"
}
