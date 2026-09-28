#!/usr/bin/env bash
# move-app-authority-to-prod.sh — move the real-work GitHub App authority into
# prod's App directories, once (#1583).
#
#   bash scripts/move-app-authority-to-prod.sh <app-state-dir> <app-credentials-dir>
#
# Real-work runs used to keep the App's authority in their own directories
# (FREESIDE_REAL_RUN_APP_STATE and FREESIDE_REAL_RUN_APP_CREDS). They now share
# prod's: <prod root>/daemon holds the authority state and <prod root>/credentials
# the credentials. The App has one authority, so the move is all or nothing and
# never merges two authorities.
#
# It holds prod's database lock (<prod root>/daemon/freeside.db.daemon.lock,
# the flock a running prod daemon holds) for the whole move, and stops,
# changing nothing, when:
#   - prod holds that lock, so it is running;
#   - $HOME and the passwd home name different prod roots;
#   - a production rig manifest, live or stale, exists under
#     ~/.freeside/rig-locks (passwd home): a run may be using the authority;
#   - <prod root>/daemon already holds App state, or <prod root>/credentials
#     exists and is not empty;
#   - a source holds anything it doesn't recognize (including a symlink), or
#     lacks the authority snapshot or a github-app registration (app.json and
#     app.pem).
#
# Otherwise it copies both sources into staging beside their targets with
# owner-only modes, then renames the state files and, last, the credentials
# directory into place. A failure or a signal (HUP, INT, TERM) before that last
# rename removes every copy it made; a signal after it keeps the complete move.
# Only then does it rename each source aside to <dir>.moved-to-prod-<UTC timestamp>, keeping it, so a stale environment
# variable fails loudly instead of naming a second authority. It never deletes a
# source.
#
# Stop prod first. Exit code: 0 on a complete move, 1 on a failure
# while moving, 2 when it refuses without changing anything.
set -euo pipefail

APP_MOVE_SCRIPT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")

# The state files the daemon's installation authority store owns
# (daemon/internal/publish/janitor_store.go).
app_state_file_recognized() {
  case $1 in
  installation-authority.json | installation-janitor-journal.json | \
    installation-janitor-journal.lock | installation-recovery-*.json) return 0 ;;
  esac
  return 1
}

# The credentials root's entries the keystore owns
# (daemon/internal/publish/keystore.go).
app_credentials_entry_recognized() {
  case $1 in
  github-app | github-app.quarantine) return 0 ;;
  esac
  return 1
}

# app_move_refuse <message>: prints a refusal and returns 2.
app_move_refuse() {
  echo "move-app-authority-to-prod: refusing: $1" >&2
  return 2
}

# app_move_check_sources <state-src> <creds-src>: returns 0 when both sources
# hold only recognized App authority entries, or refuses.
app_move_check_sources() {
  local state=$1 creds=$2 entry name link
  [[ -d "$state" && ! -L "$state" ]] || app_move_refuse "App state source \"$state\" is not a directory" || return
  [[ -d "$creds" && ! -L "$creds" ]] || app_move_refuse "App credentials source \"$creds\" is not a directory" || return
  [[ -f "$state/installation-authority.json" ]] ||
    app_move_refuse "App state source \"$state\" holds no installation-authority.json" || return
  # An empty github-app/ (say, its only record quarantined), or a record
  # missing its key, would move an authority no run can use.
  local registration found=0
  for registration in "$creds"/github-app/*/; do
    [[ -f "$registration/app.json" && -f "$registration/app.pem" ]] && found=1 && break
  done
  ((found)) ||
    app_move_refuse "App credentials source \"$creds\" holds no github-app registrations" || return
  for entry in "$state"/* "$state"/.[!.]* "$state"/..?*; do
    [[ -e "$entry" || -L "$entry" ]] || continue
    name=$(basename -- "$entry")
    if [[ -L "$entry" || ! -f "$entry" ]] || ! app_state_file_recognized "$name"; then
      app_move_refuse "App state source holds \"$name\", which is not App authority state; resolve it by hand" || return
    fi
  done
  for entry in "$creds"/* "$creds"/.[!.]* "$creds"/..?*; do
    [[ -e "$entry" || -L "$entry" ]] || continue
    name=$(basename -- "$entry")
    if [[ -L "$entry" || ! -d "$entry" ]] || ! app_credentials_entry_recognized "$name"; then
      app_move_refuse "App credentials source holds \"$name\", which the keystore doesn't own; resolve it by hand" || return
    fi
  done
  link=$(find "$creds" -type l -print -quit)
  [[ -z "$link" ]] || app_move_refuse "App credentials source holds a symlink \"$link\"" || return
}

# app_move_check_targets <prod-root> <lease-root>: returns 0 when prod holds no
# App state and no rig lease exists, or refuses.
app_move_check_targets() {
  local prod_root=$1 lease_root=$2 entry
  if [[ -e "$lease_root/production-rig.json" || -L "$lease_root/production-rig.json" ]]; then
    app_move_refuse "a production rig manifest exists at \"$lease_root/production-rig.json\"; wait for the run to end, or clear a stale lease with freesided rig recover" || return
  fi
  for entry in "$prod_root/daemon"/*; do
    [[ -e "$entry" || -L "$entry" ]] || continue
    if app_state_file_recognized "$(basename -- "$entry")"; then
      app_move_refuse "prod already holds App state \"$entry\"; the move never merges two authorities" || return
    fi
  done
  if [[ -e "$prod_root/credentials" || -L "$prod_root/credentials" ]]; then
    if [[ -L "$prod_root/credentials" || ! -d "$prod_root/credentials" ]] ||
      [[ -n "$(ls -A "$prod_root/credentials")" ]]; then
      app_move_refuse "prod already holds App credentials at \"$prod_root/credentials\"; the move never merges two authorities" || return
    fi
  fi
}

# app_move_with_prod_lock <prod-root> <function> [args...]: runs this script's
# <function> while holding prod's database lock, or refuses while prod holds
# it. The lock is the daemon's flock(2) lock; perl takes it and execs bash with
# the descriptor kept open, so it is held until the move exits.
app_move_with_prod_lock() {
  local lock=$1/daemon/freeside.db.daemon.lock
  shift
  [[ -d "$(dirname -- "$lock")" ]] || app_move_refuse "prod's daemon directory \"$(dirname -- "$lock")\" does not exist" || return
  (
    umask 077
    exec perl -MFcntl=:flock,F_SETFD -e '
      my $path = shift;
      open(my $fh, ">>", $path) or do { print STDERR "move-app-authority-to-prod: refusing: cannot open prod database lock \"$path\": $!\n"; exit 2 };
      flock($fh, LOCK_EX | LOCK_NB) or do { print STDERR "move-app-authority-to-prod: refusing: prod holds its database lock \"$path\"; stop prod first\n"; exit 2 };
      fcntl($fh, F_SETFD, 0) or exit 1;
      exec @ARGV or exit 1;
    ' "$lock" bash -c 'set -euo pipefail; source "$1"; shift; "$@"' _ "$APP_MOVE_SCRIPT" "$@"
  )
}

# move_app_authority <state-src> <creds-src> <prod-root> <lease-root> <stamp>:
# the whole move, with every root explicit so the suite can drive it.
move_app_authority() {
  local state=$1 creds=$2 prod_root=$3 lease_root=$4 stamp=$5
  local staging placed=() file name replaced_empty_credentials=0
  app_move_check_sources "$state" "$creds" || return
  app_move_check_targets "$prod_root" "$lease_root" || return

  umask 077
  mkdir -p "$prod_root/daemon"
  staging=$(mktemp -d "$prod_root/.app-authority-move.XXXXXX") || return 1
  # Removes every copy this run made and restores prod's empty credentials
  # directory; the sources are untouched until the end.
  app_move_rollback() {
    local placed_file
    for placed_file in ${placed[@]+"${placed[@]}"}; do
      rm -f -- "$placed_file"
    done
    rm -rf -- "$staging"
    if ((replaced_empty_credentials)) && [[ ! -e "$prod_root/credentials" ]]; then
      mkdir -m 700 -- "$prod_root/credentials" || true
    fi
    trap - HUP INT TERM
  }
  # A signal, including a closed terminal's HUP, must never split the
  # authority. The staged credentials leaving staging is the commit point, so
  # the handler reads it from the filesystem, not a flag set after the rename.
  # shellcheck disable=SC2329 # invoked by the trap below
  app_move_on_signal() {
    if [[ ! -e "$staging/credentials" && -d "$prod_root/credentials" ]]; then
      trap - HUP INT TERM
      rm -rf -- "$staging"
      echo "move-app-authority-to-prod: interrupted after the authority reached prod; rename \"$state\" and \"$creds\" aside by hand so nothing names them" >&2
      exit 130
    fi
    app_move_rollback
    echo "move-app-authority-to-prod: interrupted; nothing was moved" >&2
    exit 130
  }
  trap app_move_on_signal HUP INT TERM
  if ! mkdir "$staging/state" ||
    ! cp -Rp "$creds" "$staging/credentials" ||
    ! chmod -R go-rwx "$staging/credentials"; then
    app_move_rollback
    echo "move-app-authority-to-prod: staging the credentials failed; nothing was moved" >&2
    return 1
  fi
  for file in "$state"/*; do
    [[ -e "$file" ]] || continue
    if ! cp -p "$file" "$staging/state/" || ! chmod go-rwx "$staging/state/$(basename -- "$file")"; then
      app_move_rollback
      echo "move-app-authority-to-prod: staging \"$file\" failed; nothing was moved" >&2
      return 1
    fi
  done
  # Each rollback record is written before the change it undoes, so a signal
  # between the two still finds it; undoing a change that didn't happen is a
  # no-op.
  for file in "$staging/state"/*; do
    name=$(basename -- "$file")
    if [[ -e "$prod_root/daemon/$name" ]]; then
      app_move_rollback
      echo "move-app-authority-to-prod: placing \"$name\" failed; nothing was moved" >&2
      return 1
    fi
    placed+=("$prod_root/daemon/$name")
    if ! mv -- "$file" "$prod_root/daemon/$name"; then
      app_move_rollback
      echo "move-app-authority-to-prod: placing \"$name\" failed; nothing was moved" >&2
      return 1
    fi
  done
  # The credentials rename is the commit point: until it lands, prod holds an
  # authority with no credentials, which it refuses to use.
  if [[ -d "$prod_root/credentials" ]]; then
    replaced_empty_credentials=1
    if ! rmdir -- "$prod_root/credentials"; then
      app_move_rollback
      echo "move-app-authority-to-prod: prod's empty credentials directory could not be replaced; nothing was moved" >&2
      return 1
    fi
  fi
  if ! mv -- "$staging/credentials" "$prod_root/credentials"; then
    app_move_rollback
    echo "move-app-authority-to-prod: placing the credentials failed; nothing was moved" >&2
    return 1
  fi
  trap - HUP INT TERM
  rm -rf -- "$staging"

  if ! mv -- "$state" "$state.moved-to-prod-$stamp" || ! mv -- "$creds" "$creds.moved-to-prod-$stamp"; then
    echo "move-app-authority-to-prod: the authority is in prod, but a source could not be renamed aside; rename \"$state\" and \"$creds\" aside by hand so nothing names them" >&2
    return 1
  fi
  echo "moved the App authority into \"$prod_root\"; the sources are kept as *.moved-to-prod-$stamp" >&2
  echo "set FREESIDE_REAL_RUN_APP_STATE=\"$prod_root/daemon\" and FREESIDE_REAL_RUN_APP_CREDS=\"$prod_root/credentials\"" >&2
}

main() {
  if (($# != 2)); then
    echo "usage: move-app-authority-to-prod.sh <app-state-dir> <app-credentials-dir>" >&2
    return 2
  fi
  local repo_root home account_home prod_root account_root state creds
  repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
  # shellcheck source=scripts/supervised-paths.sh
  source "$repo_root/scripts/supervised-paths.sh"
  home=${HOME:?HOME is not set}
  account_home=$(supervised_account_home) || app_move_refuse "the passwd home cannot be resolved" || return
  prod_root=$(supervised_physical_path "$home/Library/Application Support/Freeside") ||
    app_move_refuse "prod's root under \"$home\" cannot be resolved" || return
  account_root=$(supervised_physical_path "$account_home/Library/Application Support/Freeside") ||
    app_move_refuse "prod's root under \"$account_home\" cannot be resolved" || return
  # Identity, not spelling: a case-only or firmlinked HOME names the same root.
  [[ "$prod_root" -ef "$account_root" ]] ||
    app_move_refuse "\$HOME names prod root \"$prod_root\" but the passwd home names \"$account_root\"" || return
  [[ -d "$prod_root" ]] || app_move_refuse "prod's root \"$prod_root\" does not exist; install and start the app once first" || return
  state=$(supervised_physical_path "$1") || app_move_refuse "\"$1\" cannot be resolved" || return
  creds=$(supervised_physical_path "$2") || app_move_refuse "\"$2\" cannot be resolved" || return
  app_move_with_prod_lock "$prod_root" move_app_authority "$state" "$creds" "$prod_root" "$account_home/.freeside/rig-locks" "$(date -u +%Y%m%dT%H%M%SZ)"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
