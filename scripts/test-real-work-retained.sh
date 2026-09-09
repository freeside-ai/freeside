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
module.check_remote(checkpoint, observed)
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

fields = "repository repository_id base_ref base_sha profile_digest review_configuration_digest allowed_paths claude_auth_identity claude_auth_volume codex_auth_identity images identity build_egress_configuration_digest".split()
original = dict.fromkeys(fields, "unchanged")
module.compare_composition(original, dict(original, daemon_build="new", rig="new"))
for field in fields:
    refuses(lambda: module.compare_composition(original, dict(original, **{field: "changed"})))
for field in ("shadow_review_configuration_digest", "review_instructions_present",
              "review_instructions_digest"):
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
print("PASS: retained identities/composition and exact remote publication checks")
PY
