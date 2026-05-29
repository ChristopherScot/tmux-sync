# tmux-sync

Port a live remote `tmux` session — layout, scrollback, every running `nvim`'s
open files & splits, the `claude` conversation, and the working files — to a
container on your laptop, work **offline**, then check it back in and resume on
the remote.

> **Status: alpha, *working*.** Full checkout + checkin round trip is wired
> and verified end-to-end against a live `claude-pod` (both `k8s` and
> `ssh-kubectl` transports). See [`SPEC.md`](./SPEC.md) for the design and
> the per-feature [implementation status table](./SPEC.md#implementation-status).
> The original bash prototype is preserved at
> [`reference/tmux-sync.sh`](./reference/tmux-sync.sh) for context.

## Why

You're driving a `claude-pod` over the network and you need to go offline (a
plane, a train, dead wifi) without losing your session. `tmux-sync checkout`
captures the whole frame and pulls it down; you keep working; `tmux-sync
checkin` pushes it back and you resume on the remote.

## Install

Until a release post-`v0.0.1` is tagged, build from source on the branch with
the working flow:

```sh
go install github.com/christopherscot/tmux-sync@feat/checkout-capture
# make sure $(go env GOBIN || echo $(go env GOPATH)/bin) is on PATH
tmux-sync version
```

After the next tag, pre-built binaries land on the
[releases page](https://github.com/ChristopherScot/tmux-sync/releases) for
`darwin/{arm64,amd64}` and `linux/{amd64,arm64}`.

The same binary is installed into the `claude-pod` container image so the
remote half is always the matching version.

**Auto-update.** Once installed, `tmux-sync` checks GitHub Releases at most
once every 6 hours and atomically replaces itself in place when a newer tag
is published; the new version takes effect on the next invocation. Opt out
with `TMUX_SYNC_NO_UPDATE=1`. Inside the pod the binary is root-owned, so
auto-update is naturally a no-op there — version-pinned by the image as
intended.

## Usage

```sh
tmux-sync checkout --from homelab    # pod  → laptop, reconstruct + attach cmd
# ...work offline (edits in ~/.tmux-sync/workspaces/<endpoint>/...)
tmux-sync checkin  --to   homelab    # laptop → pod, restored on `sync-wip` ref
tmux-sync status                     # which endpoints are checked out, what's dirty
tmux-sync list     --from homelab    # remote `tmux ls` via the Driver
```

Endpoints live in `~/.config/tmux-sync/config.yaml`. Quick example with both
direct-kubeconfig and SSH-hop transports:

```yaml
endpoints:
  homelab:                # direct kubectl (your laptop has a working kubeconfig)
    kind: k8s
    context: homelab
    namespace: claude-pods
    pod: claude-session-0
  homelab-ssh:            # SSH hop (kubectl runs on a cluster node)
    kind: ssh-kubectl
    host: homelab
    namespace: claude-pods
    pod: claude-session-0
    ssh_args: ["-o", "ConnectTimeout=8"]
```

See [`SPEC.md#config`](./SPEC.md#config) for the full set of endpoint kinds.

## Design — short version

- **Reconstruct, don't migrate** — capture state, relaunch processes (no CRIU).
- **Disk is the source of truth** — every `nvim` is `:wall`'d + `:mksession`'d
  before snapshot, so nothing important lives only in memory.
- **Backend-agnostic** — one `Exec` primitive; k8s, GCP VM, and the laptop are
  all just endpoints behind a `Driver`.
- **Full session restore is required, not optional** — `tmux-resurrect` for
  layout + scrollback + which command was running, `mksession` for nvim
  interior, `claude --resume` for the conversation.

Full details in [`SPEC.md`](./SPEC.md).

## License

[MIT](./LICENSE).
