// Package driver defines the backend-agnostic primitive (Exec) that the rest of
// tmux-sync builds on. Each implementation is a thin adapter around the
// transport for one backend: kubectl exec, ssh+docker exec, local docker exec.
//
// tar-over-Exec moves files; scripted Exec drives tmux/git/nvim remotely. Once
// callers express their needs as "run argv inside the container, stream
// stdin/stdout", everything else is shared code.
package driver

import (
	"context"
	"io"
)

// Driver runs commands inside the target container with stream wiring.
type Driver interface {
	// Exec runs argv inside the target container. argv[0] is the program; the
	// remainder are its arguments. The streams are wired straight to the
	// remote process. Errors include context cancellation and non-zero exits.
	Exec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error

	// String returns a short human-readable description, used in error
	// messages and logs, e.g. "k8s:claude-pods/claude-session-0".
	String() string

	// Close releases any cached resources (typically a no-op).
	Close() error
}
