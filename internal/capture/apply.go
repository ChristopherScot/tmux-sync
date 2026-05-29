package capture

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/christopherscot/tmux-sync/internal/driver"
)

// ApplyCheckin runs the pod-side restore for an uploaded checkin bundle. For
// each repo bundle, it stashes the pod's current state (recoverable via
// `git stash list`), force-fetches just refs/heads/sync-wip from the bundle,
// and checks it out. Loose files are cp -af'd back into workspaceRoot. The
// nvim mksession files are dropped under ~/.tmux-sync/nvim-sessions/ for
// future per-pane wiring.
//
// Symmetric to reconstruct.RestoreRepos but on the *pod* side, and
// conservative on branches: only sync-wip moves (matches the bash prototype),
// so the pod's regular branches stay untouched — only the WIP changes flow
// over.
func ApplyCheckin(ctx context.Context, d driver.Driver, remoteBundleDir, workspaceRoot string, stderr io.Writer) error {
	if remoteBundleDir == "" || workspaceRoot == "" {
		return fmt.Errorf("ApplyCheckin: remoteBundleDir and workspaceRoot are required")
	}
	script := applyCheckinScript(remoteBundleDir, workspaceRoot)
	return d.Exec(ctx, []string{"sh"}, strings.NewReader(script), io.Discard, stderr)
}

func applyCheckinScript(bundleDir, workspaceRoot string) string {
	bd := strings.ReplaceAll(bundleDir, `'`, `'\''`)
	wr := strings.ReplaceAll(workspaceRoot, `'`, `'\''`)
	return fmt.Sprintf(`set -u
bundle='%s'
workspace='%s'

restored=0
errors=0

for b in "$bundle"/repos/*.bundle; do
    [ -f "$b" ] || continue
    name=$(basename "$b" .bundle)
    target="$workspace/$name"
    if ! (cd "$target" 2>/dev/null && git rev-parse --git-dir >/dev/null 2>&1); then
        echo "  apply-checkin: skip $name — no git repo at $target" >&2
        errors=$((errors + 1))
        continue
    fi
    if (
        cd "$target"
        # Stash whatever's on disk so the user can recover if they want.
        # -u keeps untracked files; the message tags it so it's easy to find.
        git stash push -u -m "tmux-sync-checkin $(date -u +%%FT%%TZ)" >/dev/null 2>&1 || true
        # Detach so the fetch can force-update branch refs we might be on.
        git checkout -q --detach 2>/dev/null || true
        # Force-fetch ONLY sync-wip (conservative — the bash prototype's
        # choice). The pod's other branches stay where they were; only the
        # WIP changes flow over.
        git fetch -q "$b" "+refs/heads/sync-wip:refs/heads/sync-wip" && \
        git checkout -q -B sync-wip sync-wip
    ); then
        restored=$((restored + 1))
    else
        errors=$((errors + 1))
    fi
done

# Loose files: overwrite into workspace (last-writer-wins, by design).
if [ -d "$bundle/loose" ]; then
    cp -af "$bundle"/loose/. "$workspace/" 2>/dev/null || true
fi

# nvim sessions: stash for future per-pane wiring on the pod side.
if [ -d "$bundle/nvim" ]; then
    mkdir -p "$HOME/.tmux-sync/nvim-sessions"
    cp -af "$bundle"/nvim/* "$HOME/.tmux-sync/nvim-sessions/" 2>/dev/null || true
fi

printf 'apply-checkin: %%d repos restored, %%d errors\n' "$restored" "$errors" >&2
`, bd, wr)
}
