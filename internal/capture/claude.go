package capture

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/christopherscot/tmux-sync/internal/driver"
)

// CaptureClaudeTranscripts copies the remote's ~/.claude/projects/ tree (the
// jsonl session transcripts that `claude --resume <session-id>` reads) into
// outDir/claude/projects/. No-op (no error) when the remote has no
// ~/.claude/projects/ — that's the normal state on a pod that has never run
// claude.
//
// We ship the whole projects tree rather than trying to figure out which
// session each pane was on: there's no reliable way to pull a live claude's
// session id out without a debugger, and the tree is usually small enough
// (jsonl text). Per-pane `--resume` wiring would build on top of this by
// recording, at flush time, which session id was in each pane.
func CaptureClaudeTranscripts(ctx context.Context, d driver.Driver, outDir string, stderr io.Writer) error {
	if outDir == "" {
		return fmt.Errorf("CaptureClaudeTranscripts: outDir is required")
	}
	script := claudeCaptureScript(outDir)
	return d.Exec(ctx, []string{"sh"}, strings.NewReader(script), io.Discard, stderr)
}

func claudeCaptureScript(outDir string) string {
	q := strings.ReplaceAll(outDir, `'`, `'\''`)
	return fmt.Sprintf(`set -u
out='%s'
src="$HOME/.claude/projects"
if [ ! -d "$src" ]; then
    echo "claude: no ~/.claude/projects (claude not initialized on this pod?)" >&2
    exit 0
fi
mkdir -p "$out/claude"
cp -a "$src" "$out/claude/projects"
projects=$(find "$out/claude/projects" -maxdepth 1 -mindepth 1 -type d 2>/dev/null | wc -l | tr -d ' ')
sessions=$(find "$out/claude/projects" -name '*.jsonl' 2>/dev/null | wc -l | tr -d ' ')
bytes=$(du -sb "$out/claude/projects" 2>/dev/null | cut -f1)
[ -n "$bytes" ] || bytes='?'
printf 'claude: %%s projects, %%s sessions, %%s bytes\n' "$projects" "$sessions" "$bytes" >&2
`, q)
}
