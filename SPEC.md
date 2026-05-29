# tmux-sync — design spec

**Status:** alpha, **working**. Checkout + checkin round trip is implemented
and verified end-to-end against a live `claude-pod` (k8s + ssh-kubectl
transports). See [Implementation status](#implementation-status) below for
the per-feature breakdown.
**Goal:** Port a live remote `tmux` session — layout, scrollback, every running
`nvim`'s open files & splits, the `claude` conversation, and the working files —
to a container on your laptop, let you work **offline**, then check it back in
and resume on the remote. Works whether the remote is a **k8s pod** or a
**container on a GCP VM**.

> The bash prototype in [`reference/tmux-sync.sh`](./reference/tmux-sync.sh) is
> the working v0 that informed this design. Its **transport-agnostic mechanism**
> (a command-prefix template + STDIN-streamed scripts) and **`sync-wip`
> shadow-commit snapshot** are kept verbatim. The Go binary supersedes it by
> adding full session restore (resurrect + per-nvim `:mksession` + claude
> `--resume`) and shipping as a single distributable binary.

---

## Decisions (locked)

1. **Reconstruct, don't migrate.** We do NOT move live process memory (CRIU is
   Linux-only, same-arch-only, and the laptop is `arm64` while the pod is
   `amd64`). We capture *state* and re-launch equivalent processes.
2. **Disk is the source of truth.** Before snapshotting, every remote `nvim` is
   told to `:wall` (write all buffers) and `:mksession` (record splits/buffers)
   automatically — so nothing of value lives only in memory.
3. **Full session restore is *required* in the MVP**, not opt-in.
   `tmux-resurrect` captures the whole frame (windows/panes/sizes/cwd/command/
   scrollback). A fixed-layout fallback is a non-goal.
4. **Serial handoff.** No locks, no merge engine, no continuous daemon. If you
   touch both sides, last-writer-wins; the divergence shows up in `git status`.
5. **Backend-agnostic via one primitive.** Every remote op is *"exec a command
   inside the target container, streaming stdin/stdout."* k8s, GCP VM, and the
   laptop are all just **endpoints** behind a `Driver`.

### Non-goals

- Live process migration (CRIU / byte-identical running processes).
- True concurrent multi-writer editing.
- Distributed locking, lease reclamation, conflict merging as a core feature.

---

## Concepts

- **Endpoint** — a place a session can live: a k8s pod, a container on a GCP VM,
  or a local container on the laptop. Described in config; resolved by a driver.
- **Driver** — adapter that knows how to `Exec` inside an endpoint's container.
- **Session bundle** — the self-contained artifact moved between endpoints:
  tmux-resurrect save + per-nvim `:mksession` files + claude transcript +
  per-repo `git bundle`s + a tar of loose files + a small manifest.
- **checkout** = endpoint → laptop, reconstruct.
- **checkin**  = laptop → endpoint, resume.

---

## The Driver interface

The entire protocol sits on one primitive:

```go
type Driver interface {
    // Exec runs argv inside the target container, wiring streams.
    // tar-over-Exec moves files; everything else is also just Exec.
    Exec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error
    // Resolve finds the concrete container for a named endpoint.
    Resolve(ctx context.Context, name string) (Target, error)
}
```

Three implementations, each thin:

| Backend | `Exec` is | Discovery | Status |
|---|---|---|---|
| **k8s**         | `kubectl exec -n <ns> <pod> -i -- argv`        | list pods by label   | ✅ implemented |
| **ssh-kubectl** | `ssh <host> kubectl exec -i -n <ns> <pod> -- argv` (SSH-hop for laptops without direct kubeconfig) | as above on the remote | ✅ implemented |
| **local**       | `docker exec -i <ctr> argv` (for local checkin nvim flush) | `docker ps`         | ✅ implemented |
| **plain local** | run `argv[0]` on the laptop directly (for laptop-side git bundling on checkin) | n/a | ✅ implemented |
| **ssh-docker**  | `ssh <host> docker exec -i <ctr> argv` (GCP VM)        | `docker ps` over ssh | ⏳ stub — errors with a clear message |

Because file transfer is **tar streamed over `Exec`** (not `kubectl cp`), one
code path moves files regardless of backend.

### Transport-agnostic via STDIN scripts

For multi-step remote work, build a script in-process and stream it on stdin
to a bare remote `bash` invoked through `Exec`. No shell quoting, no per-driver
escaping; works through any transport that can stream stdin/stdout. (Same
mechanism the v0 bash prototype used via `$TMUX_SYNC_EXEC`.)

### Config

```yaml
endpoints:
  # Direct kubectl context (your laptop must have a working kubeconfig).
  homelab:
    kind: k8s
    context: homelab
    namespace: claude-pods
    pod: claude-session-0
  # SSH-hop transport: kubectl runs on the SSH host, not on the laptop.
  # Matches the bash prototype's default $TMUX_SYNC_EXEC.
  homelab-via-ssh:
    kind: ssh-kubectl
    host: homelab                      # alias from ~/.ssh/config
    namespace: claude-pods
    pod: claude-session-0
    ssh_args: ["-o", "ConnectTimeout=8"]
  # GCP VM (planned).
  gcp: { kind: ssh-docker, host: claude-vm.tailnet.ts.net, container: claude-chris }
```

`tmux-sync checkout --from homelab` vs `--from gcp` is the only user-visible
difference between backends.

---

## Session bundle format

```
bundle/
  manifest.json            # source endpoint, session name, image digest, timestamp
  tmux/resurrect.txt       # tmux-resurrect save (layout/cwd/command/scrollback)
  nvim/<pane>.vim          # :mksession per live nvim
  claude/                  # ~/.claude/projects/... (transcript for claude --resume)
  repos/<name>.bundle      # git bundle per repo under /workspace
  files.tar                # loose (non-repo) files + curated $HOME paths
```

- **Repos → `git bundle`** — self-contained, offline-portable, and gives a real
  3-way `git merge` / `merge-base` if you ever need to reconcile divergence.
- **Loose files → tar** — simple; serial handoff means no merge engine needed.
- **nvim content** rides in the repo bundles / file tar (already on disk thanks
  to `:wall`); the `.vim` mksession files record *which files/splits* to reopen.
- **tmux frame → resurrect** — the image already has tmux-resurrect installed
  with `@resurrect-capture-pane-contents 'on'`; the bundled file gives back
  windows/panes/sizes/cwd/running-command + scrollback.

---

## checkout flow (endpoint → laptop)

1. **Flush every editor** in the container via `Exec`:
   ```sh
   for s in $(find /tmp/nvim* /run/user/*/nvim.* -type s -name 'nvim.*' 2>/dev/null); do
     nvim --server "$s" --remote-expr \
       "execute('silent! wall | mksession! ~/.tmux-sync/'.fnamemodify('$s',':t').'.vim')" \
       2>/dev/null || true
   done
   ```
   Mode-independent (`--remote-expr`), tolerates dead sockets.
2. **Capture the tmux frame:** invoke tmux-resurrect's save via `tmux run-shell`;
   writes a single file (layout + scrollback included). Extend
   `@resurrect-processes` so `claude`, `lazygit`, etc. are also re-launched on
   restore.
3. **Capture the claude conversation:** archive `~/.claude/projects/<project>/`
   for each active claude pane; resume key is the session id.
4. **Snapshot files:** for each repo under `/workspace`, snapshot the dirty
   working tree onto a `sync-wip` ref via `git write-tree` + `git commit-tree`
   (no HEAD/branch movement, no staging), then `git bundle create -` it. Tar
   the rest + curated `$HOME` paths.
5. **Transfer** the bundle via tar-over-`Exec`.
6. **Reconstruct locally:** `docker run` the same image (multi-arch; `arm64` on
   Apple Silicon) with `/workspace` bind-mounted at the *same absolute path* so
   nvim sessions and resurrect resolve correctly; restore tmux-resurrect;
   per-pane re-launch via the resurrect process whitelist; nvim panes open with
   `nvim -S <session>`; claude panes start with `claude --resume <session-id>`.
   Attach.

You're now offline-capable.

## checkin flow (laptop → endpoint)

Symmetric: `silent! wall` the local nvims → resurrect-save the local frame →
snapshot → transfer back → on the remote, `git stash push -u` the prior state
(recoverable via `git stash list`), restore the laptop's `sync-wip` ref +
session files + resurrect file → resume.

Last-writer-wins on actual content collisions; `git status` surfaces them.

---

## Local runtime notes

- **Arch:** pod image is `linux/amd64`; Mac is `arm64`. Build a **multi-arch**
  image (`--platform linux/amd64,linux/arm64`) with the rootless buildkit
  already in the pod image, so the laptop runs natively.
- **Secrets/login:** the local container won't have k8s/Vault mounts. Carry
  `~/.git-credentials` in the synced `$HOME` (sensitive — handle with care) and
  either `/login` claude once locally or copy `~/.claude/.credentials.json`.
- **Image persistence gotchas** (must mirror, or the local container misbehaves):
  the image seeds `$HOME` from `/opt/home-skel` **once** (gated by the
  `~/.home-seeded` marker) and re-syncs `~/.tmux.conf` **every boot**. Mount
  the synced `$HOME` *with* its `.home-seeded` marker so the local container
  doesn't re-seed over it.
- **LazyVim plugin cache:** use a named volume for `~/.local/share/nvim/lazy`
  so plugins warm once online and reuse offline.

---

## CLI surface

```
tmux-sync checkout --from <endpoint> [--session <name>]   # pull + reconstruct locally
tmux-sync checkin  --to   <endpoint> [--session <name>]   # push back + resume
tmux-sync status                                          # what's checked out where
tmux-sync list     --from <endpoint>                      # sessions available
```

Cross-compiles to `darwin/{arm64,amd64}` and `linux/{amd64,arm64}` — same
binary on the laptop and inside the pod image.

---

## What you get vs. what you don't

| Restored | Not restored |
|---|---|
| Pane layout + sizes + window count | Live process memory |
| Per-pane cwd + which command was running | Variables in a python/node REPL |
| Pane scrollback content | Navigated state in k9s/lazygit/htop |
| nvim files open + splits + jumplist (`:mksession`) | Half-typed shell command lines |
| nvim buffer *content* (via `:wall`) | Unsaved scratch buffers without a filename |
| claude transcript (`claude --resume`) | claude's in-flight reasoning between turns |
| Shell cwd + history | |

The "not restored" column is fundamentally what process migration would give
you, and is off the table (see decision 1).

---

## Distribution

- **GitHub Releases** are the source of binaries. Tag `vX.Y.Z` → GoReleaser
  builds `darwin/{arm64,amd64}` and `linux/{amd64,arm64}` → uploads tarballs +
  `SHA256SUMS`.
- The **claude-pods Dockerfile** installs released `tmux-sync` like any other
  CLI tool (`ARG TMUX_SYNC_VERSION` + curl from the release).
- The **laptop** installs via a curl one-liner or (later) a Homebrew tap.

### Auto-update

On the laptop, `tmux-sync` checks GitHub Releases at most once every 6h
(cached via the mtime of `${XDG_CACHE_HOME:-~/.cache}/tmux-sync/last-update-check`)
and, when a newer tag is published, atomically replaces its own binary on
disk: `rename(2)` into place. The running process keeps the old inode; the
new binary is used on the next invocation. Implementation lives in
[`selfupdate.go`](./selfupdate.go) — pure stdlib, no third-party deps.

Skipped silently when:
- `$TMUX_SYNC_NO_UPDATE` is set,
- this is a dev build (`version == "dev"`), or
- the binary file isn't writable by the current user.

The writability gate is what makes this safe inside the pod:
`/usr/local/bin/tmux-sync` is root-owned, the pod runs as `node:1000`, so
auto-update is a no-op — the pod stays version-pinned by the image while the
laptop tracks `:latest`. Each run is throttled by an mtime cache so the
GitHub API is hit at most once per TTL even if you fire commands in a loop.

---

## Implementation status

What's wired and proven against a live `claude-pod`:

| Area | Status | Notes |
|---|---|---|
| `Driver` interface (the one primitive) | ✅ | `internal/driver/{driver,k8s,ssh_kubectl,docker,local}.go` |
| Config + endpoint resolution | ✅ | `~/.config/tmux-sync/config.yaml`; k8s, ssh-kubectl, local, docker, ssh-docker(stub) |
| Self-updater | ✅ | Throttled, writability-gated, stdlib only; no-op in the pod |
| **Checkout — capture (pod-side)** | | |
| · nvim flush + `:mksession` | ✅ | Mode-independent via `--remote-expr`; per-socket session files |
| · tmux-resurrect save | ✅ | Layout + cwd + per-pane command + scrollback (`pane_contents.tar.gz`) |
| · per-repo `git bundle` (shadow-commit) | ✅ | `GIT_INDEX_FILE` keeps user's real index untouched |
| · loose files | ✅ | Non-repo `/workspace` entries `cp -a`'d into `bundle/loose/` |
| **Checkout — transport** | ✅ | `tar czf -` over `Exec`, Go untar locally (no `tar` binary needed) |
| **Checkout — local reconstruct** | | |
| · restore repos → local clones | ✅ | Dirty-tree clobber guard; laptop-only branches survive |
| · loose files | ✅ | |
| · local container (`docker run`) | ✅ | Bind-mounts at the same absolute path; named home volume |
| · tmux-resurrect restore | ✅ | Triggered inside the container after bundle copy-in |
| · per-pane nvim `nvim -S` wiring | ⏳ | session files ship; sourcing per pane not yet wired into resurrect |
| · claude `--resume` | ⏳ | transcript not yet captured/restored |
| **Checkin (laptop → pod)** | | |
| · nvim flush in local container | ✅ | Via `DockerExec` driver — same script as checkout |
| · per-repo bundle locally | ✅ | Via `Local` driver — same `BundleRepos` script |
| · loose files | ✅ | |
| · upload + apply on pod | ✅ | `git stash push -u` saves pod's prior state (recoverable) |
| **CLI** | | |
| · `checkout` / `checkin` | ✅ | |
| · `status` | ✅ | endpoints + per-repo branch + dirty markers + container state |
| · `list --from <endpoint>` | ✅ | `tmux ls` on the remote via Driver |
| · `version` / `help` | ✅ | |

## Phasing

- **MVP** (largely complete): k8s + ssh-kubectl drivers, full session capture
  on the pod side, transport, full local reconstruct (minus per-pane nvim
  session sourcing and claude resume), full checkin round trip,
  `status`/`list`/`version`.
- **V1:** per-pane `nvim -S` on restore, claude `--resume`, GCP-VM
  (`ssh-docker`) driver, multi-arch auto-build wiring, Homebrew tap.
- **Maybe:** incremental re-sync (rsync/mutagen over `Exec`) if serial
  snapshots feel heavy; selector-based pod discovery for k8s endpoints.
