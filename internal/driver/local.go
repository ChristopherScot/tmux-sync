package driver

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// Local runs commands on the local host (no container indirection). It's the
// laptop-side counterpart to K8s / DockerExec — for operations that need to
// touch laptop files directly (e.g. bundling laptop-side repos on checkin via
// a script run against the laptop filesystem).
type Local struct{}

// Exec implements Driver.
func (Local) Exec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		return fmt.Errorf("local exec: argv is empty")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// String implements Driver.
func (Local) String() string { return "local" }

// Close implements Driver.
func (Local) Close() error { return nil }
