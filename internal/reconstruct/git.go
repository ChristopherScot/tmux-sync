// Package reconstruct contains the laptop-side counterparts to the remote
// capture steps: given a transferred bundle, it materializes the working
// tree, the tmux frame, and the editor sessions locally.
package reconstruct

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RestoreRepos applies each repos/*.bundle in bundleDir into a persistent
// local clone under workspaceRoot, landing on the `sync-wip` ref that carries
// the captured dirty working state.
//
// Layout:
//
//	bundleDir/repos/<name>.bundle      # input (from capture.Transfer)
//	workspaceRoot/<name>/              # output (persistent local clone)
//
// On a FRESH dest dir: `git clone <bundle>` + force-fetch heads from the
// bundle (some bundles only carry the cloned-from-default-branch when cloned
// directly; the explicit fetch picks up sync-wip and the rest).
//
// On an EXISTING dest dir: detach HEAD, then force-fetch
// `+refs/heads/*:refs/heads/*` from the bundle. Force-fetch is asymmetric:
// it OVERWRITES refs the bundle has (so the pod's view of shared branches
// wins, as the serial-handoff design requires) and LEAVES UNTOUCHED any
// refs the bundle doesn't have (so laptop-only branches survive). See the
// spec's "unpushed local branches" discussion for the full matrix.
//
// SAFETY: before any clobbering, every existing dest is checked for an
// uncommitted dirty working tree via `git status --porcelain`. If any has
// edits, RestoreRepos refuses with a clear error so the user can run
// `tmux-sync checkin` first (or `rm -rf` to discard).
func RestoreRepos(ctx context.Context, bundleDir, workspaceRoot string, stderr io.Writer) error {
	bundles, err := filepath.Glob(filepath.Join(bundleDir, "repos", "*.bundle"))
	if err != nil {
		return fmt.Errorf("RestoreRepos: glob: %w", err)
	}
	if len(bundles) == 0 {
		fmt.Fprintln(stderr, "restore-repos: no bundles found — nothing to restore")
		return nil
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return fmt.Errorf("RestoreRepos: mkdir workspace root: %w", err)
	}

	// Pass 1: clobber guard. We do this in a separate pass so the first
	// dirty repo's error doesn't leave us with a half-restored workspace.
	type plan struct{ name, bundle, dest string }
	plans := make([]plan, 0, len(bundles))
	for _, b := range bundles {
		name := strings.TrimSuffix(filepath.Base(b), ".bundle")
		dest := filepath.Join(workspaceRoot, name)
		plans = append(plans, plan{name, b, dest})

		if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
			continue // fresh clone — nothing to guard
		}
		dirty, err := hasUncommittedEdits(ctx, dest)
		if err != nil {
			return fmt.Errorf("RestoreRepos: %s: status check: %w", name, err)
		}
		if dirty {
			return fmt.Errorf("RestoreRepos: %s at %s has un-checked-in edits; run `tmux-sync checkin` first or `rm -rf %s` to discard them",
				name, dest, dest)
		}
	}

	// Pass 2: actually restore.
	for _, p := range plans {
		if err := restoreOne(ctx, p.bundle, p.dest, stderr); err != nil {
			return fmt.Errorf("RestoreRepos: %s: %w", p.name, err)
		}
	}
	fmt.Fprintf(stderr, "restore-repos: %d repos restored into %s\n", len(plans), workspaceRoot)
	return nil
}

func hasUncommittedEdits(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

func restoreOne(ctx context.Context, bundle, dest string, stderr io.Writer) error {
	// If there's no clone yet, create one. `git clone` brings the bundle's
	// refs in as remote-tracking refs and checks out the bundle's HEAD branch.
	if _, err := os.Stat(filepath.Join(dest, ".git")); os.IsNotExist(err) {
		if err := runGit(ctx, stderr, "clone", "--quiet", bundle, dest); err != nil {
			return fmt.Errorf("clone: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat: %w", err)
	}

	// From here, dest is a populated repo. We need to:
	//   1. Detach HEAD so the next step can force-update the branch we're on.
	//      (git refuses `fetch +refs/heads/X:refs/heads/X` if X is checked out.)
	//   2. Force-fetch every refs/heads/* from the bundle. Symmetric semantics:
	//      OVERWRITES refs the bundle has, LEAVES UNTOUCHED refs it doesn't —
	//      so laptop-only branches survive while the pod's view of shared
	//      branches wins.
	//   3. Land on sync-wip (the shadow commit carrying the dirty working tree).
	if err := runGit(ctx, io.Discard, "-C", dest, "checkout", "--quiet", "--detach"); err != nil {
		return fmt.Errorf("detach: %w", err)
	}
	if err := runGit(ctx, stderr, "-C", dest, "fetch", "--quiet", bundle, "+refs/heads/*:refs/heads/*"); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if err := runGit(ctx, stderr, "-C", dest, "checkout", "--quiet", "-B", "sync-wip", "sync-wip"); err != nil {
		return fmt.Errorf("checkout sync-wip: %w", err)
	}
	return nil
}

func runGit(ctx context.Context, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stderr = stderr
	cmd.Stdout = stderr
	return cmd.Run()
}
