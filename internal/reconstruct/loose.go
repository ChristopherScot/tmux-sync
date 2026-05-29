package reconstruct

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// RestoreLooseFiles copies the bundled loose/ tree into workspaceRoot
// alongside the restored repo clones. Skip silently when there's no loose/
// dir in the bundle (the most common case — most pods only contain repos).
//
// We shell out to `cp -a` rather than walking in Go because cp -a preserves
// modes, ownership where possible, and (importantly) symlinks correctly,
// and we already depend on a POSIX environment for the laptop side.
func RestoreLooseFiles(ctx context.Context, bundleDir, workspaceRoot string, stderr io.Writer) error {
	looseDir := filepath.Join(bundleDir, "loose")
	info, err := os.Stat(looseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	// Empty? Nothing to do.
	entries, err := os.ReadDir(looseDir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}

	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		return fmt.Errorf("RestoreLooseFiles: mkdir workspace: %w", err)
	}

	cmd := exec.CommandContext(ctx, "cp", "-a", looseDir+"/.", workspaceRoot+"/")
	cmd.Stderr = stderr
	cmd.Stdout = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("RestoreLooseFiles: cp -a: %w", err)
	}
	fmt.Fprintf(stderr, "restore-loose-files: %d entries restored into %s\n", len(entries), workspaceRoot)
	return nil
}
