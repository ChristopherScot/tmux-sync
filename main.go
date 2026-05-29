// Command tmux-sync ports a live remote tmux session to your laptop and back.
//
// It captures the full remote session — tmux layout, scrollback, every running
// nvim's open files/splits, claude's conversation transcript, and the working
// files — moves it to a container on your laptop, lets you work offline, and
// reconciles the changes back when you return.
//
// See SPEC.md in this repo for the design.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/christopherscot/tmux-sync/internal/capture"
	"github.com/christopherscot/tmux-sync/internal/config"
	"github.com/christopherscot/tmux-sync/internal/reconstruct"
)

// Set at build time by GoReleaser.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	// Best-effort auto-update before running a real command.
	// Skipped for diagnostic commands (you should be able to print --version
	// reliably without surprise network traffic or a binary swap).
	if !isDiagnostic(args[0]) {
		maybeSelfUpdate()
	}
	switch args[0] {
	case "checkout":
		cmdCheckout(args[1:])
	case "checkin":
		cmdCheckin(args[1:])
	case "status":
		cmdStatus(args[1:])
	case "list":
		cmdList(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("tmux-sync %s (commit %s, built %s)\n", version, commit, date)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "tmux-sync: unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tmux-sync — port a live remote tmux session to your laptop and back

usage: tmux-sync <command> [args...]

Commands:
  checkout --from <endpoint> [--session <name>]   pull a session down + reconstruct locally
  checkin  --to   <endpoint> [--session <name>]   push it back + resume on the remote
  status                                          what's checked out where
  list     --from <endpoint>                      sessions available to check out
  version                                         print version + build info
  help                                            this message

See SPEC.md for the design.
`)
}

// isDiagnostic reports whether a command should run without triggering the
// background auto-update (we don't want `tmux-sync --version` to ever swap the
// binary mid-diagnosis, or `--help` to make a network request).
func isDiagnostic(cmd string) bool {
	switch cmd {
	case "version", "--version", "-v", "help", "--help", "-h":
		return true
	}
	return false
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "tmux-sync: %v\n", err)
	os.Exit(1)
}

func notYet(cmd string) {
	fmt.Fprintf(os.Stderr, "tmux-sync %s: not yet implemented — see SPEC.md\n", cmd)
	os.Exit(1)
}

func cmdCheckout(args []string) {
	fs := flag.NewFlagSet("checkout", flag.ExitOnError)
	from := fs.String("from", "", "endpoint name to check out from (defined in config.yaml)")
	session := fs.String("session", "", "session name to capture (default: the foreground session)")
	_ = fs.Parse(args)
	if *from == "" {
		fmt.Fprintln(os.Stderr, "checkout: --from <endpoint> required")
		os.Exit(2)
	}
	_ = session // TODO: thread through to session capture

	cfg, err := config.Load()
	if err != nil {
		fail(err)
	}
	d, err := cfg.Resolve(*from)
	if err != nil {
		fail(err)
	}
	defer d.Close()

	// Bundle dir on the REMOTE side: all capture steps drop their artifacts
	// here, then the final tar-up step ships the whole tree to the laptop.
	bundleDir := fmt.Sprintf("/tmp/tmux-sync/checkout-%s", time.Now().UTC().Format("20060102-150405"))
	fmt.Fprintf(os.Stderr, "tmux-sync checkout: target = %s\n", d.String())
	fmt.Fprintf(os.Stderr, "  remote bundle dir = %s\n", bundleDir)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Step 1/N — flush every live nvim to disk + capture per-instance
	// mksession. After this, no editor content lives only in memory.
	nvimDir := bundleDir + "/nvim"
	if err := capture.FlushNvim(ctx, d, nvimDir, os.Stderr); err != nil {
		fail(fmt.Errorf("nvim flush: %w", err))
	}

	// Step 2/N — tmux-resurrect save: layout / cwd / per-pane running command
	// / scrollback. The image enables @resurrect-capture-pane-contents on.
	tmuxDir := bundleDir + "/tmux"
	if err := capture.SaveTmuxResurrect(ctx, d, tmuxDir, os.Stderr); err != nil {
		fail(fmt.Errorf("tmux-resurrect save: %w", err))
	}

	// Step 3/N — per-repo git bundle of the dirty working tree (tracked +
	// untracked + uncommitted), captured onto a sync-wip ref via the
	// shadow-commit trick without disturbing the user's index/HEAD/branches.
	if err := capture.BundleRepos(ctx, d, bundleDir, os.Stderr); err != nil {
		fail(fmt.Errorf("git bundle: %w", err))
	}

	// Step 4/N — package the remote bundle and stream it down to the laptop.
	// `tar czf -` on the remote, piped through Exec, untar'd in-process here.
	localBase, err := os.UserHomeDir()
	if err != nil {
		fail(fmt.Errorf("locate home dir: %w", err))
	}
	localCheckoutDir := filepath.Join(localBase, ".tmux-sync", "checkouts")
	landed, err := capture.Transfer(ctx, d, bundleDir, localCheckoutDir, os.Stderr)
	if err != nil {
		fail(fmt.Errorf("transfer: %w", err))
	}
	fmt.Fprintf(os.Stderr, "transfer: bundle landed at %s\n", landed)

	// Best-effort: clean up the remote bundle dir now that we have it locally.
	// Failure here is non-fatal — the user already has the bundle.
	if rmErr := d.Exec(ctx, []string{"rm", "-rf", bundleDir}, nil, os.Stderr, os.Stderr); rmErr != nil {
		fmt.Fprintf(os.Stderr, "transfer: warning: remote cleanup of %s failed: %v\n", bundleDir, rmErr)
	}

	// Step 5/N — restore the repo bundles into persistent local clones.
	// Each repo lands on the captured `sync-wip` ref (the shadow commit), so
	// the working tree reflects the exact mid-edit state from the pod.
	// Existing clones with un-checked-in edits are refused (no clobber).
	workspaceRoot := filepath.Join(localBase, ".tmux-sync", "workspaces", *from)
	if err := reconstruct.RestoreRepos(ctx, landed, workspaceRoot, os.Stderr); err != nil {
		fail(fmt.Errorf("restore repos: %w", err))
	}

	// Step 6/N — launch the local container running the same image and
	// restore the tmux frame inside it. Silently skipped if `docker` isn't
	// on PATH (the bundle + repo clones are already useful without it).
	attach, err := reconstruct.LaunchAndRestore(ctx, reconstruct.ContainerOptions{
		Image:          "ghcr.io/christopherscot/claude-pod:latest",
		Name:           "tmux-sync-" + *from,
		WorkspaceMount: workspaceRoot,
		HomeVolume:     "tmux-sync-home-" + *from,
		BundleDir:      landed,
	}, os.Stderr)
	if err != nil {
		fail(fmt.Errorf("launch and restore: %w", err))
	}

	fmt.Fprintln(os.Stderr, "checkout: ✓ remote captured + transferred + repos restored locally + container restore triggered.")
	if attach != nil {
		fmt.Fprintf(os.Stderr, "\nAttach to your session with:\n  %s\n", strings.Join(attach, " "))
	} else {
		fmt.Fprintf(os.Stderr, "\nYour synced files are at: %s\n", workspaceRoot)
	}
	os.Exit(0)
}

func cmdCheckin(args []string) { notYet("checkin") }
func cmdStatus(args []string)  { notYet("status") }
func cmdList(args []string)    { notYet("list") }
