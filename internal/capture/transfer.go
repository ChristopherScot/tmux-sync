package capture

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/christopherscot/tmux-sync/internal/driver"
)

// Transfer streams the remote bundle directory down to localDir by running
// `tar czf -` on the remote and piping its stdout into an in-process Go untar.
// The remote tar is invoked via Driver.Exec, so the same code path works for
// any backend (k8s pod, ssh+docker container, …).
//
// localDir is created if missing. Existing files are overwritten.
//
// Returns the absolute local path that received the bundle (i.e., localDir
// resolved + the base name of remoteBundleDir).
func Transfer(ctx context.Context, d driver.Driver, remoteBundleDir, localDir string, stderr io.Writer) (string, error) {
	if remoteBundleDir == "" || localDir == "" {
		return "", fmt.Errorf("Transfer: remoteBundleDir and localDir are required")
	}
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return "", fmt.Errorf("Transfer: mkdir local: %w", err)
	}

	parent := filepath.Dir(remoteBundleDir)
	name := filepath.Base(remoteBundleDir)
	script := fmt.Sprintf("cd %s && tar czf - %s",
		shellQuote(parent), shellQuote(name))

	// Stream remote tar -> local untar via an in-memory pipe.
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		err := d.Exec(ctx, []string{"sh", "-c", script}, nil, pw, stderr)
		// CloseWithError propagates any exec failure to the reader so untar
		// surfaces it rather than seeing a truncated stream as a parse error.
		_ = pw.CloseWithError(err)
		errCh <- err
	}()

	untarErr := untar(pr, localDir)
	// Drain the goroutine before reporting.
	execErr := <-errCh
	if execErr != nil {
		return "", fmt.Errorf("remote tar: %w", execErr)
	}
	if untarErr != nil {
		return "", fmt.Errorf("local untar: %w", untarErr)
	}

	abs, err := filepath.Abs(filepath.Join(localDir, name))
	if err != nil {
		return filepath.Join(localDir, name), nil
	}
	return abs, nil
}

// shellQuote single-quotes a value for safe inclusion in a POSIX sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, `'`, `'\''`) + "'"
}

// untar extracts a gzip'd tar stream into dest. Symlinks are honored;
// path-traversal entries (`..`) are rejected.
func untar(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	destClean := filepath.Clean(dest)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// Defense in depth: reject any path that escapes dest.
		target := filepath.Join(dest, hdr.Name)
		rel, err := filepath.Rel(destClean, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("untar: refusing entry outside destination: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// Other types (block/char devices, FIFOs, hard links) aren't
			// produced by the kinds of files this tool moves; silently skip.
		}
	}
}
