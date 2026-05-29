package capture

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/christopherscot/tmux-sync/internal/driver"
)

// BundleRepos walks /workspace on the remote, finds each git repo, snapshots
// its dirty working tree (tracked + untracked + uncommitted) onto a `sync-wip`
// ref via `git write-tree` + `git commit-tree`, and writes one `<name>.bundle`
// per repo into outDir/repos/.
//
// The shadow-commit trick — pioneered by the bash prototype — captures the
// *exact* current working state, including untracked files, without moving
// HEAD or any user branch. We go one step further than the prototype by
// using a private GIT_INDEX_FILE so the user's real index is never touched
// (the prototype briefly mutated it via `git add -A; ... git reset -q`).
//
// The bundles are self-contained: `git bundle create --all` packs every
// ref (including sync-wip), so the laptop can `git clone <bundle>` and
// immediately see the dirty state on the sync-wip branch.
//
// Non-repo entries under workspaceRoot are counted but left for the loose-
// files step (BundleLooseFiles).
//
// workspaceRoot is whichever absolute path the workspace lives at *behind
// the Driver*: "/workspace" for the K8s driver (pod) or DockerExec driver
// (the bind mount inside the local container); the laptop's local workspaces
// directory for the Local driver (checkin path).
func BundleRepos(ctx context.Context, d driver.Driver, workspaceRoot, outDir string, stderr io.Writer) error {
	if workspaceRoot == "" {
		return fmt.Errorf("BundleRepos: workspaceRoot is required")
	}
	if outDir == "" {
		return fmt.Errorf("BundleRepos: outDir is required")
	}
	script := bundleReposScript(workspaceRoot, outDir)
	return d.Exec(ctx, []string{"sh"}, strings.NewReader(script), io.Discard, stderr)
}

func bundleReposScript(workspaceRoot, outDir string) string {
	wsq := strings.ReplaceAll(workspaceRoot, `'`, `'\''`)
	q := strings.ReplaceAll(outDir, `'`, `'\''`)
	return fmt.Sprintf(`set -u
out='%s'
workspace='%s'
mkdir -p "$out/repos"

total=0
repos_done=0
repos_bytes=0
loose=0
errors=0

for entry in "$workspace"/*/; do
    [ -d "$entry" ] || continue
    entry=${entry%%/}
    name=$(basename "$entry")
    case "$name" in lost+found|.*) continue ;; esac
    total=$((total + 1))

    if ! (cd "$entry" && git rev-parse --git-dir >/dev/null 2>&1); then
        loose=$((loose + 1))
        continue
    fi

    # Snapshot dirty working tree onto refs/heads/sync-wip without touching
    # the user's real index, HEAD, or any of their branches. A private
    # GIT_INDEX_FILE captures (tracked + untracked + uncommitted); write-tree
    # objectifies it; commit-tree wraps it into a commit; update-ref points
    # the sync-wip ref at it. Then bundle --all packs everything.
    bundle="$out/repos/${name}.bundle"
    idx=$(mktemp)
    if (
        cd "$entry"
        GIT_INDEX_FILE="$idx" git read-tree HEAD 2>/dev/null || true
        GIT_INDEX_FILE="$idx" git add -A 2>/dev/null
        tree=$(GIT_INDEX_FILE="$idx" git write-tree 2>/dev/null) || exit 1
        if parent=$(git rev-parse -q --verify HEAD 2>/dev/null); then
            snap=$(git commit-tree "$tree" -p "$parent" -m "sync-wip") || exit 1
        else
            snap=$(git commit-tree "$tree" -m "sync-wip") || exit 1
        fi
        git update-ref refs/heads/sync-wip "$snap" || exit 1
        git bundle create "$bundle" --all >/dev/null 2>&1
    ); then
        b=$(wc -c < "$bundle" 2>/dev/null | tr -d ' ')
        b=${b:-0}
        repos_done=$((repos_done + 1))
        repos_bytes=$((repos_bytes + b))
    else
        errors=$((errors + 1))
    fi
    rm -f "$idx"
done

printf 'git-bundle: %%d repos (%%d B), %%d loose, %%d errors, %%d total\n' \
    "$repos_done" "$repos_bytes" "$loose" "$errors" "$total" >&2
`, q, wsq)
}
