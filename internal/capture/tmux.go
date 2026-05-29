package capture

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/christopherscot/tmux-sync/internal/driver"
)

// SaveTmuxResurrect triggers tmux-resurrect's `save.sh` on the remote (which
// writes per-pane session/window/cwd/command metadata and, with
// @resurrect-capture-pane-contents on, dumps scrollback per pane into a
// sibling tarball) and copies the produced artifacts into outDir.
//
// Layout under outDir after this returns:
//
//	outDir/
//	  resurrect.txt            # the latest tmux-resurrect TSV (the `last`
//	                           # symlink target — one line per pane)
//	  pane_contents.tar.gz     # OPTIONAL: per-pane scrollback (only when
//	                           # @resurrect-capture-pane-contents is on)
//
// The image already installs tmux-resurrect under
// ~/.tmux/plugins/tmux-resurrect and enables capture-pane-contents, so on a
// stock claude-pod both files are produced.
//
// Returns an error if there's no running tmux server or the plugin is missing.
func SaveTmuxResurrect(ctx context.Context, d driver.Driver, outDir string, stderr io.Writer) error {
	if outDir == "" {
		return fmt.Errorf("SaveTmuxResurrect: outDir is required")
	}
	script := tmuxResurrectSaveScript(outDir)
	return d.Exec(ctx, []string{"sh"}, strings.NewReader(script), io.Discard, stderr)
}

func tmuxResurrectSaveScript(outDir string) string {
	q := strings.ReplaceAll(outDir, `'`, `'\''`)
	return fmt.Sprintf(`set -u
out='%s'
mkdir -p "$out"

command -v tmux >/dev/null 2>&1 || { echo "tmux-resurrect: no tmux binary" >&2; exit 1; }
tmux ls >/dev/null 2>&1            || { echo "tmux-resurrect: no tmux server running" >&2; exit 1; }
save="$HOME/.tmux/plugins/tmux-resurrect/scripts/save.sh"
[ -x "$save" ] || { echo "tmux-resurrect: save script not found at $save" >&2; exit 1; }

# Trigger save through tmux so it inherits the server's environment.
tmux run-shell "$save"

# Locate the resurrect dir (~/.local/share/tmux/resurrect on modern installs).
RDIR=""
for d in "$HOME/.local/share/tmux/resurrect" "$HOME/.tmux/resurrect"; do
    [ -d "$d" ] && { RDIR="$d"; break; }
done
[ -d "$RDIR" ] || { echo "tmux-resurrect: dir not found" >&2; exit 1; }

last="$RDIR/last"
[ -e "$last" ] || { echo "tmux-resurrect: no save produced (no '$last')" >&2; exit 1; }
cp -L "$last" "$out/resurrect.txt"

# Optional: pane-contents tarball (only present when @resurrect-capture-pane-contents on)
if [ -f "$RDIR/pane_contents.tar.gz" ]; then
    cp "$RDIR/pane_contents.tar.gz" "$out/pane_contents.tar.gz"
fi

panes=$(grep -c '^pane' "$out/resurrect.txt" 2>/dev/null || echo 0)
meta_b=$(wc -c < "$out/resurrect.txt" | tr -d ' ')
contents_b=0
[ -f "$out/pane_contents.tar.gz" ] && contents_b=$(wc -c < "$out/pane_contents.tar.gz" | tr -d ' ')
printf 'tmux-resurrect: %%s panes, %%s B metadata, %%s B pane contents\n' \
    "$panes" "$meta_b" "$contents_b" >&2
`, q)
}
