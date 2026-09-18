#!/usr/bin/env bash
# Pure retained-coordinate and remote-head refusals; no runtime or network.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
PYTHONDONTWRITEBYTECODE=1 python3 - "$root/scripts/real-work-retained.py" <<'PY'
import copy
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile

spec = importlib.util.spec_from_file_location("retained", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def refuses(call):
    try:
        call()
    except (ValueError, KeyError, OSError):
        return
    raise AssertionError("invalid retained state was accepted")


checkpoint = {"state": "ready", "branch": "fix/output", "binding": {
    "repo": "owner/project", "repository_id": 42, "pr_number": 7,
    "head_sha": "expected-head", "base_ref": "main"}}
observed = {"state": "open", "number": 7,
            "head": {"sha": "expected-head", "ref": "fix/output", "repo": {"id": 42}},
            "base": {"ref": "main", "repo": {"id": 42}}}
for state in ("ready", "retained"):
    module.check_remote(dict(checkpoint, state=state), observed)
refuses(lambda: module.check_remote(dict(checkpoint, state="unknown"), observed))
for path, value in ((["state"], "closed"), (["number"], 8),
                    (["head", "sha"], "old-head"), (["head", "ref"], "other"),
                    (["head", "repo", "id"], 99), (["base", "repo", "id"], 99),
                    (["base", "ref"], "other")):
    changed = copy.deepcopy(observed)
    leaf = changed
    for key in path[:-1]:
        leaf = leaf[key]
    leaf[path[-1]] = value
    refuses(lambda: module.check_remote(checkpoint, changed))

completed = dict(checkpoint, state="completed", completion={
    "pr_number": 7, "merge_commit_sha": "expected-merge"})
merged = dict(observed, state="closed", merged=True, merge_commit_sha="expected-merge")
module.check_remote(completed, merged)
for path, value in ((["state"], "open"), (["merged"], False), (["merged"], 1),
                    (["merge_commit_sha"], "other-merge"), (["merge_commit_sha"], None),
                    (["number"], 8), (["head", "sha"], "other-head"),
                    (["head", "ref"], "other-branch"), (["head", "repo", "id"], 99),
                    (["base", "repo", "id"], 99), (["base", "ref"], "other-base")):
    changed = copy.deepcopy(merged)
    leaf = changed
    for key in path[:-1]:
        leaf = leaf[key]
    leaf[path[-1]] = value
    refuses(lambda: module.check_remote(completed, changed))
for completion in (None, {}, {"pr_number": 8, "merge_commit_sha": "expected-merge"},
                   {"pr_number": 7, "merge_commit_sha": ""},
                   {"pr_number": 7, "merge_commit_sha": None}):
    refuses(lambda: module.check_remote(dict(completed, completion=completion), merged))
for state in ("ready", "retained"):
    refuses(lambda: module.check_remote(dict(checkpoint, state=state), merged))

fields = "repository repository_id base_ref base_sha profile_digest review_configuration_digest allowed_paths claude_auth_identity claude_auth_volume codex_auth_identity images identity build_egress_configuration_digest".split()
original = dict.fromkeys(fields, "unchanged")
module.compare_composition(original, dict(original, daemon_build="new", rig="new"))
for field in fields:
    refuses(lambda: module.compare_composition(original, dict(original, **{field: "changed"})))
for field in ("shadow_review_configuration_digest", "review_instructions_present",
              "review_instructions_digest", "judgment_configuration_digest"):
    refuses(lambda: module.compare_composition(original, dict(original, **{field: "changed"})))

with tempfile.TemporaryDirectory() as temp:
    session = Path(temp)
    state = session / "state"
    state.mkdir()
    (state / "freeside.db").touch()
    os.environ.update(FREESIDE_REAL_RUN_STATE_ROOT=str(state),
                      FREESIDE_REAL_RUN_LISTEN="127.0.0.1:8722",
                      FREESIDE_REAL_RUN_SEED_ROOT=str(session / "seed"))
    for file, value in {"status": "completed", "state-root": str(state),
                        "listener": "127.0.0.1:8722", "implementation-run": "run-retained",
                        "implementation-invocation": "inv-implement-run-retained"}.items():
        (session / file).write_text(value + "\n")
    (session / "submit.json").write_text(json.dumps({"run_id": "run-retained", "implementation_invocation_id": "inv-implement-run-retained"}))
    composition = {"identity": {"implementation_run_id": "run-retained", "implementation_invocation_id": "inv-implement-run-retained"}}
    (session / "composition-manifest.json").write_text(json.dumps(composition))
    (session / "rig-acquisition.json").write_text(json.dumps({"manifest": {"resources": {"seed_root": str(session / "seed")}}}))
    module.validate_session(session)
    # New sessions bind the retained identity across all three artifacts;
    # the legacy fixture above remains valid without an invented identity.
    submission = json.loads((session / "submit.json").read_text())
    submission["submission_id"] = "saved-submission"
    composition["identity"]["submission_id"] = "saved-submission"
    (session / "submission-id").write_text("saved-submission\n")
    (session / "submit.json").write_text(json.dumps(submission))
    (session / "composition-manifest.json").write_text(json.dumps(composition))
    module.validate_session(session)
    for value in ("other-submission", ""):
        (session / "submission-id").write_text(value + "\n")
        refuses(lambda: module.validate_session(session))
    (session / "submission-id").write_text("saved-submission\n")
    for artifact in ("submit.json", "composition-manifest.json"):
        saved = (session / artifact).read_text()
        changed = json.loads(saved)
        target = changed if artifact == "submit.json" else changed["identity"]
        target["submission_id"] = "other-submission"
        (session / artifact).write_text(json.dumps(changed))
        refuses(lambda: module.validate_session(session))
        (session / artifact).write_text(saved)
    for file, bad in (("status", "walkthrough"), ("state-root", "/other"),
                      ("listener", "127.0.0.1:9999"), ("implementation-run", "run-other"),
                      ("implementation-invocation", "inv-other")):
        saved = (session / file).read_text()
        (session / file).write_text(bad)
        refuses(lambda: module.validate_session(session))
        (session / file).write_text(saved)
    composition["identity"]["implementation_run_id"] = "run-other"
    (session / "composition-manifest.json").write_text(json.dumps(composition))
    refuses(lambda: module.validate_session(session))
    composition["identity"]["implementation_run_id"] = "run-retained"
    (session / "composition-manifest.json").write_text(json.dumps(composition))
    (session / "retained-composition.json").write_text(json.dumps(composition))
    (session / "composition-manifest.json").write_text('{}')
    (session / "status").write_text('recovery-required')
    refuses(lambda: module.validate_session(session))
    (session / "runtime-upgrade-started").touch()
    refuses(lambda: module.validate_session(session))
    (session / "rig-release-verified").touch()
    module.validate_session(session)
    (state / "freeside.db").unlink()
    refuses(lambda: module.validate_session(session))
    content = session / 'approved-input'
    content.write_text('private credential fixture')
    names = ['FREESIDE_REAL_RUN_PROMPT_PACKAGE', 'FREESIDE_REAL_RUN_SPECIFICATION_PROMPT_PACKAGE',
             'FREESIDE_REAL_RUN_REMEDIATION_PROMPT_PACKAGE', 'FREESIDE_REAL_RUN_INSTRUCTIONS',
             'FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT', 'FREESIDE_REAL_RUN_REVIEW_INSTRUCTIONS',
             'FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT', 'FREESIDE_REAL_RUN_BUILD_PROXY']
    for name in names:
        os.environ[name] = str(content)
    os.environ['FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT'] = str(session)
    inputs = [str(content)] * 3 + ['']
    receipt = module.upgrade_receipt('reviewed-version', inputs, names)
    assert receipt == module.upgrade_receipt('reviewed-version', inputs, names)
    assert receipt != module.upgrade_receipt('changed-version', inputs, names)
    assert 'private credential fixture' not in json.dumps(receipt)
    for name in names:
        saved = os.environ[name]
        if name == 'FREESIDE_REAL_RUN_BUILD_PROXY':
            os.environ[name] = 'changed'
            assert receipt != module.upgrade_receipt('reviewed-version', inputs, names)
            os.environ[name] = saved
    content.write_text('changed credential fixture')
    assert receipt != module.upgrade_receipt('reviewed-version', inputs, names)
    judgment_bin = session / 'claude'
    judgment_token = session / 'judgment-token'
    judgment_bin.write_text('pinned CLI fixture')
    judgment_token.write_text('private subscription fixture')
    extra = {'FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN': str(judgment_bin),
             'FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_SHA256': 'pin',
             'FREESIDE_REAL_RUN_JUDGMENT_MODEL': 'model',
             'FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT': judgment_token.name}
    os.environ.update(extra)
    names.extend(extra)
    receipt = module.upgrade_receipt('reviewed-version', inputs, names)
    assert 'private subscription fixture' not in json.dumps(receipt)
    for path in (judgment_bin, judgment_token):
        saved = path.read_text()
        path.write_text('changed')
        assert receipt != module.upgrade_receipt('reviewed-version', inputs, names)
        path.write_text(saved)
    os.environ['FREESIDE_REAL_RUN_JUDGMENT_MODEL'] = 'another'
    assert receipt != module.upgrade_receipt('reviewed-version', inputs, names)
with tempfile.TemporaryDirectory() as temp:
    root = Path(temp)
    operator = root / 'operator.json'
    operator.write_text('{"version":1,"projects":[]}')
    original = root / 'original'
    original.mkdir()
    snapshot = module.stage_manual_submission(str(operator), '', original)
    assert Path(snapshot).read_bytes() == operator.read_bytes()
    # New input is a distinct receipt dimension; old receipts remain compatible.
    names = ['FREESIDE_REAL_RUN_PROMPT_PACKAGE', 'FREESIDE_REAL_RUN_SPECIFICATION_PROMPT_PACKAGE',
             'FREESIDE_REAL_RUN_REMEDIATION_PROMPT_PACKAGE', 'FREESIDE_REAL_RUN_INSTRUCTIONS',
             'FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT', 'FREESIDE_REAL_RUN_REVIEW_INSTRUCTIONS',
             'FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT']
    for name in names:
        os.environ[name] = str(operator)
    os.environ['FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT'] = str(root)
    old = module.upgrade_receipt('version', [str(operator)] * 3 + [''], names)
    configured = module.upgrade_receipt('version', [str(operator)] * 3 + [''], names, snapshot)
    assert 'manual_submission_digest' not in old
    assert configured != old
    # A reviewed completed-session restart may replace or add configuration.
    (original / 'status').write_text('completed')
    (original / 'runtime-upgrade-started').touch()
    operator.write_text('{"version":1,"projects":[]}\n')
    replaced = root / 'replaced'
    replaced.mkdir()
    replacement = module.stage_manual_submission(str(operator), str(original), replaced)
    assert Path(replacement).read_bytes() == operator.read_bytes()
    assert Path(snapshot).read_bytes() != operator.read_bytes()
    legacy = root / 'legacy'
    legacy.mkdir()
    added = root / 'added'
    added.mkdir()
    assert module.stage_manual_submission(str(operator), str(legacy), added)
    # Interrupted upgrades cannot change bytes, add a file, or lose a snapshot.
    (original / 'status').write_text('recovery-required')
    refuses(lambda: module.stage_manual_submission(str(operator), str(original), root / 'bad'))
    same = root / 'same'
    same.mkdir()
    assert module.stage_manual_submission('', str(original), same)
    # A retry that failed before opening writable still inherits the original
    # interrupted upgrade's input constraint.
    (original / 'runtime-upgrade-started').rename(original / 'runtime-upgrade-inherited')
    refuses(lambda: module.stage_manual_submission(str(operator), str(original), root / 'changed-inherited'))
    inherited = root / 'inherited'
    inherited.mkdir()
    assert module.stage_manual_submission('', str(original), inherited)
    saved = Path(snapshot).read_bytes()
    Path(snapshot).write_text('tampered')
    refuses(lambda: module.manual_submission_path(original))
    refuses(lambda: module.stage_manual_submission('', str(original), root / 'bad2'))
    assert configured != module.upgrade_receipt('version', [str(operator)] * 3 + [''], names, snapshot)
    Path(snapshot).unlink()
    refuses(lambda: module.manual_submission_path(original))
    refuses(lambda: module.stage_manual_submission('', str(original), root / 'bad3'))
    refuses(lambda: module.upgrade_receipt('version', [str(operator)] * 3 + [''], names, snapshot))
    (legacy / 'status').write_text('recovery-required')
    (legacy / 'runtime-upgrade-started').touch()
    refuses(lambda: module.stage_manual_submission(str(operator), str(legacy), root / 'bad4'))
    (legacy / 'runtime-upgrade-started').rename(legacy / 'runtime-upgrade-inherited')
    refuses(lambda: module.stage_manual_submission(str(operator), str(legacy), root / 'added-inherited'))
    still_disabled = root / 'still-disabled'
    still_disabled.mkdir()
    assert module.stage_manual_submission('', str(legacy), still_disabled) == ''
    Path(snapshot).write_bytes(saved)
    (original / 'manual-submission-input.json').unlink()
    refuses(lambda: module.manual_submission_path(original))
    operator.write_bytes(b'x' * ((4 << 20) + 1))
    refuses(lambda: module.stage_manual_submission(str(operator), '', root / 'too-large'))
import subprocess
with tempfile.TemporaryDirectory() as temp:
    # A client-target session binds the retained run and invocation to the
    # selected target.json, while submit.json/composition remain the ignored
    # seed the composition approved.
    session = Path(temp)
    state = session / "state"
    state.mkdir()
    (state / "freeside.db").touch()
    seed = session / "seed"
    os.environ.update(FREESIDE_REAL_RUN_STATE_ROOT=str(state),
                      FREESIDE_REAL_RUN_LISTEN="127.0.0.1:8733",
                      FREESIDE_REAL_RUN_SEED_ROOT=str(seed))
    (session / "status").write_text("completed\n")
    (session / "state-root").write_text(str(state) + "\n")
    (session / "listener").write_text("127.0.0.1:8733\n")
    (session / "rig-acquisition.json").write_text(
        json.dumps({"manifest": {"resources": {"seed_root": str(seed)}}}))
    (session / "mode").write_text("client-target\n")
    (session / "submit.json").write_text(json.dumps({
        "run_id": "run-seed", "implementation_invocation_id": "inv-implement-run-seed",
        "submission_id": "seed-submission"}))
    (session / "composition-manifest.json").write_text(json.dumps({"identity": {
        "implementation_run_id": "run-seed", "implementation_invocation_id": "inv-implement-run-seed",
        "submission_id": "seed-submission"}}))
    (session / "submission-id").write_text("seed-submission\n")
    (session / "target.json").write_text(json.dumps({
        "task_id": "task-client", "project": "proj",
        "specification_run_id": "run-specification-target",
        "implementation_run_id": "run-target",
        "implementation_invocation_id": "inv-implement-run-target"}))
    (session / "implementation-run").write_text("run-target\n")
    (session / "implementation-invocation").write_text("inv-implement-run-target\n")
    module.validate_session(session)
    assert module.session_mode(session) == "client-target"

    def identity(key):
        out = subprocess.run([sys.executable, sys.argv[1], "identity", str(session), key],
                             capture_output=True, text=True, check=True)
        return out.stdout.strip()
    assert identity("run_id") == "run-target"
    assert identity("implementation_invocation_id") == "inv-implement-run-target"
    assert identity("specification_run_id") == "run-specification-target"

    saved_target = (session / "target.json").read_text()
    (session / "target.json").unlink()
    refuses(lambda: module.validate_session(session))
    (session / "target.json").write_text(saved_target)
    for artifact in ("implementation-run", "implementation-invocation"):
        saved = (session / artifact).read_text()
        (session / artifact).write_text("run-seed\n")
        refuses(lambda: module.validate_session(session))
        (session / artifact).write_text(saved)
    comp = json.loads((session / "composition-manifest.json").read_text())
    comp["identity"]["implementation_run_id"] = "run-other"
    (session / "composition-manifest.json").write_text(json.dumps(comp))
    refuses(lambda: module.validate_session(session))
print("PASS: retained identities/composition and exact remote publication checks")
PY
