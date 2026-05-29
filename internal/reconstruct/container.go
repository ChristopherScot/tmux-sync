package reconstruct

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ContainerOptions configures the local container that hosts the reconstructed
// session.
type ContainerOptions struct {
	// Image is the container image. Should match what the remote ran so that
	// paths, tools, and tmux/nvim config line up — typically
	// ghcr.io/christopherscot/claude-pod:latest, multi-arch.
	Image string
	// Name uniquely identifies the local container (we derive it from the
	// endpoint name so re-checkouts reuse the same container).
	Name string
	// WorkspaceMount is the local path mounted at /workspace inside the
	// container. The remote bundle's absolute paths (in :mksession and
	// resurrect.txt) reference /workspace/<repo>, so this MUST be the
	// directory containing the per-repo clones from RestoreRepos.
	WorkspaceMount string
	// HomeVolume is the docker named volume mounted at /home/node. Using a
	// named volume (not a bind) lets the image's entrypoint seed $HOME from
	// /opt/home-skel on first boot and re-sync ~/.tmux.conf every boot
	// without surprises, and caches LazyVim plugins across sessions.
	HomeVolume string
	// BundleDir is the local bundle directory (Transfer's "landed" path).
	// LaunchAndRestore copies the captured tmux/nvim/claude state from here
	// into the container.
	BundleDir string
}

// LaunchAndRestore ensures the local container is up, copies the captured
// session state into it, triggers tmux-resurrect restore, and returns the
// command the caller should exec or print so the user can attach.
//
// Returns (nil, nil) — with a printed explanation — when `docker` is not on
// PATH. That lets the rest of checkout still complete (the user has the
// bundle + local clones); they can install Docker and re-run when ready.
func LaunchAndRestore(ctx context.Context, opts ContainerOptions, stderr io.Writer) ([]string, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		fmt.Fprintln(stderr, "container: docker not found on PATH — skipping local launch.")
		fmt.Fprintln(stderr, "          install Docker on the laptop and re-run `tmux-sync checkout` to get the full restored session.")
		return nil, nil
	}
	if err := ensureContainerRunning(ctx, opts, stderr); err != nil {
		return nil, fmt.Errorf("ensureContainerRunning: %w", err)
	}
	if err := copyBundleIn(ctx, opts, stderr); err != nil {
		return nil, fmt.Errorf("copyBundleIn: %w", err)
	}
	if err := triggerResurrectRestore(ctx, opts, stderr); err != nil {
		return nil, fmt.Errorf("triggerResurrectRestore: %w", err)
	}
	// Per-pane nvim mksession: resurrect relaunches `nvim` in each pane that
	// had one; this step then sources our captured mksession into each so the
	// pane comes back with the right buffer list / window splits / jumplist.
	// Best-effort — failure here doesn't break the rest of restore.
	if err := sourceNvimSessions(ctx, opts, stderr); err != nil {
		fmt.Fprintf(stderr, "container: warning: nvim session sourcing failed: %v\n", err)
	}
	return []string{"docker", "exec", "-it", opts.Name, "tmux", "attach"}, nil
}

// ensureContainerRunning makes a container named opts.Name exist and be in
// the Running state, idempotently:
//   - already running → no-op
//   - exists but stopped → docker start
//   - does not exist → docker run -d with the right mounts + sleep infinity
func ensureContainerRunning(ctx context.Context, opts ContainerOptions, stderr io.Writer) error {
	state, err := containerState(ctx, opts.Name)
	if err != nil {
		return err
	}
	switch state {
	case "running":
		fmt.Fprintf(stderr, "container: %s already running\n", opts.Name)
		return nil
	case "exited", "created", "paused":
		fmt.Fprintf(stderr, "container: starting existing %s (was %s)\n", opts.Name, state)
		return runDocker(ctx, stderr, "start", opts.Name)
	case "missing":
		fmt.Fprintf(stderr, "container: creating %s from %s\n", opts.Name, opts.Image)
		return runDocker(ctx, stderr,
			"run", "-d",
			"--name", opts.Name,
			"-v", opts.WorkspaceMount+":/workspace",
			"-v", opts.HomeVolume+":/home/node",
			"-w", "/workspace",
			opts.Image,
			"sleep", "infinity",
		)
	default:
		return fmt.Errorf("unexpected container state %q for %s", state, opts.Name)
	}
}

// containerState reports the state of a docker container by name, or
// "missing" if no such container exists.
func containerState(ctx context.Context, name string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Status}}", name)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		// inspect returns non-zero when the container doesn't exist.
		return "missing", nil
	}
	return strings.TrimSpace(out.String()), nil
}

// copyBundleIn drops the captured tmux + nvim + claude artifacts into the
// container so the restore step finds them where it expects.
func copyBundleIn(ctx context.Context, opts ContainerOptions, stderr io.Writer) error {
	// Ensure the destination dirs exist inside the container.
	if err := dockerExec(ctx, opts.Name, stderr, "mkdir", "-p",
		"/home/node/.local/share/tmux/resurrect",
		"/home/node/.tmux-sync/nvim-sessions",
		"/home/node/.claude/projects"); err != nil {
		return err
	}

	// tmux-resurrect metadata
	if path := filepath.Join(opts.BundleDir, "tmux", "resurrect.txt"); fileExists(path) {
		if err := dockerCp(ctx, stderr, path, opts.Name+":/home/node/.local/share/tmux/resurrect/last"); err != nil {
			return fmt.Errorf("copy resurrect.txt: %w", err)
		}
	}
	// pane scrollback
	if path := filepath.Join(opts.BundleDir, "tmux", "pane_contents.tar.gz"); fileExists(path) {
		if err := dockerCp(ctx, stderr, path, opts.Name+":/home/node/.local/share/tmux/resurrect/pane_contents.tar.gz"); err != nil {
			return fmt.Errorf("copy pane_contents: %w", err)
		}
	}
	// per-nvim mksession files
	if nvimDir := filepath.Join(opts.BundleDir, "nvim"); dirExists(nvimDir) {
		// docker cp <localdir>/. into <container>:<destdir>/ to copy contents only
		if err := dockerCp(ctx, stderr, nvimDir+"/.", opts.Name+":/home/node/.tmux-sync/nvim-sessions/"); err != nil {
			return fmt.Errorf("copy nvim sessions: %w", err)
		}
	}
	// claude transcripts — bundle/claude/projects/ → /home/node/.claude/projects/
	// so `claude --resume <session-id>` can find them.
	if claudeProjects := filepath.Join(opts.BundleDir, "claude", "projects"); dirExists(claudeProjects) {
		if err := dockerCp(ctx, stderr, claudeProjects+"/.", opts.Name+":/home/node/.claude/projects/"); err != nil {
			return fmt.Errorf("copy claude projects: %w", err)
		}
	}

	// Fix ownership so the (non-root) node user can read everything.
	// `docker cp` lands files as root inside the container by default.
	if err := dockerExec(ctx, opts.Name, stderr, "chown", "-R", "1000:1000",
		"/home/node/.local/share/tmux/resurrect",
		"/home/node/.tmux-sync",
		"/home/node/.claude/projects"); err != nil {
		return fmt.Errorf("chown bundle in container: %w", err)
	}
	return nil
}

// triggerResurrectRestore starts a tmux server inside the container (if not
// already) and invokes the resurrect plugin's restore script, which rebuilds
// the windows/panes/cwds, relaunches the whitelisted programs (incl. nvim),
// and (with capture-pane-contents) puts scrollback back on screen.
func triggerResurrectRestore(ctx context.Context, opts ContainerOptions, stderr io.Writer) error {
	// Start a detached server if there isn't one yet, so run-shell has
	// somewhere to attach. -2 forces 256-color terminal handling.
	script := `set -e
tmux ls >/dev/null 2>&1 || tmux new-session -d -s _bootstrap
restore="$HOME/.tmux/plugins/tmux-resurrect/scripts/restore.sh"
[ -x "$restore" ] || { echo "container: tmux-resurrect restore script not found at $restore" >&2; exit 1; }
tmux run-shell "$restore"
# Drop the bootstrap session if a real one was restored alongside it.
if [ "$(tmux list-sessions -F '#S' | wc -l)" -gt 1 ]; then
    tmux kill-session -t _bootstrap 2>/dev/null || true
fi
echo "container: tmux-resurrect restore triggered" >&2
`
	return dockerExecScript(ctx, opts.Name, stderr, script)
}

// sourceNvimSessions reads the pane-map.txt captured at flush time and, for
// each (pane-location → mksession file) entry, finds the current pane at that
// location and `:source`s the session file into the nvim that tmux-resurrect
// just relaunched there.
//
// Mechanism: tmux send-keys (with an Esc preamble to ensure normal mode) into
// the resolved pane id. Yes, send-keys is mode-fragile — but immediately after
// resurrect's restart the pane is the freshly-spawned `nvim`, which IS in
// normal mode with an empty buffer, so the Esc+`:source X<CR>` sequence lands
// reliably. A small sleep upfront lets nvim finish coming up.
//
// No-op (and no error) when the bundle had no per-pane mapping (e.g. resurrect
// captured panes but the capture happened before this commit, or there were
// no live nvims).
func sourceNvimSessions(ctx context.Context, opts ContainerOptions, stderr io.Writer) error {
	script := `set -u
map="$HOME/.tmux-sync/nvim-sessions/pane-map.txt"
[ -f "$map" ] || { echo "nvim-sessions: no pane-map.txt (nothing to source)" >&2; exit 0; }

# Give the panes resurrect just respawned a moment to land in nvim.
sleep 0.6

sourced=0
missing=0
while IFS=' ' read -r loc sess; do
    [ -n "$loc" ] && [ -n "$sess" ] || continue
    pane=$(tmux list-panes -a -F '#{pane_id} #{session_name}:#{window_index}.#{pane_index}' 2>/dev/null \
        | awk -v L="$loc" '$2==L{print $1; exit}')
    if [ -z "$pane" ]; then
        missing=$((missing + 1))
        continue
    fi
    sess_path="$HOME/.tmux-sync/nvim-sessions/$sess"
    [ -f "$sess_path" ] || { missing=$((missing + 1)); continue; }
    # Esc → leave any pending keymap; :source <path> → load the mksession; <CR>.
    tmux send-keys -t "$pane" Escape
    tmux send-keys -t "$pane" ":source $sess_path" Enter
    sourced=$((sourced + 1))
done < "$map"
printf 'nvim-sessions: %d sourced, %d unmatched (pane gone / file missing)\n' \
    "$sourced" "$missing" >&2
`
	return dockerExecScript(ctx, opts.Name, stderr, script)
}

// --- helpers ----------------------------------------------------------------

func runDocker(ctx context.Context, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	return cmd.Run()
}

func dockerExec(ctx context.Context, name string, stderr io.Writer, argv ...string) error {
	full := append([]string{"exec", name}, argv...)
	return runDocker(ctx, stderr, full...)
}

// dockerExecScript runs `sh` inside the container with the given script on
// stdin — the same transport-agnostic trick the capture path uses, so we
// avoid shell-quoting the script body.
func dockerExecScript(ctx context.Context, name string, stderr io.Writer, script string) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", name, "sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	return cmd.Run()
}

func dockerCp(ctx context.Context, stderr io.Writer, src, dst string) error {
	return runDocker(ctx, stderr, "cp", src, dst)
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
