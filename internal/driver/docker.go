package driver

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// DockerExec runs commands inside a local docker container via `docker exec`.
// Symmetric to K8s — same primitive, different transport — used on checkin
// to drive the nvim-flush script (and anything else that has to reach the
// running container) the same way K8s drives them in the pod on checkout.
type DockerExec struct {
	// Container is the local container name (matches what
	// reconstruct.LaunchAndRestore created: "tmux-sync-<endpoint>").
	Container string
}

// NewDockerExec returns a validated DockerExec driver.
func NewDockerExec(container string) (*DockerExec, error) {
	if container == "" {
		return nil, fmt.Errorf("docker driver: container is required")
	}
	return &DockerExec{Container: container}, nil
}

// Exec implements Driver. Unlike kubectl, `docker exec` doesn't take a `--`
// separator before the user command — argv is passed straight through.
func (d *DockerExec) Exec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		return fmt.Errorf("docker exec: argv is empty")
	}
	args := append([]string{"exec", "-i", d.Container}, argv...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// String implements Driver.
func (d *DockerExec) String() string { return "docker:" + d.Container }

// Close implements Driver.
func (d *DockerExec) Close() error { return nil }
