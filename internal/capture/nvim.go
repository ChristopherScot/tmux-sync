// Package capture contains the in-container scripts that capture session
// state on the remote side. Each function builds a small POSIX-sh script and
// streams it on stdin to a bare remote `bash` via the Driver — the bash
// prototype's transport-agnostic trick, so there's no quoting hell and the
// same code path works through any backend (kubectl exec, ssh+docker, …).
package capture

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/christopherscot/tmux-sync/internal/driver"
)

// FlushNvim drives every live nvim instance on the remote to:
//   1. write all changed buffers (`:silent! wall`) — disk becomes the source
//      of truth for editor content, so the file snapshot in later steps
//      captures everything,
//   2. snapshot its own window/buffer/split layout to a per-socket .vim
//      session file under outDir, ready to be reopened on the laptop with
//      `nvim -S`.
//
// Implementation: enumerates nvim sockets under /tmp/nvim* and /run/user/*/
// nvim.* (the standard XDG and tmp locations), and for each runs
// `nvim --server <sock> --remote-expr "execute('silent! wall | mksession! …')"`.
// `--remote-expr` is mode-independent (doesn't care whether nvim is in
// insert/visual/cmdline) and `silent!` swallows E141 for unnamed scratch
// buffers — the rare cost we accept here (no filename → can't be written).
//
// outDir is created on the remote if needed. stderr receives the script's
// summary line (`nvim-flush: N flushed, M skipped, …`) plus any per-socket
// errors.
//
// Returns nil even when zero live nvims are found — that's a normal state
// (a freshly-attached pod hasn't run nvim yet).
func FlushNvim(ctx context.Context, d driver.Driver, outDir string, stderr io.Writer) error {
	if outDir == "" {
		return fmt.Errorf("FlushNvim: outDir is required")
	}
	script := nvimFlushScript(outDir)
	return d.Exec(ctx, []string{"sh"}, strings.NewReader(script), io.Discard, stderr)
}

// nvimFlushScript is exported only via FlushNvim; kept package-private so the
// quoting contract (POSIX sh, single shell quote around outDir) is local.
func nvimFlushScript(outDir string) string {
	// outDir is single-quoted in the script. Embed any literal single quotes
	// using the standard '\'' trick so the script remains valid sh.
	q := strings.ReplaceAll(outDir, `'`, `'\''`)
	return fmt.Sprintf(`set -u
out='%s'
mkdir -p "$out"
map="$out/pane-map.txt"
: > "$map"
count=0
flushed=0
skipped=0
mapped=0
for s in $(find /tmp/nvim* /run/user/*/nvim.* -type s -name 'nvim.*' 2>/dev/null); do
    count=$((count + 1))
    sname=$(basename "$s")
    # Where in tmux is this nvim running? $TMUX_PANE is inherited from the
    # spawning pane. We use the session:window.pane *index* string (e.g.
    # "work-homelab:1.1") because the pane *id* (%5) is server-generated and
    # doesn't survive a server restart; the indexed location does (tmux-
    # resurrect rebuilds the same windows/panes in the same positions).
    pane_id=$(nvim --server "$s" --remote-expr "expand('\$TMUX_PANE')" 2>/dev/null)
    pane_loc=""
    if [ -n "$pane_id" ]; then
        pane_loc=$(tmux display-message -t "$pane_id" -p '#{session_name}:#{window_index}.#{pane_index}' 2>/dev/null)
    fi
    expr="execute('silent! wall | mksession! ${out}/${sname}.vim')"
    if nvim --server "$s" --remote-expr "$expr" >/dev/null 2>&1; then
        flushed=$((flushed + 1))
        if [ -n "$pane_loc" ]; then
            printf '%%s %%s\n' "$pane_loc" "$sname.vim" >> "$map"
            mapped=$((mapped + 1))
        fi
    else
        skipped=$((skipped + 1))
    fi
done
printf 'nvim-flush: %%d flushed (%%d mapped to panes), %%d skipped (dead socket / no nvim), %%d total\n' \
    "$flushed" "$mapped" "$skipped" "$count" >&2
`, q)
}
