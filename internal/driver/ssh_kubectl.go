package driver

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// SSHKubectl wraps `ssh <host> kubectl exec -i -n <ns> <pod> -- <argv>` — the
// SSH-hop transport the bash prototype used by default via $TMUX_SYNC_EXEC.
//
// Why this exists: many real homelab setups don't expose the cluster API
// server directly to the laptop (it's only reachable from inside the tailnet,
// from a cluster node, etc.). Users *do* have `ssh <node>` working though, and
// the cluster node has a working `kubectl`. This driver runs kubectl on the
// node, with the pod's stdin/stdout flowing back over SSH. Everything else
// (the in-container scripts, file transfer, restore) is unchanged because the
// Driver abstraction hides where Exec runs from.
type SSHKubectl struct {
	// Host is the SSH target — an alias from ~/.ssh/config or user@hostname.
	Host string
	// Context is an optional kubeconfig context *on the SSH host*; empty means
	// use whatever current-context is configured there.
	Context string
	// Namespace and Pod are required.
	Namespace string
	Pod       string
	// Container is the optional container name inside the pod (-c).
	Container string
	// SSHArgs are extra arguments inserted before the host on the ssh command
	// line — e.g. ["-o", "ConnectTimeout=8"]. The bash prototype's default
	// transport set ConnectTimeout this way.
	SSHArgs []string
}

// NewSSHKubectl returns a validated SSHKubectl driver.
func NewSSHKubectl(host, kubeContext, namespace, pod, container string, sshArgs []string) (*SSHKubectl, error) {
	if host == "" {
		return nil, fmt.Errorf("ssh-kubectl driver: host is required")
	}
	if namespace == "" {
		return nil, fmt.Errorf("ssh-kubectl driver: namespace is required")
	}
	if pod == "" {
		return nil, fmt.Errorf("ssh-kubectl driver: pod is required")
	}
	return &SSHKubectl{
		Host:      host,
		Context:   kubeContext,
		Namespace: namespace,
		Pod:       pod,
		Container: container,
		SSHArgs:   sshArgs,
	}, nil
}

// Exec implements Driver. Builds the command:
//
//	ssh <sshArgs...> <host> kubectl [--context <ctx>] exec -i \
//	    -n <ns> [-c <ctr>] <pod> -- <argv>
//
// SSH's argv1 is the *remote command* — ssh joins it with spaces before
// invoking the user's login shell on the remote. We rely on the argv pieces
// being shell-safe (which is the case for everything tmux-sync passes:
// `sh`, `tar`, `rm`, paths under our own dirs). If a script needs to be run,
// it should be fed on stdin to `sh`, exactly like the K8s driver does.
func (d *SSHKubectl) Exec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		return fmt.Errorf("ssh-kubectl exec: argv is empty")
	}

	args := make([]string, 0, len(d.SSHArgs)+12+len(argv))
	args = append(args, d.SSHArgs...)
	args = append(args, d.Host, "kubectl")
	if d.Context != "" {
		args = append(args, "--context", d.Context)
	}
	args = append(args, "exec", "-i", "-n", d.Namespace)
	if d.Container != "" {
		args = append(args, "-c", d.Container)
	}
	args = append(args, d.Pod, "--")
	args = append(args, argv...)

	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// String implements Driver.
func (d *SSHKubectl) String() string {
	return fmt.Sprintf("ssh-kubectl:%s→%s/%s", d.Host, d.Namespace, d.Pod)
}

// Close implements Driver.
func (d *SSHKubectl) Close() error { return nil }
