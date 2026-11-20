#!/bin/zsh
# Cordoned-verifier spike (#3526): run clean verification jobs on the host as
# the owner's account under cordoned-verifier.sb, beside cordoned writers.
# Method and results: docs/spikes/cordoned-verifier.md.
#
# Usage: zsh cordoned-verifier.zsh SUBCOMMAND [args]
#   setup | job ID | rmjob ID | exec ID SBFILE|none CMD... |
#   recipe [ID] [SBFILE] [TAG] | network | writer-vs-job [VARIANT...] |
#   job-vs-job [SBFILE] | writer-vs-daemon [VARIANT...] | teardown [SBFILE] |
#   services [SBFILE] | capture renders|macapp|sim [SBFILE] [JOB] |
#   writer-cli | writer-xcode [VARIANT...] | density N [SBFILE] |
#   overhead [N] | cleanup [all]
# SBFILE defaults to cordoned-verifier.sb; a VARIANT is base, final, or the
#   name of a file in $ROOT/variants/ added after the final writer file.
# Env: ROOT (spike root, a mktemp -d under /tmp: outside any repository and
#   the per-user directories, which the script refuses), REPO
#   (the Freeside checkout jobs and worktrees are cut from),
#   OWNER_CLAUDE_CONFIG (the owner's own CLAUDE_CONFIG_DIR, if not
#   ~/.claude), PROJECTS (the owner's projects directory, for the .env rule),
#   SLOTS_IDLE=1 (overhead's slot path; set only once no daemon serves them).
#
# Only writer-cli starts the Claude CLI. It reads the enrolled setup token
# from stdin, one line, and keeps it in this shell's memory only, as
# cordoned-writer.zsh does; feed it the same way:
#   container run --rm --no-dns -v VOLUME:/cred:ro alpine:3.22 cat /cred/token \
#     | zsh cordoned-verifier.zsh writer-cli
# Never run this script under `set -x` or `zsh -x`: the launch would trace
# the token. Probes print exit statuses and counts, never a credential.
set -u
[[ -o xtrace ]] && { echo "refusing to run traced: writer-cli's launch would print the token" >&2; exit 2; }
: ${ROOT:?} ${REPO:?}
# A TOKEN the caller exported would pass the token to every child.
unset TOKEN
ROOT=${ROOT:A}
OWNER_CLAUDE_CONFIG=${OWNER_CLAUDE_CONFIG:-$HOME/.claude}
PROJECTS=${PROJECTS:-$HOME/proj}
HERE=${0:A:h}
VSB=$HERE/cordoned-verifier.sb
WSB=$HERE/cordoned-writer.sb
WADD=$HERE/cordoned-writer-additions.sb
MV=/Library/Freeside/macverify
GO_PIN=go1.27.4                           # the installed executor's toolchain
COMMAND_PATH=/usr/bin:/bin:/usr/sbin:/sbin  # daemon/internal/macverify/helper.go
PORT_BASE=47100                           # the slots hold 47000 to 47007
CLI=${CLI:-$HOME/.local/bin/claude}       # the owner's installed CLI; no download
PROBE=$HERE/cordoned-verifier-probe.zsh
LOGS=$ROOT/logs
log() { echo "$(date -u +%FT%TZ) $*" | tee -a $LOGS/events.log; }
now() { date +%s.%N; }
dt() { printf "%.1f" $(echo "$2 - $1" | bc -l); }
base() { cat $ROOT/base; }               # the main commit setup pinned
# The owner's per-user directories, shared with every writer on the account.
USER_TEMP=${$(getconf DARWIN_USER_TEMP_DIR)%/}; USER_TEMP=${USER_TEMP:A}
USER_CACHE=${$(getconf DARWIN_USER_CACHE_DIR)%/}; USER_CACHE=${USER_CACHE:A}
# A job's own pair under them comes from a fixed pool of suffixes: the
# directory dirhelper makes in USER_TEMP is flagged sunlnk, so the owner can
# empty it but never remove it, and a name per job would leave one per job.
SUFFIX_POOL=4
# A ROOT inside either directory would be writable by every job, which the
# verifier file opens to USER_TEMP; a job could then rewrite a record the
# harness acts on, such as a job's suffix.
[[ $ROOT/ == ($USER_TEMP|$USER_CACHE)/* ]] && { echo "ROOT must be outside $USER_TEMP and $USER_CACHE" >&2; exit 2; }
mkdir -p $LOGS $ROOT/jobs $ROOT/runs

# Seatbelt logs each denial to the kernel log. The log is system-wide, so
# this keeps every Sandbox line in the window and the caller attributes them
# by process name.
denials() { # start "YYYY-MM-DD HH:MM:SS" out_file -> prints operation counts
  /usr/bin/log show --start "$1" --style compact --predicate 'sender == "Sandbox"' 2>/dev/null \
    | grep -E 'deny\([0-9]+\)' > $2
  sed -E -n 's/.*Sandbox: ([^(]+)\([0-9]+\) deny\([0-9]+\) ([a-z*-]+) ?(.*)$/\1 \2 \3/p' $2 \
    | sed -E 's#(/private/var/folders/[^/]+/[^/]+/[A-Z])/.*#\1/...#' \
    | sort | uniq -c | sort -rn | head -40
}

# --- jobs --------------------------------------------------------------------

# A cache entry matches when every file its record hashes has that hash in
# the workspace, as the room's swiftCacheFor and goCacheFor require.
cache_entry() { # kind workspace -> entry dir
  local kind=$1 ws=$2 rec e ok f h
  for rec in $MV/$kind-cache/*/record.json(N); do
    ok=1
    for f h in $(jq -r '.files[] | .path, .sha256' $rec); do
      [[ $(shasum -a 256 $ws/$f 2>/dev/null | cut -d' ' -f1) == $h ]] || { ok=0; break; }
    done
    (( ok )) && { print -r -- ${rec:h}; return 0; }
  done
  return 1
}
empty_suffix() { # suffix: a job's per-user directories start empty
  # dirhelper's TemporaryItems inside it is protected the same way. Only the
  # pool's own names: anything else could name the per-user directory itself.
  [[ $1 =~ "^freeside-verify-spike-[1-$SUFFIX_POOL]\$" ]] || { echo "not a pool suffix: $1" >&2; return 1; }
  local d; for d in $USER_TEMP/$1 $USER_CACHE/$1; do
    [[ -d $d ]] && find $d -mindepth 1 -maxdepth 1 -not -name TemporaryItems -exec rm -rf {} +
  done
}
port_free() { [[ -z $(lsof -nP -iTCP:$1 -iUDP:$1 2>/dev/null) ]]; }

# new_job builds a job the way slot-run prepare builds one for a slot, as
# the owner: the workspace from git archive of the pinned commit, the Swift
# seed beside it, and job-private home, tmp, and cache directories.
job_id() { [[ $1 =~ '^[A-Za-z0-9][A-Za-z0-9_.-]*$' && $1 != *..* ]] || { echo "bad job id: $1" >&2; return 1; }; }
new_job() { # id -> prints the job root
  job_id $1 || return 1
  local id=$1 j=$ROOT/jobs/$1 n p i sw go
  [[ -e $j ]] && { print -r -- $j; return 0; }
  mkdir -p $j/job/workspace $j/home $j/tmp $j/cache
  git -C $REPO archive $(base) | tar -x -f - -C $j/job/workspace
  sw=$(cache_entry swift $j/job/workspace) || { echo "no Swift cache entry matches" >&2; return 1; }
  go=$(cache_entry go $j/job/workspace) || { echo "no Go cache entry matches" >&2; return 1; }
  tar -c -f - --no-xattrs --no-acls --no-fflags --no-mac-metadata -C $sw/seed/job workspace source-packages \
    | tar -x -f - -C $j/job
  cp $sw/seed/home/.gitconfig $j/home/
  chmod 0700 $j/job
  # The job takes the first pool entry no other job holds: its suffix and
  # four ports outside the slots' range, which cordoned-writer-additions.sb
  # names. rm_job gives the entry back.
  for n in $(seq 0 $((SUFFIX_POOL - 1))); do
    grep -q -x freeside-verify-spike-$((n + 1)) $ROOT/jobs/*/suffix(N) /dev/null || break
  done
  grep -q -x freeside-verify-spike-$((n + 1)) $ROOT/jobs/*/suffix(N) /dev/null \
    && { echo "every pool entry is held" >&2; rm -rf $j; return 1; }
  p=$(( PORT_BASE + 4 * n ))
  for i in 0 1 2 3; do port_free $((p + i)) || { echo "port $((p + i)) is in use" >&2; return 1; }; done
  print -r -- "$p,$((p+1)),$((p+2)),$((p+3))" > $j/ports
  print -r -- $go > $j/go-entry
  print -r -- freeside-verify-spike-$(( n + 1 )) > $j/suffix
  empty_suffix $(cat $j/suffix)
  print -r -- $j
}

rm_job() { # id: removes a job root, which frees its pool entry
  job_id $1 || return 1
  local j=$ROOT/jobs/$1
  [[ -f $j/suffix ]] && empty_suffix $(cat $j/suffix)
  chmod -R u+w $j 2>/dev/null; rm -rf $j
}

# The environment commandEnv gives a slot job, and nothing inherited, plus
# two variables that point the owner's account-wide places at the job's own:
# CFFIXED_USER_HOME moves Foundation's home (SwiftPM's caches, config, and
# security directories follow it; HOME alone doesn't move them), and
# DIRHELPER_USER_DIR_SUFFIX moves confstr's per-user temporary and cache
# directories for tools that aren't platform binaries. Clang and the Swift
# compiler keep their implicit module cache in the shared per-user cache
# directory unless CLANG_MODULE_CACHE_PATH points elsewhere, and a module a
# writer planted there would be compiled into the job. PLAIN=1 leaves all
# three out, as the first recipe pass ran.
job_env() { # job_root -> JENV
  local j=$1 go=$(cat $1/go-entry)
  JENV=(HOME=$j/home TMPDIR=$j/tmp PATH=$MV/go-cache/$GO_PIN/bin:$COMMAND_PATH LC_ALL=C
    FREESIDE_VERIFY_PORTS=$(cat $j/ports) GOFLAGS=-mod=readonly GOMODCACHE=$go/modcache
    GOPROXY=off GOTOOLCHAIN=local GOWORK=off)
  [[ ${PLAIN:-} == 1 ]] || JENV+=(CFFIXED_USER_HOME=$j/home DIRHELPER_USER_DIR_SUFFIX=$(cat $j/suffix)
    CLANG_MODULE_CACHE_PATH=$j/cache/clang/ModuleCache)
}
# The rule file's parameters for a job: the slot profile's set plus the
# owner's paths for the secret rules. Seatbelt matches resolved paths.
job_params() { # job_root -> JPAR
  local j=$1 P=(${(s:,:)$(cat $1/ports)})
  JPAR=(-D JOB_DIR=$j/job -D SLOT_HOME=$j/home -D SLOT_TMP=$j/tmp -D SLOT_CACHE=$j/cache
    -D PORT_1=$P[1] -D PORT_2=$P[2] -D PORT_3=$P[3] -D PORT_4=$P[4]
    -D USER_TEMP=$USER_TEMP -D USER_CACHE=$USER_CACHE -D JOB_SUFFIX=$(cat $j/suffix)
    -D VERIFY_ROOT=$ROOT/jobs -D OWNER_HOME=${HOME:A} -D OWNER_CLAUDE_CONFIG=${OWNER_CLAUDE_CONFIG:A} -D PROJECTS=${PROJECTS:A})
}
# Runs a command in a job's workspace under a verifier rule file, with the
# job's environment and anything in XENV added. SBFILE "none" runs it with
# the same environment and no rule file.
vrun() { # job_root sbfile cmd...
  local j=$1 f=$2 JENV JPAR; shift 2
  job_env $j; job_params $j
  if [[ $f == none ]]; then
    ( cd $j/job/workspace && exec /usr/bin/env -i $JENV $XENV "$@" )
  else
    ( cd $j/job/workspace && exec /usr/bin/env -i $JENV $XENV /usr/bin/sandbox-exec -f $f $JPAR -- "$@" )
  fi
}
XENV=()

recipe_commands() { # workspace -> one JSON argv per line
  jq -c '.commands[] | select(type == "object" and .room == "macos_host") | .argv' $1/.freeside/verify.json
}

# --- writers -----------------------------------------------------------------

# A writer runs under cordoned-writer.sb with cordoned-writer-additions.sb
# after it, so the additions win. "base" is the writer file alone, as #3463
# left it; any other name adds $ROOT/variants/NAME.sb last, for a rule being
# tried before it joins the additions.
writer_file() { # base|final|NAME -> path of the concatenated file
  local out=$ROOT/sb/writer-$1.sb; mkdir -p $ROOT/sb
  case $1 in
    base) cat $WSB > $out ;;
    final) cat $WSB $WADD > $out ;;
    *) cat $WSB $WADD $ROOT/variants/$1.sb > $out ;;
  esac
  print -r -- $out
}
# The writer file's parameters plus the additions'. The daemon's scratch is
# a directory of the spike's, standing in for the daemon's TMPDIR.
writer_params() { # run_dir -> WPAR
  WPAR=(-D HOME=${HOME:A} -D OWNER_CLAUDE_CONFIG=${OWNER_CLAUDE_CONFIG:A}
    -D PROJECTS=${PROJECTS:A} -D WORKTREE=${1:A}
    -D VERIFY_ROOT=$ROOT/jobs -D DAEMON_TMP=$ROOT/daemon-tmp
    -D USER_TEMP=$USER_TEMP -D USER_CACHE=$USER_CACHE)
}
wrun() { # sbfile run_dir cmd...: a command under a writer file, no CLI
  local f=$1 r=$2 WPAR; shift 2; writer_params $r
  /usr/bin/sandbox-exec -f $f $WPAR "$@"
}
new_run() { # id: a worktree of the pinned commit under its own run root
  local r=$ROOT/runs/$1
  [[ -d $r/wt ]] || { mkdir -p $r/config $r/tmp; git -C $REPO worktree add -q --detach $r/wt $(base); }
  print -r -- $r
}
# One probe result: label, exit status, and whether the kernel log or the
# error text says Seatbelt refused it.
st() { # label cmd...
  local label=$1 err s; shift
  err=$("$@" 2>&1 >/dev/null); s=$?
  [[ $err == *(not permitted|denied|Permission)* ]] && echo "$label exit=$s denied" || echo "$label exit=$s"
}
# Creates a new file with a random name in dir and deletes it at once. It
# never opens a file that already exists, so an installed cache can't change.
try_create() { # dir
  local f=$1/.cordon-verify-probe-$RANDOM$RANDOM
  : > $f && rm -f $f
}
# start_job_target runs the spike's target program inside a job, under the
# verifier file, listening on the job's first port with a marker in its
# environment, and sets TPID and TLSTART. It stands in for a test binary a
# job runs; a probe aimed at it can only ever hit the spike's own process.
start_job_target() { # job_root sbfile
  local j=$1 f=$2 P
  cp $ROOT/bin/target $j/job/target
  P=(${(s:,:)$(cat $j/ports)})
  XENV=(SPIKE_JOB_MARK=job-marker-not-a-secret) vrun $j $f $j/job/target $P[1] \
    < /dev/null > $j/target.log 2>&1 &
  for i in {1..20}; do TPID=$(pgrep -f "^$j/job/target") && break; sleep 0.5; done
  TLSTART=$(ps -o lstart= -p $TPID)
}
build_target() {
  [[ -x $ROOT/bin/target && -x $ROOT/bin/sbfind ]] && return 0
  mkdir -p $ROOT/src/target $ROOT/bin
  cat > $ROOT/src/target/go.mod <<'EOF'
module target

go 1.27
EOF
  cat > $ROOT/src/target/main.go <<'EOF'
// target is the spike's stand-in for a job process or the daemon: a
// non-platform binary that holds the environment it was started with,
// optionally listens on a port, and sleeps until killed.
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 {
		l, err := net.Listen("tcp", "127.0.0.1:"+os.Args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "listen:", err)
			os.Exit(1)
		}
		defer l.Close()
	}
	fmt.Println("ready", os.Getpid())
	for {
		time.Sleep(time.Hour)
	}
}
EOF
  cat > $ROOT/src/sbfind.c <<'EOF'
// sbfind JOB_DIR OTHER_DIR PID... prints each pid whose sandbox lets it
// create a file in JOB_DIR and refuses one in OTHER_DIR: the processes that
// run under one job's rule file, whatever their command, directory, or
// process group. Both directories must exist; sandbox_check refuses a path
// it can't resolve.
#include <stdio.h>
#include <stdlib.h>
#include <sys/types.h>
enum { SANDBOX_FILTER_NONE = 0, SANDBOX_FILTER_PATH = 1 };
extern int sandbox_check(pid_t pid, const char *operation, int type, ...);
int main(int argc, char **argv) {
  if (argc < 3) return 2;
  for (int i = 3; i < argc; i++) {
    pid_t pid = (pid_t)atoi(argv[i]);
    if (sandbox_check(pid, NULL, SANDBOX_FILTER_NONE) != 1) continue;
    if (sandbox_check(pid, "file-write-create", SANDBOX_FILTER_PATH, argv[1]) != 0) continue;
    if (sandbox_check(pid, "file-write-create", SANDBOX_FILTER_PATH, argv[2]) == 0) continue;
    printf("%d\n", pid);
  }
  return 0;
}
EOF
  ( cd $ROOT/src/target && env -i HOME=$ROOT/src PATH=$MV/go-cache/$GO_PIN/bin:$COMMAND_PATH \
      GOTOOLCHAIN=local GOCACHE=$ROOT/src/gocache go build -o $ROOT/bin/target . ) || return 1
  /usr/bin/cc -O -o $ROOT/bin/sbfind $ROOT/src/sbfind.c
}
# The preference probes use a key of the spike's own, never the owner's
# appearance: a write that lands proves the domain is writable without
# changing what the owner sees, and cleanup deletes the key.
SPIKE_PREF=FreesideCordonVerifySpike
pref_probe() { # label sbfile run_dir domain: write from the writer, read back outside
  local v=$RANDOM$RANDOM
  wrun $2 $3 /usr/bin/defaults write $4 $SPIKE_PREF -string $v >/dev/null 2>&1
  [[ $(defaults read $4 $SPIKE_PREF 2>/dev/null) == $v ]] && echo "$1 landed=yes" || echo "$1 landed=no"
  defaults delete $4 $SPIKE_PREF >/dev/null 2>&1
}

# writer_probes aims every writer-side probe at one running job target.
writer_probes() { # tag sbfile run_dir job_root
  local tag=$1 f=$2 r=$3 j=$4 P sfx=$(cat $4/suffix)
  P=(${(s:,:)$(cat $j/ports)})
  {
    echo "== $tag"
    st workspace-write wrun $f $r /usr/bin/touch $j/job/workspace/.cordon-verify-probe
    rm -f $j/job/workspace/.cordon-verify-probe
    st workspace-read wrun $f $r /bin/cat $j/job/workspace/README.md
    st job-home-create wrun $f $r /bin/zsh -c "$(typeset -f try_create); try_create $j/home"
    st toolchain-create wrun $f $r /bin/zsh -c "$(typeset -f try_create); try_create $MV/go-cache/$GO_PIN/bin"
    st go-cache-create wrun $f $r /bin/zsh -c "$(typeset -f try_create); try_create $(cat $j/go-entry)/modcache"
    st swift-cache-create wrun $f $r /bin/zsh -c "$(typeset -f try_create); try_create $(ls -d $MV/swift-cache/*/ | head -1)"
    st job-user-temp-create wrun $f $r /bin/zsh -c "$(typeset -f try_create); try_create $USER_TEMP/$sfx"
    st daemon-tmp-create wrun $f $r /bin/zsh -c "$(typeset -f try_create); try_create $ROOT/daemon-tmp"
    # The owner's shared per-user places a job's Xcode tools use (the
    # verifier file's USER_TEMP rule and the module cache it opens).
    st owner-user-temp-create wrun $f $r /bin/zsh -c "$(typeset -f try_create); try_create $USER_TEMP"
    st module-cache-create wrun $f $r /bin/zsh -c "$(typeset -f try_create); try_create $USER_CACHE/clang/ModuleCache"
    wrun $f $r /bin/zsh $PROBE signal $TPID "$TLSTART"
    wrun $f $r /bin/zsh $PROBE attach $TPID "$TLSTART"
    wrun $f $r /bin/zsh $PROBE env $TPID "$TLSTART" job-marker- not-a-secret
    for a in 127.0.0.1 0.0.0.0; do
      st bind-$a-port2 wrun $f $r /usr/bin/python3 -I -c \
        "import socket; s=socket.socket(); s.bind(('$a', $P[2])); s.listen(); s.close()"
    done
    pref_probe pref-global $f $r -g
    pref_probe pref-universalaccess $f $r com.apple.universalaccess
    # The global domain's file, written directly rather than through cfprefsd:
    # a new file beside it whose name the closing rule's prefix covers.
    local gp=${HOME:A}/Library/Preferences/.GlobalPreferences.cordon-verify-probe-$RANDOM
    st pref-global-file-create wrun $f $r /usr/bin/touch $gp; rm -f $gp
  } | tee -a $LOGS/writer-vs-job.txt
}
# writer_work is what a writer still has to do under the same file: build
# and test daemon code, commit, and stop its own child.
writer_work() { # tag sbfile run_dir
  local tag=$1 f=$2 r=$3
  {
    echo "== $tag work"
    ( cd $r/wt/daemon && st go-build wrun $f $r /usr/bin/env GOCACHE=$r/gocache GOTOOLCHAIN=local \
        PATH=$MV/go-cache/$GO_PIN/bin:$COMMAND_PATH go build ./... )
    ( cd $r/wt/daemon && st go-test-golden wrun $f $r /usr/bin/env GOCACHE=$r/gocache GOTOOLCHAIN=local \
        PATH=$MV/go-cache/$GO_PIN/bin:$COMMAND_PATH go test ./internal/golden/... -count=1 )
    echo probe >> $r/wt/README.md
    ( cd $r/wt && st git-commit wrun $f $r /usr/bin/git -c user.name=spike -c user.email=spike@invalid \
        commit -q -a -m "Probe a commit from a cordoned writer" \
        -m "The cordoned-verifier spike resets this commit at once." )
    git -C $r/wt reset -q --hard $(base)
    st own-child-signal wrun $f $r /bin/sh -c 'sleep 30 & kill $!; wait $! ; [ $? -ne 0 ]'
  } | tee -a $LOGS/writer-vs-job.txt
}

# --- the CLI ------------------------------------------------------------------
# read_token, start_cli's fd mode, and leak_check are cordoned-writer.zsh's,
# with the rule file's parameters from writer_params.

read_token() {
  if [[ -t 0 ]]; then read -rs "TOKEN?setup token: "; echo; else IFS= read -r TOKEN; fi
  [[ $TOKEN == sk-ant-oat01-* ]] || { echo "stdin held no setup token" >&2; exit 2; }
}
# Starts the CLI under a writer file and waits for it. The environment is
# cleared and rebuilt inside a subshell; the token reaches the CLI on a
# pipe, fd 3, written by print, a builtin, so it is in no argv and no
# environment. The CLI's stdin is /dev/null, so -p never appends it.
run_cli() { # run_dir sbfile prompt [claude args...]
  local r=$1 f=$2 prompt=$3 t0=$(now) s; shift 3
  local -a args=(-p "$prompt" --output-format stream-json --verbose --safe-mode
    --session-id $(uuidgen | tr A-Z a-z) "$@")
  (
    local v home=$HOME WPAR
    writer_params $r
    for v in ${(k)parameters[(R)*export*]}; do unset $v 2>/dev/null; done
    export HOME=$home PATH=$MV/go-cache/$GO_PIN/bin:/usr/bin:/bin:/usr/sbin:/sbin
    export TMPDIR=$r/tmp CLAUDE_CONFIG_DIR=$r/config LANG=en_US.UTF-8 GOCACHE=$r/gocache GOTOOLCHAIN=local
    export DISABLE_AUTOUPDATER=1 DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1
    export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3
    cd $r/wt
    exec /usr/bin/sandbox-exec -f $f $WPAR $CLI $args \
      3< <(print -r -- $TOKEN) < /dev/null > $r/out.jsonl 2> $r/err.log
  )
  s=$?
  log "${r:t}: cli exit $s in $(dt $t0 $(now)) s; $(result_line $r)"
}
result_line() { # run_dir: the run's final result, without tool output
  jq -r 'select(.type=="result") | "result=\(.subtype) turns=\(.num_turns) cost_usd=\(.total_cost_usd)"' \
    $1/out.jsonl 2>/dev/null | tail -1
}
leak_check() { # run_dir: counts copies of the token in everything the run wrote
  # The token reaches grep on a pipe, never in its arguments. The exact search
  # covers the whole spike root (runs, jobs, logs) and the CLI's cache outside
  # its config dir, and counts files grep couldn't read, so a short count
  # says so. The commit search covers every ref the worktree's repository
  # holds. The worktree is left out of the shape count only: its source has
  # credential-rule fixtures.
  local -a where=($ROOT); [[ -d $HOME/Library/Caches/claude-cli-nodejs ]] && where+=($HOME/Library/Caches/claude-cli-nodejs)
  local unread=$LOGS/leak-check-unreadable.txt
  local exact=$(grep -r -l -F -f <(print -r -- $TOKEN) $where 2> $unread | wc -l | tr -d ' ')
  local commits=$(git -C $1/wt log -p --all 2>/dev/null | grep -c -F -f <(print -r -- $TOKEN))
  local n=$(grep -r -l -E --exclude-dir=wt --exclude-dir=gocache 'sk-ant-[a-z]{3}[0-9]{2}-' $1 2>/dev/null | wc -l | tr -d ' ')
  log "leak check ${1:t}: token in $exact file(s) and $commits commit line(s); $(wc -l < $unread | tr -d ' ') path(s) unreadable; $n file(s) outside wt/ with a token-shaped string"
}

# The subcommands. The whole body is one function, called on the last line
# with exit beside it: zsh reads a script as it runs, so a run in progress
# would otherwise go on to read whatever an edit put at its old offset.
main() {
case ${1:-} in
  setup)
    touch $ROOT/.cordon-verify-spike   # cleanup acts only on a marked root
    git -C $REPO fetch -q origin
    git -C $REPO rev-parse origin/main > $ROOT/base
    { sw_vers; xcodebuild -version; DevToolsSecurity -status
      sysctl -n hw.model machdep.cpu.brand_string hw.memsize
      echo "base $(base)"; } > $LOGS/versions.txt 2>&1
    # The installed executor, compared again by cleanup: path, size, mtime.
    find $MV/bin $MV/go-cache $MV/swift-cache -exec stat -f '%N %z %m %Lp %Su' {} + \
      | sort > $LOGS/executor-manifest.before
    defaults read -g AppleInterfaceStyle > $ROOT/pref-style 2>/dev/null || echo '<unset>' > $ROOT/pref-style
    defaults read com.apple.universalaccess increaseContrast > $ROOT/pref-contrast 2>/dev/null \
      || echo '<unset>' > $ROOT/pref-contrast
    cat $LOGS/versions.txt; echo "executor entries: $(wc -l < $LOGS/executor-manifest.before)"
    echo "prefs: style=$(cat $ROOT/pref-style) contrast=$(cat $ROOT/pref-contrast)" ;;

  job) new_job ${2:?ID} ;;
  rmjob) rm_job ${2:?ID} ;;
  exec) # one command in a job under a rule file: exec ID SBFILE|none CMD...
    j=$(new_job ${2:?ID}) || exit 1; f=${3:?SBFILE}; [[ $f == none ]] || f=${f:A}; shift 3
    vrun $j $f "$@" ;;

  recipe) # the macos_host commands under a verifier rule file
    id=${2:-recipe}; f=${${3:-$VSB}:A}; tag=${4:-${f:t:r}}
    j=$(new_job $id) || exit 1
    log "recipe $id under ${f:t} ($tag): ports $(cat $j/ports)"
    recipe_commands $j/job/workspace | while IFS= read -r argv; do
      cmd=(${(f)"$(jq -r '.[]' <<< $argv)"})
      label=${(j:-:)${cmd:#(env|APP_MACVERIFY=1|bash|scripts/check.sh)}}
      t=$(date '+%F %T'); t0=$(now)
      vrun $j $f $cmd < /dev/null > $LOGS/recipe-$tag-$label.log 2>&1; s=$?
      sleep 2
      log "recipe $tag $label: exit $s in $(dt $t0 $(now)) s"
      denials "$t" $LOGS/recipe-$tag-$label.denials | tee -a $LOGS/recipe-$tag-$label.summary
      if (( s != 0 )); then
        # Tell a rule-file failure from an environment one: the same command,
        # environment, and job, with no rule file.
        t0=$(now)
        vrun $j none $cmd < /dev/null > $LOGS/recipe-$tag-$label-norules.log 2>&1
        log "recipe $tag $label without the rule file: exit $? in $(dt $t0 $(now)) s"
      fi
    done ;;

  network) # the slot probe's sandbox checks, unix sockets, and pf as the owner
    j=$(new_job network) || exit 1; build_target
    P=(${(s:,:)$(cat $j/ports)})
    [[ -x $ROOT/bin/freesided ]] || vrun $j none /bin/sh -c "cd daemon && go build -o $ROOT/bin/freesided ./cmd/freesided" \
      > $LOGS/network-build.log 2>&1 || { echo "freesided build failed" >&2; exit 1; }
    # A listener outside the job on a loopback port the job doesn't hold,
    # which the probe's first check must fail to reach.
    $ROOT/bin/target $((PORT_BASE - 1)) > /dev/null 2>&1 & lpid=$!
    mkdir -p $ROOT/rt; rm -f $ROOT/rt/owner.sock
    /usr/bin/python3 -I -c "import socket,time; s=socket.socket(socket.AF_UNIX); s.bind('$ROOT/rt/owner.sock'); s.listen(); time.sleep(120)" & upid=$!
    sleep 1
    {
      echo "== probe sandbox under ${VSB:t}"
      vrun $j $VSB $ROOT/bin/freesided macverify probe sandbox $((PORT_BASE - 1)) github.com \
        192.0.2.1:443 192.0.2.1:53 $(cat $j/ports) $((P[4] + 1)) /private/tmp /Users/Shared ${HOME:A} $USER_TEMP $ROOT/jobs
      echo "probe exit $?"
      echo "== unix sockets under ${VSB:t}"
      st unix-connect-outside vrun $j $VSB /usr/bin/python3 -I -c \
        "import socket; s=socket.socket(socket.AF_UNIX); s.connect('$ROOT/rt/owner.sock')"
      st unix-bind-in-job vrun $j $VSB /usr/bin/python3 -I -c \
        "import socket,os; p='$j/tmp/s.sock'; s=socket.socket(socket.AF_UNIX); s.bind(p); s.listen(); c=socket.socket(socket.AF_UNIX); c.connect(p); os.unlink(p)"
      st curl-github vrun $j $VSB /usr/bin/curl -sS -m 5 -o /dev/null https://github.com
      echo "== packet-filter mode as the owner, no rule file"
      vrun $j none $ROOT/bin/freesided macverify probe packet-filter 192.0.2.1:443 192.0.2.1:53
      echo "probe exit $?"
      echo "== pf state (read-only): $(pfctl -s info 2>&1 | head -1)"
    } 2>&1 | tee $LOGS/network.txt
    kill $lpid $upid 2>/dev/null; rm -f $ROOT/rt/owner.sock; rm_job network ;;

  writer-vs-job) # a writer under base and final files against a running job
    build_target; mkdir -p $ROOT/daemon-tmp
    j=$(new_job wvj) || exit 1; r=$(new_run writer)
    start_job_target $j $VSB
    log "writer-vs-job: target pid $TPID, ports $(cat $j/ports)"
    vs=(${@[2,-1]}); (( $#vs )) || vs=(base final)
    for v in $vs; do
      f=$(writer_file $v); t=$(date '+%F %T')
      writer_probes $v $f $r $j
      writer_work $v $f $r
      denials "$t" $LOGS/writer-vs-job-$v.denials > $LOGS/writer-vs-job-$v.summary
    done
    echo "== job-side preference read: what a render inherits"
    for plain in 1 0; do
      echo "PLAIN=$plain style=$(PLAIN=$plain vrun $j $VSB /usr/bin/defaults read -g AppleInterfaceStyle 2>&1)"
    done | tee -a $LOGS/writer-vs-job.txt
    kill $TPID; rm_job wvj ;;

  job-vs-job) # one job's probes against another job's running target
    build_target
    a=$(new_job jvj-a) || exit 1; b=$(new_job jvj-b) || exit 1
    f=${${2:-$VSB}:A}
    start_job_target $a $f; P=(${(s:,:)$(cat $a/ports)})
    {
      echo "== job B against job A under ${f:t}"
      st read-other-workspace vrun $b $f /bin/cat $a/job/workspace/README.md
      st write-other-workspace vrun $b $f /usr/bin/touch $a/job/workspace/.cordon-verify-probe
      rm -f $a/job/workspace/.cordon-verify-probe
      st read-owner-user-temp vrun $b $f /bin/ls $USER_TEMP
      st owner-user-temp-create vrun $b $f /bin/zsh -c "$(typeset -f try_create); try_create $USER_TEMP"
      st module-cache-create vrun $b $f /bin/zsh -c "$(typeset -f try_create); try_create $USER_CACHE/clang/ModuleCache"
      st other-suffix-create vrun $b $f /bin/zsh -c "$(typeset -f try_create); try_create $USER_TEMP/$(cat $a/suffix)"
      vrun $b $f /bin/zsh $PROBE signal $TPID "$TLSTART"
      vrun $b $f /bin/zsh $PROBE attach $TPID "$TLSTART"
      vrun $b $f /bin/zsh $PROBE env $TPID "$TLSTART" job-marker- not-a-secret
      st bind-other-port vrun $b $f /usr/bin/python3 -I -c \
        "import socket; s=socket.socket(); s.bind(('127.0.0.1', $P[2])); s.listen()"
      st connect-other-port vrun $b $f /usr/bin/python3 -I -c \
        "import socket; socket.create_connection(('127.0.0.1', $P[1]), 3)"
    } 2>&1 | tee -a $LOGS/job-vs-job.txt
    kill $TPID; rm_job jvj-a; rm_job jvj-b ;;

  services) # what a job reaches through the owner's session services, which run outside its rule file
    j=$(new_job services) || exit 1; f=${${2:-$VSB}:A}
    { echo "== services from a job under ${f:t}"
      # cfprefsd writes a preference for its client. The spike's own domain
      # and key, deleted after.
      for d in com.freeside.spike.prefprobe -g; do
        v=$RANDOM$RANDOM
        vrun $j $f /usr/bin/defaults write $d $SPIKE_PREF -string $v >/dev/null 2>&1
        [[ $(defaults read $d $SPIKE_PREF 2>/dev/null) == $v ]] && echo "pref-write $d landed=yes" || echo "pref-write $d landed=no"
        defaults delete $d $SPIKE_PREF >/dev/null 2>&1
      done
      # Another app's preferences, read through cfprefsd rather than the
      # file: counted, never printed.
      echo "pref-read com.apple.Terminal bytes=$(vrun $j $f /usr/bin/defaults read com.apple.Terminal 2>/dev/null | wc -c | tr -d ' ')"
      echo "pref-file-read com.apple.Terminal bytes=$(vrun $j $f /bin/cat ${HOME:A}/Library/Preferences/com.apple.Terminal.plist 2>/dev/null | wc -c | tr -d ' ')"
      # The general pasteboard, read only and counted; the find pasteboard,
      # written and put back, stands in for a write the owner would paste.
      echo "pasteboard-read bytes=$(vrun $j $f /usr/bin/pbpaste 2>/dev/null | wc -c | tr -d ' ')"
      saved=$(pbpaste -pboard find 2>/dev/null); v=cordon-verify-$RANDOM
      print -rn -- $v | vrun $j $f /usr/bin/pbcopy -pboard find 2>/dev/null
      [[ $(pbpaste -pboard find) == $v ]] && echo "pasteboard-write landed=yes" || echo "pasteboard-write landed=no"
      print -rn -- $saved | pbcopy -pboard find
      # The whole screen, every app's windows: deleted unseen, its size kept.
      vrun $j $f /usr/sbin/screencapture -x $j/job/screen.png 2>/dev/null; s=$?
      echo "screen-capture exit=$s bytes=$(stat -f %z $j/job/screen.png 2>/dev/null || echo 0)"
      rm -f $j/job/screen.png
    } 2>&1 | tee -a $LOGS/services.txt
    rm_job services ;;

  writer-xcode) # a writer's own manifest compile, with and without the shared per-user places
    r=$(new_run writer); vs=(${@[2,-1]}); (( $#vs )) || vs=(final sharedtmp)
    for v in $vs; do
      f=$(writer_file $v)
      # No manifest cache, so the manifest is compiled on every run.
      ( cd $r/wt && wrun $f $r /usr/bin/env TMPDIR=$r/tmp \
          swift package --package-path app --disable-sandbox --manifest-cache none dump-package ) > $LOGS/writer-xcode-$v.log 2>&1
      echo "swift-dump-package $v: exit $? $(grep -v xcrun_db $LOGS/writer-xcode-$v.log | grep -m1 -i error | cut -c1-160)"
      # A module hash no cache entry has: the Foundation import must be
      # compiled into the shared module cache, the cold case.
      print -r -- 'import Foundation; print(Date())' > $r/tmp/cold.swift
      wrun $f $r /usr/bin/env TMPDIR=$r/tmp swiftc -typecheck -Xcc -DCORDON_VERIFY_SPIKE_$RANDOM$RANDOM \
        $r/tmp/cold.swift > $LOGS/writer-xcode-cold-$v.log 2>&1
      echo "swiftc-cold-module-cache $v: exit $? $(grep -v xcrun_db $LOGS/writer-xcode-cold-$v.log | grep -m1 -i error | cut -c1-160)"
    done | tee -a $LOGS/writer-xcode.txt ;;

  writer-vs-daemon) # writers and a job against a stand-in daemon under launchd
    # The stand-in is the spike's target program under a label of its own,
    # so no probe can reach the owner's daemons (ai.freeside.daemon*). Its
    # signature is compared with the installed freesided's: neither has the
    # hardened runtime or get-task-allow, so the OS treats them alike.
    build_target; label=com.freeside.spike.daemon; svc=gui/$(id -u)/$label
    plist=$ROOT/$label.plist; cp $ROOT/bin/target $ROOT/bin/daemon-standin
    cat > $plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>$ROOT/bin/daemon-standin</string><string>$((PORT_BASE - 2))</string></array>
  <key>EnvironmentVariables</key><dict><key>SPIKE_DAEMON_MARK</key><string>daemon-marker-not-a-secret</string></dict>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>$ROOT/logs/daemon-standin.log</string>
  <key>StandardErrorPath</key><string>$ROOT/logs/daemon-standin.log</string>
</dict></plist>
EOF
    launchctl bootout $svc 2>/dev/null; launchctl bootstrap gui/$(id -u) $plist || exit 1
    { echo "== signatures"
      for b in $ROOT/bin/daemon-standin ${FREESIDED:-$HOME/Applications/Freeside.app/Contents/Resources/freesided}; do
        echo "${b:t}: $(codesign -dv $b 2>&1 | grep -E '^(CodeDirectory|Signature)' | tr '\n' ' ')"
        echo "  entitlements: $(codesign -d --entitlements - $b 2>/dev/null | grep -c get-task-allow) get-task-allow"
      done; } | tee $LOGS/writer-vs-daemon.txt
    dpid() { launchctl print $svc 2>/dev/null | awk '/^\tpid = / {print $3}'; }
    restart() { # the stand-in, freshly started; on a loaded host launchd takes seconds
      launchctl print $svc >/dev/null 2>&1 || launchctl bootstrap gui/$(id -u) $plist
      launchctl kickstart -k $svc >/dev/null 2>&1
      DPID=; for i in {1..30}; do DPID=$(dpid); [[ -n $DPID ]] && break; sleep 1; done
      [[ -n $DPID ]] || { echo "the stand-in did not start: $(launchctl print $svc 2>&1 | grep -E '^\t(state|last exit code) =' | tr -d '\t' | tr '\n' ' ')" >&2; exit 1; }
      DLSTART=$(ps -o lstart= -p $DPID); }
    r=$(new_run writer); j=$(new_job wvd) || exit 1; mkdir -p $ROOT/daemon-tmp
    vs=(${@[2,-1]}); (( $#vs )) || vs=(base final job)
    for v in $vs; do
      if [[ $v == job ]]; then
        f=$VSB
        x() { vrun $j $f "$@"; }
      else
        f=$(writer_file $v); x() { wrun $f $r "$@"; }
      fi
      restart
      { echo "== ${v} (${f:t}) against pid $DPID"
        st daemon-tmp-create x /bin/zsh -c "$(typeset -f try_create); try_create $ROOT/daemon-tmp"
        x /bin/zsh $PROBE signal $DPID "$DLSTART"
        x /bin/zsh $PROBE attach $DPID "$DLSTART"
        x /bin/zsh $PROBE env $DPID "$DLSTART" daemon-marker- not-a-secret
        x /bin/zsh $PROBE kickstart $label; restart
        x /bin/zsh $PROBE lkill $label; restart
        x /bin/zsh $PROBE stop $label; restart
        x /bin/zsh $PROBE bootout $label
      } 2>&1 | grep -v xcrun_db | tee -a $LOGS/writer-vs-daemon.txt
    done
    launchctl bootout $svc; rm_job wvd ;;

  teardown) # four survivors a job leaves behind, against each way to find them
    build_target; j=$(new_job teardown) || exit 1; f=${${2:-$VSB}:A}; hb=$j/tmp
    # Each survivor writes its pid to its heartbeat file every 2 s. The last
    # one detaches, cds to /, and rewrites its argv, so neither its command
    # line, its directory, nor its process group names the job.
    cat > $j/job/bg.sh <<EOF
probe='while :; do echo \$\$ > "\$0"; sleep 2; done'
nohup sh -c "\$probe" $hb/hb-nohup >/dev/null 2>&1 &
( sh -c "\$probe" $hb/hb-doublefork >/dev/null 2>&1 & )
/usr/bin/perl -e 'use POSIX; exit if fork; POSIX::setsid(); exec "sh", "-c", @ARGV' "\$probe" $hb/hb-setsid
HB=$hb/hb-rewrite /usr/bin/perl -e 'use POSIX; exit if fork; POSIX::setsid(); chdir "/";
  open STDIN, "<", "/dev/null"; open STDOUT, ">", "/dev/null"; open STDERR, ">", "/dev/null";
  exec { "/usr/bin/perl" } "idle", "-e", "while (1) { open my \\\$h, q(>), \\\$ENV{HB}; print \\\$h qq(\\\$\\\$\\\\n); close \\\$h; sleep 2 }"'
EOF
    names=(nohup doublefork setsid rewrite)
    # A survivor is alive while the pid its heartbeat names is still a sh or
    # perl process; the kill rounds check this seconds after the kill.
    alive() { local n p; for n in $names; do p=$(cat $hb/hb-$n 2>/dev/null) \
        && [[ $(ps -o comm= -p $p 2>/dev/null) == (sh|*/sh|*perl*|idle) ]] && print -n "$n "; done; }
    # Ends whatever is left by the pids the survivors wrote, each checked to
    # be a sh or perl whose heartbeat was written in the last 4 s (it beats
    # every 2 s), so a reused pid is never hit.
    reap() { local n p; for n in $names; do
        [[ -f $hb/hb-$n ]] || continue; p=$(cat $hb/hb-$n)
        (( $(date +%s) - $(stat -f %m $hb/hb-$n) <= 4 )) \
          && [[ $(ps -o comm= -p $p 2>/dev/null) == (sh|*/sh|*perl*|idle) ]] && kill -9 $p 2>/dev/null
        rm -f $hb/hb-$n
      done; sleep 3; }
    launch() { # starts bg.sh in the job under the rule file, in a new process group
      rm -f $hb/pgid
      vrun $j $f /usr/bin/perl -e 'setpgrp(0,0); open my $h, ">", shift; print $h "$$\n"; close $h; exec @ARGV' \
        $hb/pgid /bin/sh $j/job/bg.sh < /dev/null > /dev/null 2>&1
      PGID=$(cat $hb/pgid)
      for i in {1..60}; do (( ${#${=$(alive)}} == 4 )) && break; sleep 1; done
    }
    for finder in pgroup pgrep lsof sandbox; do
      launch
      case $finder in
        pgroup) pids=($(pgrep -g ${PGID:?})) ;;
        pgrep) pids=($(pgrep -f "$j/")) ;;
        lsof) pids=($(lsof -t +D $j 2>/dev/null)) ;;
        sandbox) pids=($($ROOT/bin/sbfind $j/job /private/tmp $(pgrep -U $(id -u)))) ;;
      esac
      (( $#pids )) && kill -9 $pids 2>/dev/null; sleep 5
      echo "$finder: found ${#pids} process(es); survivors still running: $(alive)" | tee -a $LOGS/teardown.txt
      reap
    done
    # A per-job launchd service: the job's command as its program, booted
    # out when the job ends, which ends what launchd still tracks for it.
    label=com.freeside.spike.job-teardown; svc=gui/$(id -u)/$label; plist=$ROOT/$label.plist
    job_env $j; job_params $j
    { print -r -- '<?xml version="1.0" encoding="UTF-8"?>'
      print -r -- '<plist version="1.0"><dict><key>Label</key><string>'$label'</string><key>ProgramArguments</key><array>'
      for a in /usr/bin/env -i $JENV /usr/bin/sandbox-exec -f $f $JPAR -- /bin/sh $j/job/bg.sh; do
        print -r -- "<string>${a//&/&amp;}</string>"; done
      print -r -- '</array><key>WorkingDirectory</key><string>'$j/job/workspace'</string><key>RunAtLoad</key><true/>'
      print -r -- '<key>StandardOutPath</key><string>'$LOGS/teardown-launchd.log'</string><key>StandardErrorPath</key><string>'$LOGS/teardown-launchd.log'</string></dict></plist>'
    } > $plist
    launchctl bootstrap gui/$(id -u) $plist
    # bg.sh returns once its survivors are started; wait for that, as the
    # vrun rounds do, so bootout meets what a finished job leaves. On a
    # loaded host this takes tens of seconds. launchd itself ends what is
    # left in the service's process group when its program exits, so the
    # line before bootout records that.
    for i in {1..120}; do
      [[ $(launchctl print $svc 2>/dev/null | awk '/^\tstate = / {print $3}') == running ]] || break; sleep 1
    done; sleep 3
    { echo "-- the service's process group before bootout"
      ps -o pid=,ppid=,pgid=,stat=,command= -g $(launchctl print $svc 2>/dev/null | awk '/^\tpid = / {print $3}') 2>/dev/null | cut -c1-120; } >> $LOGS/teardown.txt
    echo "launchd: before bootout, running: $(alive); state $(launchctl print $svc 2>/dev/null | awk '/^\t(state|pid) = / {printf "%s ", $3}')" \
      | tee -a $LOGS/teardown.txt
    launchctl bootout $svc 2>/dev/null; sleep 5
    echo "launchd: after bootout, survivors still running: $(alive)" | tee -a $LOGS/teardown.txt
    reap; rm -f $plist
    echo "left after reap: $(alive)" | tee -a $LOGS/teardown.txt
    rm_job teardown ;;

  capture) # renders in-process, the built Mac app's window, and a Simulator
    f=${${3:-$VSB}:A}
    # The capture job reuses a recipe job's build products when one is named.
    j=$(new_job ${4:-capture}) || exit 1; t=$(date '+%F %T')
    case ${2:?renders|macapp|sim} in
      renders) # the screenshot suite's selected-key probe, as render-surfaces.sh runs it
        out=$j/job/renders; mkdir -p $out
        XENV=(FREESIDE_SCREENSHOT_PROBE_KEYS=decision-system_health-large FREESIDE_SCREENSHOT_PROBE_MODE=selected
          FREESIDE_SCREENSHOT_PROBE_REPETITIONS=1 FREESIDE_SCREENSHOT_OUTPUT=$out) \
          vrun $j $f swift test --package-path app --only-use-versions-from-resolved-file --disable-sandbox \
          --filter ScreenshotRegressionTests/probeScreenshotDeterminism < /dev/null > $LOGS/capture-renders.log 2>&1
        log "capture renders: exit $?; images: $(find $out -name '*.png' | wc -l | tr -d ' ')"
        for i in $(find $out -name '*.png' | head -2); do log "  ${i:t} $(sips -g pixelWidth -g pixelHeight $i | awk 'NR>1 {printf "%s ", $2}')"; done ;;

      macapp) # the app the recipe built, launched directly (no Launch Services), then its window
        app=$j/job/derived-data/Build/Products/Debug/FreesideMac.app
        [[ -d $app ]] || { echo "no build in $j; run the recipe on this job first" >&2; exit 2; }
        # winid lists a pid's on-screen windows; it is built outside the job.
        [[ -x $ROOT/bin/winid ]] || { print -r -- 'import CoreGraphics
import Foundation
let pid = Int(CommandLine.arguments[1])!
let windows = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String: Any]] ?? []
for w in windows where w[kCGWindowOwnerPID as String] as? Int == pid {
    if let id = w[kCGWindowNumber as String] as? Int { print(id) }
}' > $ROOT/src/winid.swift && /usr/bin/swiftc -O -o $ROOT/bin/winid $ROOT/src/winid.swift; }
        vrun $j $f $app/Contents/MacOS/FreesideMac -ApplePersistenceIgnoreState YES -FreesideMock YES \
          -FreesideColorScheme light -FreesideContrast standard -FreesideSelect item-system_health \
          < /dev/null > $LOGS/capture-macapp.log 2>&1 &
        sleep 8; apid=$(pgrep -f "^$app/Contents/MacOS/FreesideMac")
        wid=$(vrun $j $f $ROOT/bin/winid ${apid:-0} 2>>$LOGS/capture-macapp.log | head -1)
        log "capture macapp: pid ${apid:-none}, window ${wid:-none}"
        [[ -n $wid ]] && { vrun $j $f /usr/sbin/screencapture -l $wid -o $j/job/mac.png; log "  screencapture under the rule file: exit $?"; }
        [[ -f $j/job/mac.png ]] && log "  mac.png $(sips -g pixelWidth -g pixelHeight $j/job/mac.png | awk 'NR>1 {printf "%s ", $2}')"
        [[ -n $apid ]] && kill $apid ;;  # only the job's own app, found by its job path

      sim) # a device in a set inside the job, the recipe's iOS build, and probes run in the device
        set=$j/job/simulators; mkdir -p $set
        sc() { local l=$1; shift; vrun $j $f xcrun simctl --set $set "$@" < /dev/null >> $LOGS/capture-sim.log 2>&1; local s=$?; log "capture sim $l: exit $s"; return $s; }
        udid=$(vrun $j $f xcrun simctl --set $set create freeside-spike com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro \
          com.apple.CoreSimulator.SimRuntime.iOS-27-2 2>>$LOGS/capture-sim.log)
        log "capture sim create: ${udid:-failed}"
        [[ -n $udid ]] || exit 1
        sc boot boot $udid; sc bootstatus bootstatus $udid -b
        # The recipe's own iOS build when this job ran it; otherwise one here.
        app=$j/job/derived-data/Build/Products/Debug-iphonesimulator/FreesideIOS.app
        [[ -d $app ]] || vrun $j $f xcodebuild -project app/Freeside.xcodeproj -scheme FreesideIOS -destination "id=$udid" \
          -derivedDataPath $j/job/derived-data -clonedSourcePackagesDirPath $j/job/source-packages \
          -onlyUsePackageVersionsFromResolvedFile -skipPackageUpdates -skipPackagePluginValidation -skipMacroValidation \
          -disablePackageRepositoryCache -scmProvider system -IDEPackageSupportDisableManifestSandbox=YES \
          -IDEPackageSupportDisablePluginExecutionSandbox=YES 'OTHER_SWIFT_FLAGS=$(inherited) -disable-sandbox' \
          CODE_SIGNING_ALLOWED=NO COMPILER_INDEX_STORE_ENABLE=NO build < /dev/null > $LOGS/capture-sim-build.log 2>&1
        log "capture sim build: exit $?"
        if [[ -d $app ]]; then
          bid=$(/usr/libexec/PlistBuddy -c 'Print CFBundleIdentifier' $app/Info.plist)
          sc install install $udid $app; sc launch launch $udid $bid -FreesideMock YES; sleep 6
          sc screenshot io $udid screenshot $j/job/sim.png
          [[ -f $j/job/sim.png ]] && log "  sim.png $(sips -g pixelWidth -g pixelHeight $j/job/sim.png | awk 'NR>1 {printf "%s ", $2}')"
        fi
        # A process in the device is launchd_sim's child, not the job's, so
        # the job's rule file doesn't reach it. Each probe prints a status
        # and a count, never what it read.
        { echo "== in-device probes (simctl spawn under ${f:t})"
          # No shell: the iOS runtime has none. A failure prints its error
          # line; a success prints only how many lines it wrote.
          for probe in "/bin/ls ${HOME:A}/.ssh" "/bin/ls ${HOME:A}/Library/Keychains" \
              "/usr/bin/touch /private/tmp/cordon-verify-sim-probe" "/usr/bin/touch ${j}/job/sim-probe-in-job"; do
            out=$(vrun $j $f xcrun simctl --set $set spawn $udid ${=probe} 2>&1); s=$?
            out=${(F)${(f)out}:#*xcrun_db*}
            (( s == 0 )) && echo "$probe: exit 0 lines $(print -r -- $out | grep -c .)" \
              || echo "$probe: exit $s: ${${(f)out}[1]:0:120}"
          done
          echo "touched outside the job: $([[ -e /private/tmp/cordon-verify-sim-probe ]] && echo yes || echo no)"
          rm -f /private/tmp/cordon-verify-sim-probe $j/job/sim-probe-in-job
          echo "-- device processes and their parents"
          ps -ax -o ppid=,comm= | grep -E 'launchd_sim|SimulatorTrampoline|CoreSimulatorService' | awk '{n=split($2,a,"/"); print ($1==1)?"launchd":"other", a[n]}' | sort | uniq -c
        } 2>&1 | tee -a $LOGS/capture-sim.txt
        sc shutdown shutdown $udid; sc delete delete $udid ;;
    esac
    denials "$t" $LOGS/capture-$2.denials > $LOGS/capture-$2.summary ;;

  writer-cli) # one real CLI writer run under the final writer file, beside a running job
    read_token; build_target
    j=$(new_job wcli) || exit 1; r=$(new_run writer-cli); f=$(writer_file final); mkdir -p $r/gocache
    start_job_target $j $VSB
    # The run's one command: a writer's ordinary work, then the same reaches
    # at the job the harness's probes make, from a tool the CLI started.
    cat > $r/task.sh <<EOF
cd $r/wt/daemon && go build ./... > /dev/null 2>&1; echo "go-build exit=\$?"
cd $r/wt && echo cordoned >> README.md && git -c user.name=spike -c user.email=spike@invalid commit -q -a \\
  -m "Note a cordoned writer run" -m "The cordoned-verifier spike makes this commit from a CLI run." > /dev/null 2>&1
echo "git-commit exit=\$?"
touch $j/job/workspace/.cordon-verify-probe 2>/dev/null; echo "job-workspace-write exit=\$?"
cat $j/job/workspace/README.md > /dev/null 2>&1; echo "job-workspace-read exit=\$?"
zsh $PROBE signal $TPID "$TLSTART"
zsh $PROBE env $TPID "$TLSTART" job-marker- not-a-secret
EOF
    run_cli $r $f "Run exactly this one command and nothing else: sh $r/task.sh
Then reply with its output, verbatim." --dangerously-skip-permissions
    # The leak check runs before anything the CLI wrote is printed.
    leak_check $r
    jq -rs 'map(select(.type=="result")) | last | .result' $r/out.jsonl 2>/dev/null | tee $LOGS/writer-cli.txt
    git -C $r/wt log --oneline -1 $(base)..HEAD | sed 's/^/commit: /' | tee -a $LOGS/writer-cli.txt
    kill $TPID; rm_job wcli ;;

  density) # N jobs run one recipe step at once, beside four writers building and testing Go
    n=${2:?N}; f=${${3:-$VSB}:A}; step=${STEP:-app-build-mac}
    (( n >= 1 && n <= SUFFIX_POOL )) || { echo "N is 1 to $SUFFIX_POOL" >&2; exit 2; }
    for i in $(seq $n); do new_job density-$i > /dev/null || exit 1; done
    for w in 1 2 3 4; do mkdir -p $(new_run writer-$w)/gocache; done
    wf=$(writer_file final)
    log "density $n ($step under ${f:t}): load $(sysctl -n vm.loadavg)"
    t0=$(now)
    for w in 1 2 3 4; do (
      r=$ROOT/runs/writer-$w; s0=$(now); cd $r/wt/daemon
      wrun $wf $r /usr/bin/env GOCACHE=$r/gocache GOTOOLCHAIN=local PATH=$MV/go-cache/$GO_PIN/bin:$COMMAND_PATH \
        /bin/sh -c 'go build ./... && go test ./internal/golden/... ./internal/domain/... -count=1' \
        < /dev/null > $LOGS/density-$n-writer-$w.log 2>&1
      log "density $n writer $w: exit $? in $(dt $s0 $(now)) s" ) &
    done
    for i in $(seq $n); do (
      j=$ROOT/jobs/density-$i
      recipe_commands $j/job/workspace | while IFS= read -r argv; do
        cmd=(${(f)"$(jq -r '.[]' <<< $argv)"})
        [[ ${(j:-:)${cmd:#(env|APP_MACVERIFY=1|bash|scripts/check.sh)}} == $step ]] || continue
        s0=$(now); vrun $j $f $cmd < /dev/null > $LOGS/density-$n-job-$i.log 2>&1
        log "density $n job $i: exit $? in $(dt $s0 $(now)) s"
      done ) &
    done
    wait
    log "density $n: everything done in $(dt $t0 $(now)) s; load $(sysctl -n vm.loadavg)"
    for i in $(seq $n); do rm_job density-$i; done ;;

  overhead) # per-command cost: bare, under the verifier file, and through the slot path
    n=${2:-20}; j=$(new_job overhead) || exit 1
    timed() { local label=$1 i t0; shift; t0=$(now)
      for i in $(seq $n); do "$@" < /dev/null > /dev/null 2>&1 || { echo "$label failed" >&2; return 1; }; done
      log "overhead $label: $(printf %.1f $(echo "($(now) - $t0) * 1000 / $n" | bc -l)) ms per command over $n"; }
    log "overhead: load $(sysctl -n vm.loadavg)"
    timed bare vrun $j none /usr/bin/true
    timed verifier-file vrun $j $VSB /usr/bin/true
    timed verifier-file-go-version vrun $j $VSB go version
    timed bare-go-version vrun $j none go version
    rm_job overhead
    # The slot path is the production room's: Room.Run through the installed
    # helper, from a scratch test in a copy of the daemon outside the
    # repository, so the checkout gains no file. It runs only while no daemon
    # serves the slots, since a second Backend would lease a slot twice and
    # its teardown would end the daemon's job. A daemon can take the executor
    # from its stored instance settings with no flag in its argv, so the
    # operator asserts it (SLOTS_IDLE=1) after checking each daemon's
    # settings; the harness adds only that no slot process runs now.
    if [[ ${SLOTS_IDLE:-} != 1 ]] || pgrep -u fvslot1,fvslot2 > /dev/null; then
      log "overhead slot-path: skipped; set SLOTS_IDLE=1 once no daemon serves fvslot1 and fvslot2"; exit 0
    fi
    ws=$ROOT/slot-overhead; rm -rf $ws; mkdir -p $ws
    git -C $REPO archive $(base) daemon | tar -x -f - -C $ws
    cat > $ws/daemon/internal/macverify/spike_overhead_live_test.go <<'EOF'
//go:build darwin

package macverify

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The cordoned-verifier spike's slot-path timing: Run of /usr/bin/true,
// SPIKE_N times, through liveRoom's production room.
func TestSpikeSlotOverhead(t *testing.T) {
	room, _ := liveRoom(t)
	n, err := strconv.Atoi(os.Getenv("SPIKE_N"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < n; i++ {
		res, err := room.Run(t.Context(), dir, []string{"/usr/bin/true"})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("run %d: exit %d, %v", i, res.ExitCode, err)
		}
	}
	t.Logf("slot-path: %.1f ms per command over %d", float64(time.Since(start).Milliseconds())/float64(n), n)
}
EOF
    ( cd $ws/daemon && SPIKE_N=$((n < 10 ? n : 10)) FREESIDE_MACVERIFY_LIVE_TEST=1 FREESIDE_MACVERIFY_ROOT=$MV \
        FREESIDE_MACVERIFY_SLOTS=fvslot1,fvslot2 GOTOOLCHAIN=local PATH=$MV/go-cache/$GO_PIN/bin:$PATH \
        go test ./internal/macverify -run '^TestSpikeSlotOverhead$' -count=1 -v ) > $LOGS/overhead-slot.log 2>&1
    log "overhead slot-path: test exit $?; $(grep -o 'slot-path: .*' $LOGS/overhead-slot.log)"
    rm -rf $ws ;;

  cleanup) # ends what the spike started and checks the host; "all" also removes ROOT
    [[ -f $ROOT/.cordon-verify-spike ]] || { echo "$ROOT is not a spike root" >&2; exit 2; }
    # The spike's launchd services, by its own label prefix only.
    for l in $(launchctl list | awk '$3 ~ /^com\.freeside\.spike\./ {print $3}'); do
      launchctl bootout gui/$(id -u)/$l && log "cleanup: booted out $l"
    done
    # Processes whose command line names the spike root: job targets, the
    # stand-ins, builds still running. Nothing outside ROOT matches.
    pids=($(pgrep -f -- "$ROOT/")); pids=(${pids:#($$|$PPID)}); (( $#pids )) && { kill $pids 2>/dev/null; log "cleanup: ended ${#pids} process(es) under ROOT"; }
    # Simulator devices live in device sets inside job roots.
    for s in $ROOT/jobs/*/job/simulators(N/); do xcrun simctl --set $s delete all 2>/dev/null; done
    for d in -g com.apple.universalaccess; do defaults delete $d $SPIKE_PREF 2>/dev/null; done
    for r in $ROOT/runs/*/wt(N/); do git -C $REPO worktree remove --force $r && log "cleanup: removed worktree ${r:h:t}"; done
    for id in $ROOT/jobs/*(N/); do rm_job ${id:t}; done
    for n in $(seq $SUFFIX_POOL); do empty_suffix freeside-verify-spike-$n; done
    { echo "prefs now: style=$(defaults read -g AppleInterfaceStyle 2>/dev/null || echo '<unset>')" \
        "contrast=$(defaults read com.apple.universalaccess increaseContrast 2>/dev/null || echo '<unset>')"
      echo "prefs before: style=$(cat $ROOT/pref-style) contrast=$(cat $ROOT/pref-contrast)"
      find $MV/bin $MV/go-cache $MV/swift-cache -exec stat -f '%N %z %m %Lp %Su' {} + | sort > $LOGS/executor-manifest.after
      echo "executor manifest changes: $(diff $LOGS/executor-manifest.before $LOGS/executor-manifest.after | grep -c '^[<>]')"
      echo "slot processes: $(pgrep -U fvslot1,fvslot2 | wc -l | tr -d ' ')"
      echo "spike labels left: $(launchctl list | grep -c com.freeside.spike.)"
      echo "processes under ROOT left: $(pgrep -f -- "$ROOT/" | wc -l | tr -d ' ')"
      echo "suffix directories: $(ls -d $USER_TEMP/freeside-verify-spike-* $USER_CACHE/freeside-verify-spike-* 2>/dev/null | wc -l | tr -d ' ')," \
        "entries in them: $(find $USER_TEMP/freeside-verify-spike-* $USER_CACHE/freeside-verify-spike-* -mindepth 1 -maxdepth 1 -not -name TemporaryItems 2>/dev/null | wc -l | tr -d ' ')"
    } > $LOGS/cleanup.txt; cat $LOGS/cleanup.txt   # a tee into ROOT would count itself
    if [[ ${2:-} == all ]]; then
      cp $LOGS/cleanup.txt ${TMPDIR:-/tmp}/cordon-verify-cleanup.txt
      chmod -R u+w $ROOT 2>/dev/null; rm -rf $ROOT && echo "removed $ROOT; the cleanup record is at ${TMPDIR:-/tmp}/cordon-verify-cleanup.txt"
    fi ;;

  *) sed -n '2,28p' $0; exit 2 ;;
esac
}

main "$@"; exit
