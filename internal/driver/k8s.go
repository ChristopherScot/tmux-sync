package driver

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// K8s execs into a pod via the local `kubectl` binary. The current environment
// must have a kubeconfig that resolves the named context (empty = current).
type K8s struct {
	// Context is the kubeconfig context to use; empty means current-context.
	Context string
	// Namespace is the pod's namespace; required.
	Namespace string
	// Pod is the target pod name; required.
	Pod string
	// Container is the optional container name inside the pod (passes -c).
	Container string
}

// NewK8s constructs a validated K8s driver.
func NewK8s(kubeContext, namespace, pod, container string) (*K8s, error) {
	if namespace == "" {
		return nil, fmt.Errorf("k8s driver: namespace is required")
	}
	if pod == "" {
		return nil, fmt.Errorf("k8s driver: pod is required")
	}
	return &K8s{Context: kubeContext, Namespace: namespace, Pod: pod, Container: container}, nil
}

// Exec implements Driver.
func (d *K8s) Exec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		return fmt.Errorf("k8s exec: argv is empty")
	}
	args := make([]string, 0, 10+len(argv))
	if d.Context != "" {
		args = append(args, "--context", d.Context)
	}
	args = append(args, "exec", "-i", "-n", d.Namespace)
	if d.Container != "" {
		args = append(args, "-c", d.Container)
	}
	args = append(args, d.Pod, "--")
	args = append(args, argv...)

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// String implements Driver.
func (d *K8s) String() string {
	return fmt.Sprintf("k8s:%s/%s", d.Namespace, d.Pod)
}

// Close implements Driver.
func (d *K8s) Close() error { return nil }
