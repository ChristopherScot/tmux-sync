package capture

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/christopherscot/tmux-sync/internal/driver"
)

// UploadBundle streams localBundleDir up to the remote, extracting it into
// remoteParent/<basename> via tar-over-Exec. Returns the absolute remote
// path of the unpacked bundle directory.
//
// Symmetric to Transfer (the checkout download), reversed direction. Uses
// Go's archive/tar + gzip for the local side (pure stdlib — no `tar` binary
// dependency on the laptop), piped into the Driver's stdin which then runs
// `sh -c "tar xzf - -C <parent>"` on the remote.
func UploadBundle(ctx context.Context, d driver.Driver, localBundleDir, remoteParent string, stderr io.Writer) (string, error) {
	if localBundleDir == "" || remoteParent == "" {
		return "", fmt.Errorf("UploadBundle: localBundleDir and remoteParent are required")
	}
	info, err := os.Stat(localBundleDir)
	if err != nil {
		return "", fmt.Errorf("UploadBundle: stat local: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("UploadBundle: %s is not a directory", localBundleDir)
	}

	name := filepath.Base(localBundleDir)

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		err := tarTree(localBundleDir, name, pw)
		_ = pw.CloseWithError(err)
		errCh <- err
	}()

	script := fmt.Sprintf("mkdir -p %s && tar xzf - -C %s",
		shellQuote(remoteParent), shellQuote(remoteParent))
	execErr := d.Exec(ctx, []string{"sh", "-c", script}, pr, io.Discard, stderr)
	tarErr := <-errCh

	if execErr != nil {
		return "", fmt.Errorf("remote untar: %w", execErr)
	}
	if tarErr != nil {
		return "", fmt.Errorf("local tar: %w", tarErr)
	}
	return path.Join(remoteParent, name), nil
}

// tarTree gzip-tar's the directory tree rooted at src into w, with the
// archive entries prefixed by `name` (so the receiver's `tar xzf -` produces
// `<remoteParent>/<name>/...`). Honors symlinks; skips devices/FIFOs/hard
// links the way Transfer's untar does for symmetry.
func tarTree(src, name string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.WalkDir(src, func(p string, dent fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := dent.Info()
		if err != nil {
			return err
		}

		// Skip irregular files we don't ship (devices etc.).
		mode := info.Mode()
		switch {
		case mode&os.ModeDevice != 0,
			mode&os.ModeCharDevice != 0,
			mode&os.ModeNamedPipe != 0,
			mode&os.ModeSocket != 0:
			return nil
		}

		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		archivePath := name
		if rel != "." {
			archivePath = filepath.Join(name, rel)
		}

		var linkname string
		if mode&os.ModeSymlink != 0 {
			linkname, err = os.Readlink(p)
			if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, linkname)
		if err != nil {
			return err
		}
		hdr.Name = archivePath
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		if mode.IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			_ = f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
}
