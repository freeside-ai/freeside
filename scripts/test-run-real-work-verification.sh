#!/usr/bin/env bash
# Hermetic fixtures execute the shipped supervision, identity adoption, final
# verification and EXIT trap, without agents, services, containers or forge writes.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

python3 - "$root/scripts/run-real-work.sh" "$tmp" <<'PY'
import pathlib, sys
source = pathlib.Path(sys.argv[1]).read_text()
directory = pathlib.Path(sys.argv[2])
directory.joinpath("final.sh").write_text(source[source.index("# Follow durable, read-only snapshots"):])
start = source.index('\ncleanup() {\n') + 1
end = source.index("trap 'exit 143' TERM", start) + len("trap 'exit 143' TERM")
directory.joinpath("cleanup.sh").write_text(source[start:end] + '\n')
start = source.index('specification_verifier_env=(-u FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID)')
end = source.index('\nfi', start) + len('\nfi')
directory.joinpath("specification-env.sh").write_text(source[start:end] + '\n')
PY

mkdir "$tmp/bin"
cat >"$tmp/bin/go" <<'STUB'
#!/usr/bin/env python3
import json, os, pathlib, sys
session = pathlib.Path(os.environ["CASE_DIR"])
scenario = os.environ["VERIFY_SCENARIO"]
env = {k: v for k, v in os.environ.items() if k.startswith("FREESIDE_REAL_RUN_")}
session.joinpath("verifier-env.json").write_text(json.dumps(env))
assert sys.argv[1:] == ["test", "-C", os.environ["ROOT"] + "/daemon",
    "./internal/integration/", "-run", "TestRealWorkItemCompletesProductionPipeline",
    "-count=1", "-v"]
client = os.environ["CLIENT_TARGET"] == "true"
run = "target-run" if client else "run-impl"
invocation = "target-inv" if client else "inv-admission"
assert env["FREESIDE_REAL_RUN_IMPLEMENTATION_RUN_ID"] == run
assert env["FREESIDE_REAL_RUN_IMPLEMENTATION_INVOCATION"] == invocation
assert env["FREESIDE_REAL_RUN_LIVE_TEST"] == "1"
assert "FREESIDE_REAL_RUN_RUN_ID" not in env
assert "FREESIDE_REAL_RUN_INVOCATION" not in env
if os.environ["SPECIFICATION"]:
    assert env["FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID"] == os.environ["SPECIFICATION"]
else:
    assert "FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID" not in env
if client:
    assert env["FREESIDE_REAL_RUN_TARGET_TASK_ID"] == "target-task"
retained = env.get("FREESIDE_REAL_RUN_RETAINED") == "1"
state = os.environ["SNAPSHOT_STATE"]
if state == "published":
    assert env.get("FREESIDE_REAL_RUN_RETAINED") == "0"
    assert "FREESIDE_REAL_RUN_CHECKPOINT_PATH" not in env
elif not retained:
    print("normal verification refuses completed history")
    sys.exit(1)
if scenario == "error":
    print("verifier failed")
    sys.exit(1)
if scenario == "spec-failure":
    print("real run specification failed: fixture")
    sys.exit(1)
if scenario == "skip":
    print("--- SKIP: TestRealWorkItemCompletesProductionPipeline")
    sys.exit(0)
checkpoint_path = env.get("FREESIDE_REAL_RUN_CHECKPOINT_PATH")
if retained:
    assert checkpoint_path and checkpoint_path != str(session / "stale-checkpoint.json")
    path = pathlib.Path(checkpoint_path)
    assert not path.exists(), "completed checkpoint must be fresh"
    if scenario not in ("marker-only", "ready-marker", "retained-marker"):
        checkpoint = {"state": "completed", "binding": {"run_id": run},
                      "completion": {"merge_commit_sha": "merge-fixture"}}
        if scenario == "wrong-state":
            checkpoint["state"] = "ready"
        if scenario == "incomplete-history":
            checkpoint["state"] = "retained"
        if scenario == "wrong-run":
            checkpoint["binding"]["run_id"] = "foreign-run"
        if scenario == "missing-binding":
            del checkpoint["binding"]
        path.write_text("{" if scenario == "malformed" else json.dumps(checkpoint))
    if scenario == "checkpoint-only":
        sys.exit(0)
    marker_run = run
    if scenario == "wrong-marker-run":
        marker_run = "foreign-run"
    if scenario == "marker-prefix-run":
        marker_run += "-foreign"
    if scenario == "ready-marker":
        print("real production pipeline verified: PR #7")
    elif scenario == "retained-marker":
        print(f"retained production checkpoint verified: run={run} prior PR #7")
    else:
        print(f"completed production checkpoint verified: run={marker_run} PR #7 merged as merge-fixture")
else:
    if scenario == "completed-marker":
        print(f"completed production checkpoint verified: run={run} PR #7 merged as merge-fixture")
    else:
        print("real production pipeline verified: PR #7 at head accepted-head over base base-sha")
if scenario == "positive-error":
    sys.exit(1)
STUB
chmod +x "$tmp/bin/go"

cat >"$tmp/caller.sh" <<'CALLER'
#!/usr/bin/env bash
set -euo pipefail
source "$ROOT/scripts/run-real-work-supervision.sh"
workdir=$CASE_DIR
repo_root=$ROOT
db_path=/fixture/database
daemon_pid=""
rig_pid=""
rig_acquired=true
composition_evidence_tmp=""
listen_address=127.0.0.1:7331
supervision_timeout=3
last_supervision_snapshot=$workdir/snapshot.json
client_target=$CLIENT_TARGET
specification_run_id=$SPECIFICATION
target_task_id=""
implementation_run_id=run-impl
implementation_invocation_id=inv-admission
required=()
source "$SPECIFICATION_ENV"
if [[ "$client_target" == true ]]; then
  implementation_run_id=""
  implementation_invocation_id=seed-inv
  target_task_id=target-task
  specification_verifier_env=(FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID="$specification_run_id")
fi
real_work_resolve_implementation() { printf 'target-run\n'; }
real_work_walkthrough() { touch "$workdir/walkthrough"; }
write_diagnostic() { touch "$workdir/diagnostic"; }
real_work_restore_supervised() {
  touch "$workdir/restored"
  [[ "$CLEANUP_FAIL" == false ]]
}
source "$CLEANUP"
source "$FINAL"
CALLER

assert_absent() {
  if grep -Fq "$2" "$1"; then
    echo "unexpected '$2' in $1" >&2
    exit 1
  fi
}

run_case() {
  local name=$1 state=$2 scenario=$3 expected=$4 specification=${5:-} client=${6:-false} cleanup_fail=${7:-false} snapshot_run=${8:-}
  local session=$tmp/$name status run=run-impl
  [[ "$client" != true ]] || run=target-run
  mkdir "$session"
  printf 'running\n' >"$session/status"
  printf 'fixture daemon log\n' >"$session/daemon.log"
  # Seed tempting evidence, including the ambient checkpoint override. The
  # current verifier attempt must supply its own log and completed checkpoint.
  printf 'completed production checkpoint verified: run=%s PR #7\nreal production pipeline verified: PR #7\n' "$run" >"$session/verify-final.log"
  printf '{"state":"completed","binding":{"run_id":"%s"}}\n' "$run" >"$session/stale-checkpoint.json"
  printf '{"implementation_run_id":"target-run","implementation_invocation_id":"target-inv"}\n' >"$session/target.json"
  python3 - "$session" "$state" "${snapshot_run:-$run}" "$run" <<'PY'
import json, pathlib, sys
snapshot = {"state": sys.argv[2], "run_id": sys.argv[3],
            "admission": {"invocation_id": "inv-successor"}, "attention_items": []}
if sys.argv[3] in ("__MALFORMED__", "__MISSING_RUN__"):
    snapshot["run_id"] = sys.argv[4]
if sys.argv[3] == "__MISSING_RUN__":
    del snapshot["run_id"]
text = json.dumps(snapshot, separators=(",", ":"))
if sys.argv[3] == "__MALFORMED__":
    text += " invalid JSON"
pathlib.Path(sys.argv[1], "implementation.json").write_text(text)
PY
  cat >"$session/freesided" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
run=""
while (($#)); do
  case "$1" in
    -run) run=$2; shift 2 ;;
    *) shift ;;
  esac
done
printf '%s\n' "$run" >>"$CASE_DIR/observed-runs"
if [[ -n "$SPECIFICATION" && "$run" == "$SPECIFICATION" ]]; then
  printf '{"state":"implementation_bound","run_id":"%s"}\n' "$run"
else
  cat "$CASE_DIR/implementation.json"
fi
STUB
  chmod +x "$session/freesided"
  cat >"$session/real-work-retained.py" <<'STUB'
import json, os, pathlib, sys
session = pathlib.Path(os.environ["CASE_DIR"])
assert sys.argv[1] == "remote"
checkpoint = json.loads(pathlib.Path(sys.argv[2]).read_text())
assert checkpoint["state"] == "completed"
session.joinpath("remote").write_text(sys.argv[2])
sys.exit(1 if os.environ["VERIFY_SCENARIO"] == "remote-refusal" else 0)
STUB
  if [[ "$scenario" == log-unwritable ]]; then
    rm "$session/verify-final.log"
    mkdir "$session/verify-final.log"
  fi
  set +e
  env PATH="$tmp/bin:$PATH" ROOT="$root" CASE_DIR="$session" \
    CLEANUP="$tmp/cleanup.sh" FINAL="$tmp/final.sh" \
    SPECIFICATION_ENV="$tmp/specification-env.sh" \
    VERIFY_SCENARIO="$scenario" SNAPSHOT_STATE="$state" \
    CLIENT_TARGET="$client" SPECIFICATION="$specification" CLEANUP_FAIL="$cleanup_fail" \
    FREESIDE_REAL_RUN_RETAINED="${AMBIENT_RETAINED:-1}" FREESIDE_REAL_RUN_CHECKPOINT_PATH="$session/stale-checkpoint.json" \
    FREESIDE_REAL_RUN_RUN_ID=legacy-run FREESIDE_REAL_RUN_INVOCATION=legacy-inv \
    FREESIDE_REAL_RUN_SPECIFICATION_RUN_ID=ambient-spec \
    bash "$tmp/caller.sh" >"$session/output" 2>&1
  status=$?
  set -e
  if [[ "$status" -ne "$expected" ]]; then
    echo "$name: status=$status, want $expected" >&2
    cat "$session/output" >&2
    exit 1
  fi
  [[ -f "$session/diagnostic" && -f "$session/restored" ]]
  if [[ "$expected" == 0 && "$state" == completed ]]; then
    grep -Fq "verified completed history for implementation run=$run invocation=" "$session/output"
    [[ -f "$session/remote" && ! -e "$session/walkthrough" ]]
    assert_absent "$session/output" 'run-real-work: verified ready publication for'
  elif [[ "$expected" == 0 && "$state" == published ]]; then
    grep -Fq 'verified ready publication' "$session/output"
    [[ -f "$session/walkthrough" && ! -e "$session/remote" ]]
  else
    [[ ! -e "$session/walkthrough" ]]
    assert_absent "$session/output" 'run-real-work: verified ready publication for'
    if [[ "$cleanup_fail" == false ]]; then
      assert_absent "$session/output" 'run-real-work: verified completed history for'
      if [[ "$scenario" != remote-refusal ]]; then
        [[ ! -e "$session/remote" ]]
      fi
    fi
  fi
  if [[ "$client" == true && "$expected" == 0 ]]; then
    [[ "$(cat "$session/implementation-run")" == target-run ]]
    [[ "$(cat "$session/implementation-invocation")" == target-inv ]]
    grep -Fq 'FREESIDE_REAL_RUN_TARGET_TASK_ID=target-task' "$session/verification-env.sh"
    assert_absent "$session/verification-env.sh" seed
  fi
  if [[ "$cleanup_fail" == true ]]; then
    [[ -f "$session/remote" && "$(cat "$session/status")" == recovery-required ]]
  fi
}

run_case direct-completed completed success 0
run_case specification-handoff completed success 0 run-spec
run_case client-target completed success 0 target-spec true
run_case published published success 0
run_case published-handoff published success 0 run-spec
run_case published-client published success 0 target-spec true
run_case cleanup-failure completed success 1 "" false true
for scenario in error positive-error skip marker-only checkpoint-only wrong-state incomplete-history \
  wrong-run missing-binding malformed wrong-marker-run marker-prefix-run \
  ready-marker retained-marker remote-refusal spec-failure log-unwritable; do
  run_case "completed-$scenario" completed "$scenario" 1
done
for scenario in error positive-error skip completed-marker; do
  run_case "published-$scenario" published "$scenario" 1
done
run_case unsupported-snapshot retained success 1
run_case foreign-snapshot completed success 1 "" false false foreign-run
run_case foreign-client-snapshot completed success 1 target-spec true false run-impl
run_case malformed-snapshot completed success 1 "" false false __MALFORMED__
run_case missing-snapshot-run completed success 1 "" false false __MISSING_RUN__
echo 'run-real-work final verification fixtures passed'
