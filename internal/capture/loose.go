package capture

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/christopherscot/tmux-sync/internal/driver"
)

// BundleLooseFiles copies any non-repo directories under /workspace on the
// remote into outDir/loose/, so they ride along in the same tar stream the
// transfer step ships. These are the entries BundleRepos counted but didn't
// bundle (no .git, not in a git worktree).
//
// Conservative scope by design: only direct children of /workspace are
// copied (no /workspace itself, no `lost+found`, no dot-entries). The
// serial-handoff model means plain `cp -a` is enough — no merge needed.
//
// At restore time, the laptop side mirrors the loose tree back into
// workspaceRoot via reconstruct.RestoreLooseFiles.
func BundleLooseFiles(ctx context.Context, d driver.Driver, outDir string, stderr io.Writer) error {
	if outDir == "" {
		return fmt.Errorf("BundleLooseFiles: outDir is required")
	}
	script := bundleLooseFilesScript(outDir)
	return d.Exec(ctx, []string{"sh"}, strings.NewReader(script), io.Discard, stderr)
}

func bundleLooseFilesScript(outDir string) string {
	q := strings.ReplaceAll(outDir, `'`, `'\''`)
	return fmt.Sprintf(`set -u
out='%s'
workspace='/workspace'
mkdir -p "$out/loose"

count=0
for entry in "$workspace"/*/; do
    [ -d "$entry" ] || continue
    entry=${entry%%/}
    name=$(basename "$entry")
    case "$name" in lost+found|.*) continue ;; esac
    if [ -d "$entry/.git" ] || (cd "$entry" && git rev-parse --git-dir >/dev/null 2>&1); then
        continue   # repo — handled by BundleRepos
    fi
    cp -a "$entry" "$out/loose/"
    count=$((count + 1))
done

bytes=0
if [ "$count" -gt 0 ]; then
    # du -sb is GNU; fall back to wc-on-tar if -b is unsupported.
    bytes=$(du -sb "$out/loose" 2>/dev/null | cut -f1)
    [ -n "$bytes" ] || bytes='?'
fi
printf 'loose-files: %%s entries, %%s bytes copied\n' "$count" "$bytes" >&2
`, q)
}
