# tmux-sync — bash prototype (v0)
#
# This is the working bash implementation that informed the Go rewrite.
# Extracted verbatim from ChristopherScot/homelab @ 4f260c3
#   (scripts/claude-pods.sh, the tmux-sync section).
# Kept here as a reference impl + an immediately-usable fallback while the
# Go binary is built out.  Source this file from your shell rc to use it.
#
# ── TMUX-SYNC: take a remote session offline and check it back in ────────────
# Carry a remote machine's working dir onto this laptop so you can keep editing
# with NO connectivity (plane / cruise / dead wifi), then hand your edits back
# when you reconnect. The remote stays the canonical copy; you've borrowed it.
#
#   tmux-sync checkout [target] [dir]   remote -> laptop, then OPEN it locally
#   tmux-sync checkin  [target] [dir]   laptop -> remote (run when back online)
#
# Transport-agnostic: it reaches the remote through whatever command prefix is
# in $TMUX_SYNC_EXEC (default = this homelab's k8s double-hop). Each call's
# script is fed over STDIN to a bare remote `bash`, so NOTHING needs quoting and
# it works through any transport — `target` fills the {N} slot:
#   k8s (default):  'ssh -o ConnectTimeout=8 homelab kubectl exec -i -n claude-pods claude-session-{N} --'
#   k8s, no hop:    'kubectl exec -i -n claude-pods claude-session-{N} --'
#   plain ssh:      'ssh {N}'             (target = a host alias in ~/.ssh/config)
#   docker:         'docker exec -i {N}'  (target = a container name)
# How checkout OPENS the local copy is also pluggable via $TMUX_SYNC_OPEN
# (default = a container on the same pod image: identical nvim/LazyVim + tmux).
#
#   checkout, online, in order:
#     1. :wa every open nvim buffer on the remote (mid-edit work -> disk).
#     2. Snapshot the EXACT working tree (tracked + untracked + uncommitted)
#        onto a `sync-wip` ref WITHOUT moving the real branch.
#     3. Stream it to ~/.claude-pod-sync/<target>-<slug>/repo and OPEN it.
#   Re-run checkout while genuinely offline -> it skips the remote and just
#   REOPENS the local session (getting back in stays one command).
#   checkin: :wa the local buffers, commit offline edits, bundle them back,
#     STASH the remote's current state (recoverable via `git stash list`), and
#     check the edits out onto `sync-wip` in the remote dir.

# Transport: a command PREFIX that runs an appended command on the target, with
# {N} replaced by `target`. Override for your environment (examples above).
: "${TMUX_SYNC_EXEC:=ssh -o ConnectTimeout=8 homelab kubectl exec -i -n claude-pods claude-session-{N} --}"
# Local opener: a command run (with $REPO = local clone, $DIR = its remote path
# exported) to open the checkout. Empty = the built-in Docker pod-image session.
# Examples:  'cd "$REPO" && exec nvim .'   |   'code "$REPO"'
: "${TMUX_SYNC_OPEN:=}"
# Image + default remote base dir for the built-in opener / dir resolution.
: "${TMUX_SYNC_IMAGE:=ghcr.io/christopherscot/claude-pod:latest}"
: "${TMUX_SYNC_DIR_DEFAULT:=/workspace}"

# Expand the transport prefix for a given target.
_remote_pre() { printf '%s' "${TMUX_SYNC_EXEC//\{N\}/$1}"; }

# Run a script (arg $2) on target $1, exposing args $3.. as $1.. inside it. The
# script rides STDIN to a bare remote `bash` — no quoting, transport-agnostic;
# remote stdout (incl. a binary git bundle) passes straight back. `sh -c` splits
# the prefix into words portably across bash and zsh (zsh doesn't word-split
# unquoted expansions; this avoids that).
_sync_exec() {
  local n="$1" script="$2"; shift 2
  local pre setargs="set --" a
  pre=$(_remote_pre "$n")
  for a in "$@"; do setargs="$setargs '$a'"; done
  { printf '%s\n' "$setargs"; printf '%s' "$script"; } | sh -c "exec $pre bash"
}

# Copy a local file to a remote path (binary-safe, quoting-free via `tee`).
_sync_put() {
  local n="$1" src="$2" dst="$3" pre
  pre=$(_remote_pre "$n")
  sh -c "exec $pre tee '$dst'" < "$src" > /dev/null
}

# Resolve the dir argument to an absolute remote path. $1 = dir arg, $2 = base
# (default /workspace): empty arg -> the base's .primary-repo marker else base;
# relative arg -> under base. So checkout/checkin agree on the local slug.
_SYNC_RESOLVE='
d="$1"; base="$2"; [ -n "$base" ] || base=/workspace
if [ -z "$d" ]; then
  if [ -f "$base/.primary-repo" ]; then d=$(cat "$base/.primary-repo"); else d="$base"; fi
fi
case "$d" in /*) ;; *) d="$base/$d";; esac
printf "%s\n" "$d"
'

# Save open nvim buffers, then snapshot the working tree onto sync-wip.
_SYNC_SNAP='
set -e
dir="$1"
tmux list-panes -a -F "#{pane_id} #{pane_current_command}" 2>/dev/null \
  | awk "\$2==\"nvim\"{print \$1}" \
  | while read -r p; do tmux send-keys -t "$p" Escape; tmux send-keys -t "$p" ":wa" Enter; done || true
sleep 0.4
cd "$dir"
if ! git rev-parse --git-dir >/dev/null 2>&1; then
  git init -q; git add -A; git commit -qm "sync: initial snapshot" || true
fi
git add -A
tree=$(git write-tree)
if parent=$(git rev-parse -q --verify HEAD); then
  snap=$(git commit-tree "$tree" -p "$parent" -m "pod-sync wip")
else
  snap=$(git commit-tree "$tree" -m "pod-sync wip")
fi
git branch -f sync-wip "$snap"
git reset -q
echo "  snapshotted $dir -> sync-wip ($(git rev-parse --short sync-wip))" >&2
'

# Land the laptop edits back in the pod dir; park the pod prior state in a stash.
_SYNC_RESTORE='
set -e
dir="$1"; cd "$dir"
git rev-parse --git-dir >/dev/null 2>&1 || { echo "pod dir is not a git repo: $dir" >&2; exit 1; }
git stash push -u -m "pre-restore $(date -u +%FT%TZ)" >/dev/null 2>&1 || true
git checkout -q --detach 2>/dev/null || true   # free all branch refs for the fetch
git fetch -q /tmp/restore.bundle "+refs/heads/sync-wip:refs/heads/sync-wip"
git checkout -q sync-wip
rm -f /tmp/restore.bundle
echo "  restored laptop edits -> $dir (now on branch sync-wip)" >&2
echo "  pod prior state stashed (recover: git -C $dir stash list)" >&2
'

_tmuxsync_safe()      { printf '%s' "$1" | sed 's#[^A-Za-z0-9_.-]#-#g'; }
_tmuxsync_slug()      { printf '%s' "$1" | sed 's#^/##; s#/#-#g'; }
_tmuxsync_container() { printf 'claude-offline-%s-%s' "$(_tmuxsync_safe "$1")" "$2"; }

# Best-effort `:wa` of every nvim buffer in the LOCAL offline container (mirror
# of the pod-side save), so mid-edit work is on disk before we read it.
_tmuxsync_save_local() {
  local name; name=$(_tmuxsync_container "$1" "$2")
  command -v docker >/dev/null 2>&1 || return 0
  docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$name" || return 0
  docker exec "$name" bash -lc 'tmux list-panes -a -F "#{pane_id} #{pane_current_command}" 2>/dev/null \
    | awk "\$2==\"nvim\"{print \$1}" \
    | while read -r p; do tmux send-keys -t "$p" Escape; tmux send-keys -t "$p" ":wa" Enter; done; sleep 0.4' \
    >/dev/null 2>&1 || true
}

# Open (or reopen) the local checkout. If $TMUX_SYNC_OPEN is set, run that (with
# $REPO + $DIR exported) and stop. Otherwise the built-in default: a container
# on the SAME pod image — identical nvim/LazyVim + tmux, minus the claude pane.
# A long-lived keepalive container holds the tmux server so detach/reattach
# survives (mirrors the pod); the repo is bind-mounted at its remote path so
# absolute paths line up; a named home volume caches LazyVim plugins (warmed on
# the first, online open).
_tmuxsync_open() {
  local n="$1" slug="$2" repo="$3" dir="$4"
  if [ -n "$TMUX_SYNC_OPEN" ]; then
    REPO="$repo" DIR="$dir" sh -c "$TMUX_SYNC_OPEN"
    return $?
  fi
  command -v docker >/dev/null 2>&1 || {
    echo "tmux-sync: no \$TMUX_SYNC_OPEN set and docker not found." >&2
    echo "           set TMUX_SYNC_OPEN (e.g. 'cd \"\$REPO\" && exec nvim .') or install Docker." >&2
    echo "           your edits are in $repo regardless." >&2
    return 1
  }
  local img name; img="$TMUX_SYNC_IMAGE"; name=$(_tmuxsync_container "$n" "$slug")
  if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$name"; then
    docker rm -f "$name" >/dev/null 2>&1 || true
    if ! docker run -d --name "$name" -v "$repo":"$dir" \
           -v claude-offline-home:/home/node -w "$dir" "$img" sleep infinity >/dev/null 2>&1; then
      echo "tmux-sync: couldn't start the container. If it's an auth/pull error, run while online:" >&2
      echo "  gh auth token | docker login ghcr.io -u <github-user> --password-stdin" >&2
      return 1
    fi
    # One-time home seed (the named volume usually auto-populates from the image)
    # + LazyVim plugin warm. Needs network, so it only fully works on the first,
    # online open; offline reopens reuse the cached plugins.
    docker exec "$name" bash -lc '
      [ -e "$HOME/.zshrc" ] || cp -a /opt/home-skel/. "$HOME/" 2>/dev/null || true
      [ -d "$HOME/.local/share/nvim/lazy/lazy.nvim" ] || nvim --headless "+Lazy! sync" +qa >/dev/null 2>&1 || true
    ' >/dev/null 2>&1 || true
  fi
  # nvim | shell layout (no claude pane); idempotent — reattaches if it exists.
  local boot='
cd "$1"; SES=offline
tmux has-session -t "$SES" 2>/dev/null || {
  tmux new-session -d -s "$SES" -n code -c "$1" "nvim .; exec zsh"
  tmux split-window -h -p 40 -t "$SES:code" -c "$1"
  tmux select-pane -t "$SES:code.1"
}
exec tmux attach -t "$SES"'
  local b64; b64=$(printf '%s' "$boot" | base64 | tr -d '\n')
  docker exec -it "$name" bash -lc "printf %s ${b64} | base64 -d | bash -s -- '$dir'"
}

tmux-sync() {
  local cmd="${1:-}"; [ $# -gt 0 ] && shift
  case "$cmd" in
    checkout) _tmuxsync_checkout "$@" ;;
    checkin)  _tmuxsync_checkin  "$@" ;;
    *) echo "usage: tmux-sync <checkout|checkin> [target] [dir]" >&2; return 2 ;;
  esac
}

_tmuxsync_checkout() {
  local n="${1:-0}" dirarg="${2:-}" dir slug local_dir
  local meta="$HOME/.claude-pod-sync/last-$(_tmuxsync_safe "$n")"

  if dir=$(_sync_exec "$n" "$_SYNC_RESOLVE" "$dirarg" "$TMUX_SYNC_DIR_DEFAULT" 2>/dev/null) && [ -n "$dir" ]; then
    # ── ONLINE: snapshot the remote, refresh the local clone ──
    slug=$(_tmuxsync_slug "$dir")
    local_dir="$HOME/.claude-pod-sync/$(_tmuxsync_safe "$n")-${slug}"
    # Guard: never clobber un-checked-in local edits on a re-checkout.
    if [ -d "$local_dir/repo/.git" ]; then
      _tmuxsync_save_local "$n" "$slug"
      if [ -n "$(git -C "$local_dir/repo" status --porcelain 2>/dev/null)" ]; then
        echo "tmux-sync checkout: local session for ${n}:${dir} has un-checked-in edits." >&2
        echo "  run 'tmux-sync checkin ${n}' first, or 'rm -rf $local_dir' to discard them." >&2
        return 1
      fi
    fi
    echo "tmux-sync checkout: ${n}:${dir} (online)" >&2
    echo "  saving open nvim buffers + snapshotting…" >&2
    _sync_exec "$n" "$_SYNC_SNAP" "$dir" || return 1
    mkdir -p "$local_dir"
    echo "  streaming bundle → ${local_dir}/repo …" >&2
    _sync_exec "$n" 'cd "$1" && git bundle create - --all' "$dir" > "$local_dir/latest.bundle" || return 1
    [ -d "$local_dir/repo/.git" ] || git clone -q "$local_dir/latest.bundle" "$local_dir/repo"
    git -C "$local_dir/repo" checkout -q --detach 2>/dev/null || true  # free refs for the fetch
    git -C "$local_dir/repo" fetch -q "$local_dir/latest.bundle" '+refs/heads/*:refs/heads/*'
    git -C "$local_dir/repo" checkout -q -B sync-wip sync-wip
    mkdir -p "$HOME/.claude-pod-sync"; printf '%s\n' "$dir" > "$meta"
  else
    # ── OFFLINE: skip the remote, just reopen the last checkout for this target ──
    case "$dirarg" in /*) dir="$dirarg" ;; esac
    [ -n "${dir:-}" ] || { [ -f "$meta" ] && dir=$(cat "$meta"); }
    [ -n "${dir:-}" ] || { echo "tmux-sync checkout: ${n} unreachable and nothing checked out yet." >&2; return 1; }
    slug=$(_tmuxsync_slug "$dir")
    local_dir="$HOME/.claude-pod-sync/$(_tmuxsync_safe "$n")-${slug}"
    [ -d "$local_dir/repo/.git" ] || { echo "tmux-sync checkout: offline, and no local checkout at $local_dir." >&2; return 1; }
    echo "tmux-sync checkout: ${n} unreachable — reopening local session (offline)" >&2
  fi

  _tmuxsync_open "$n" "$slug" "$local_dir/repo" "$dir"
}

_tmuxsync_checkin() {
  local n="${1:-0}" dirarg="${2:-}" dir slug local_dir repo

  dir=$(_sync_exec "$n" "$_SYNC_RESOLVE" "$dirarg" "$TMUX_SYNC_DIR_DEFAULT") \
    || { echo "tmux-sync checkin: ${n} unreachable — connect, then retry." >&2; return 1; }
  slug=$(_tmuxsync_slug "$dir")
  local_dir="$HOME/.claude-pod-sync/$(_tmuxsync_safe "$n")-${slug}"
  repo="$local_dir/repo"
  [ -d "$repo/.git" ] || { echo "tmux-sync checkin: nothing checked out for ${n}:${dir} ($repo)." >&2; return 1; }

  _tmuxsync_save_local "$n" "$slug"   # capture local mid-edit buffers first
  echo "tmux-sync checkin: ${n}:${dir}" >&2
  git -C "$repo" add -A
  git -C "$repo" commit -q -m "offline edits $(date -u +%FT%TZ)" 2>/dev/null \
    || echo "  (no new offline edits to commit)" >&2
  git -C "$repo" bundle create "$local_dir/restore.bundle" sync-wip || return 1
  echo "  sending edits up the channel…" >&2
  _sync_put "$n" "$local_dir/restore.bundle" /tmp/restore.bundle || return 1
  _sync_exec "$n" "$_SYNC_RESTORE" "$dir" || return 1
  echo "✓ checked in to ${n}." >&2
}
