# tmux-sync

Port a live remote `tmux` session — layout, scrollback, every running `nvim`'s
open files & splits, the `claude` conversation, and the working files — to a
container on your laptop, work **offline**, then check it back in and resume on
the remote.

> **Status: alpha — CLI is a skeleton.** See [`SPEC.md`](./SPEC.md) for the
> design. The working bash prototype that informed it lives in
> [`reference/tmux-sync.sh`](./reference/tmux-sync.sh) — sourceable directly if
> you want a usable v0 while the Go binary lands. No tagged releases yet; once
> tagged, binaries appear on the
> [releases page](https://github.com/ChristopherScot/tmux-sync/releases).

## Why

You're driving a `claude-pod` over the network and you need to go offline (a
plane, a train, dead wifi) without losing your session. `tmux-sync checkout`
captures the whole frame and pulls it down; you keep working; `tmux-sync
checkin` pushes it back and you resume on the remote.

## Install

Pre-built binaries are published with each release for `darwin/{arm64,amd64}`
and `linux/{amd64,arm64}`. Pick the tarball from the
[releases page](https://github.com/ChristopherScot/tmux-sync/releases), extract,
and drop `tmux-sync` somewhere on your `PATH`.

The same binary is installed into the `claude-pod` container image so the
remote half is always the matching version.

## Usage

```
tmux-sync checkout --from homelab
# ...work offline...
tmux-sync checkin  --to   homelab
```

Endpoints (k8s pod, GCP VM container, the laptop) live in
`~/.config/tmux-sync/config.yaml`. See [`SPEC.md`](./SPEC.md#config).

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
