// Self-updater for the tmux-sync CLI.
//
// On each non-diagnostic command run, maybeSelfUpdate() checks the GitHub
// Releases API for ChristopherScot/tmux-sync at most once every selfUpdateTTL
// (state cached as the mtime of $XDG_CACHE_HOME/tmux-sync/last-update-check).
// If a newer tag is published, it downloads the matching archive, extracts the
// `tmux-sync` binary, and atomically replaces its own file via os.Rename.
// rename(2) succeeds even while we're running — the running process keeps the
// old inode open; the new file gets the path. The next invocation uses the
// new version.
//
// Silently skipped when:
//   - $TMUX_SYNC_NO_UPDATE is set
//   - this is a dev build (version == "dev")
//   - the binary file isn't writable by the current user
//     (the normal case inside the pod: /usr/local/bin/tmux-sync is root-owned,
//     so auto-update is a no-op there; the pod stays image-pinned while the
//     laptop tracks releases)
//
// All errors are swallowed (best-effort). Failure never blocks the user's
// command.

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	selfUpdateRepo = "ChristopherScot/tmux-sync"
	selfUpdateTTL  = 6 * time.Hour
	httpTimeout    = 4 * time.Second
	dlTimeout      = 60 * time.Second
)

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}
type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

func maybeSelfUpdate() {
	if os.Getenv("TMUX_SYNC_NO_UPDATE") != "" {
		return
	}
	if version == "dev" || version == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if !writable(exe) {
		return
	}
	if recentlyChecked() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	rel, err := fetchLatestRelease(ctx)
	touchCheck() // mark "checked" even on failure, to avoid hammering on outages
	if err != nil || rel == nil {
		return
	}
	if sameVersion(rel.TagName, version) {
		return
	}

	wantedName := assetName(rel.TagName)
	var dlURL string
	for _, a := range rel.Assets {
		if a.Name == wantedName {
			dlURL = a.URL
			break
		}
	}
	if dlURL == "" {
		return
	}

	dlCtx, dlCancel := context.WithTimeout(context.Background(), dlTimeout)
	defer dlCancel()
	if err := downloadAndReplace(dlCtx, dlURL, exe); err != nil {
		return
	}
	fmt.Fprintf(os.Stderr,
		"tmux-sync: updated to %s (was %s) — next run uses the new version\n",
		rel.TagName, version)
}

// writable reports whether path is openable for writing by the current user.
func writable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func cacheFile() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "tmux-sync", "last-update-check")
}

func recentlyChecked() bool {
	p := cacheFile()
	if p == "" {
		return false
	}
	s, err := os.Stat(p)
	if err != nil {
		return false
	}
	return time.Since(s.ModTime()) < selfUpdateTTL
}

func touchCheck() {
	p := cacheFile()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	if f, err := os.Create(p); err == nil {
		_ = f.Close()
	}
}

// sameVersion compares two tag strings, tolerating an optional `v` prefix on
// either side (the GitHub API returns `v0.1.0`; GoReleaser's default ldflag
// embeds `0.1.0`).
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

func fetchLatestRelease(ctx context.Context) (*ghRelease, error) {
	url := "https://api.github.com/repos/" + selfUpdateRepo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tmux-sync/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api: %s", resp.Status)
	}
	var r ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// assetName builds the GoReleaser-pattern archive filename for this OS/arch.
// Must match .goreleaser.yml's name_template.
func assetName(tag string) string {
	osPart := runtime.GOOS
	if osPart == "darwin" {
		osPart = "macOS"
	}
	return fmt.Sprintf("tmux-sync_%s_%s_%s.tar.gz",
		strings.TrimPrefix(tag, "v"), osPart, runtime.GOARCH)
}

// downloadAndReplace fetches the .tar.gz, extracts the `tmux-sync` binary into
// a temp file in the same directory as `exe`, chmods +x, and atomically
// renames it over `exe`. rename(2) works on Linux/macOS even while the binary
// is running.
func downloadAndReplace(ctx context.Context, url, exe string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "tmux-sync/"+version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".tmux-sync.update.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	var found bool
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			return err
		}
		if filepath.Base(hdr.Name) != "tmux-sync" {
			continue
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			cleanup()
			return err
		}
		found = true
		break
	}
	if !found {
		cleanup()
		return fmt.Errorf("archive missing tmux-sync binary")
	}
	if err := tmp.Chmod(0o755); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, exe); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
