#!/usr/bin/env python3
"""Read retained session coordinates and check a publication against GitHub."""

import json
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def read_json(path):
    return json.loads(Path(path).read_text())


def validate_session(session):
    status = (session / "status").read_text().strip()
    released_upgrade = (status == "recovery-required"
                        and ((session / "runtime-upgrade-started").is_file()
                             or (session / "runtime-upgrade-inherited").is_file())
                        and (session / "rig-release-verified").is_file())
    if status != "completed" and not released_upgrade:
        raise ValueError("complete/recover the old session and verify restoration first")
    for file, variable in (("state-root", "FREESIDE_REAL_RUN_STATE_ROOT"),
                           ("listener", "FREESIDE_REAL_RUN_LISTEN")):
        if (session / file).read_text().strip() != os.environ[variable]:
            raise ValueError(f"retained {file} differs from configured resource")
    resource = read_json(session / "rig-acquisition.json")["manifest"]["resources"]
    if resource["seed_root"] != os.environ["FREESIDE_REAL_RUN_SEED_ROOT"]:
        raise ValueError("retained seed root differs from configured resource")
    if not (Path(os.environ["FREESIDE_REAL_RUN_STATE_ROOT"]) / "freeside.db").is_file():
        raise ValueError("retained database is missing; refusing an empty replacement")
    submit = read_json(session / "submit.json")
    identity = read_json(approved_composition(session))["identity"]
    for file, key in (("implementation-run", "run_id"),
                      ("implementation-invocation", "implementation_invocation_id")):
        if (session / file).read_text().strip() != submit[key]:
            raise ValueError(f"retained {file} disagrees with original submission")
        identity_key = "implementation_run_id" if key == "run_id" else key
        if submit[key] != identity[identity_key]:
            raise ValueError(f"retained {file} disagrees with original composition")


def approved_composition(session):
    original = session / "retained-composition.json"
    return original if original.is_file() else session / "composition-manifest.json"


def upgrade_receipt(version, inputs, names):
    values = {name: os.environ.get(name, "") for name in names}
    paths = [Path(path) for path in inputs if path]
    for name in ("FREESIDE_REAL_RUN_PROMPT_PACKAGE",
                 "FREESIDE_REAL_RUN_SPECIFICATION_PROMPT_PACKAGE",
                 "FREESIDE_REAL_RUN_REMEDIATION_PROMPT_PACKAGE",
                 "FREESIDE_REAL_RUN_INSTRUCTIONS"):
        paths.append(Path(values[name]))
    for name in ("FREESIDE_REAL_RUN_REVIEW_AUTH_SNAPSHOT",
                 "FREESIDE_REAL_RUN_REVIEW_INSTRUCTIONS"):
        paths.append(Path(values["FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT"]) / values[name])
    content = [hashlib.sha256(path.read_bytes()).hexdigest() for path in paths]
    canonical = json.dumps({"values": values, "content": content}, sort_keys=True).encode()
    return {"version": version, "inputs_digest": hashlib.sha256(canonical).hexdigest()}


def compare_composition(old, new):
    # Build/schema/rig/observation timestamps legitimately change. These
    # approved inputs do not gain new authority through a runtime restart.
    for field in ("repository", "repository_id", "base_ref", "base_sha",
                  "profile_digest", "review_configuration_digest", "allowed_paths",
                  "claude_auth_identity", "claude_auth_volume", "codex_auth_identity",
                  "images", "identity", "build_egress_configuration_digest"):
        if old[field] != new[field]:
            raise ValueError(f"retained composition changed: {field}")
    for field in ("shadow_review_configuration_digest", "review_instructions_present",
                  "review_instructions_digest"):
        if old.get(field) != new.get(field):
            raise ValueError(f"retained composition changed: {field}")


def check_remote(checkpoint, observed):
    binding = checkpoint["binding"]
    if (observed["number"] != binding["pr_number"]
            or observed["head"]["sha"] != binding["head_sha"]
            or observed["head"]["ref"] != checkpoint["branch"]
            or observed["base"]["ref"] != binding["base_ref"]
            or observed["base"]["repo"]["id"] != binding["repository_id"]
            or observed["head"]["repo"]["id"] != binding["repository_id"]):
        raise ValueError("remote PR no longer matches the authenticated publication")
    if checkpoint["state"] == "completed":
        completion = checkpoint.get("completion")
        if not isinstance(completion, dict):
            raise ValueError("completed checkpoint has no authenticated completion")
        merge = completion.get("merge_commit_sha")
        if (not isinstance(merge, str) or not merge
                or completion.get("pr_number") != binding["pr_number"]
                or observed["state"] != "closed" or observed.get("merged") is not True
                or observed.get("merge_commit_sha") != merge):
            raise ValueError("remote merge does not match the authenticated completion")
    elif checkpoint["state"] not in ("ready", "retained") or observed["state"] != "open":
        raise ValueError("remote PR no longer matches the authenticated publication")


def main():
    action = sys.argv[1]
    if action == "validate":
        validate_session(Path(sys.argv[2]))
    elif action == "identity":
        value = read_json(Path(sys.argv[2]) / "submit.json").get(sys.argv[3], "")
        if not isinstance(value, str) or any(c.isspace() for c in value):
            raise ValueError("invalid retained run identity")
        print(value)
    elif action == "compare":
        compare_composition(read_json(sys.argv[2]), read_json(sys.argv[3]))
    elif action == "health":
        health = read_json(sys.argv[2])
        if health.get("status") != "ok" or health.get("version") != sys.argv[3]:
            raise ValueError("listener does not serve the expected healthy daemon build")
    elif action in ("receipt-write", "receipt-check"):
        receipt = upgrade_receipt(sys.argv[3], sys.argv[4:8], sys.argv[8:])
        if action == "receipt-check":
            if read_json(sys.argv[2]) != receipt:
                raise ValueError("interrupted migration inputs or reviewed build changed")
        else:
            target = Path(sys.argv[2])
            with tempfile.NamedTemporaryFile(mode="w", dir=target.parent, delete=False) as output:
                pending = Path(output.name)
                try:
                    output.write(json.dumps(receipt) + "\n")
                    output.flush()
                    os.fsync(output.fileno())
                    os.replace(pending, target)
                finally:
                    pending.unlink(missing_ok=True)
    elif action == "remote":
        checkpoint = read_json(sys.argv[2])
        binding = checkpoint["binding"]
        response = subprocess.run(
            ["gh", "api", "--method", "GET",
             f"repos/{binding['repo']}/pulls/{binding['pr_number']}"],
            check=True, capture_output=True, text=True,
        )
        check_remote(checkpoint, json.loads(response.stdout))
        print(f"Remote {checkpoint['state']} checkpoint verified: "
              f"PR #{binding['pr_number']} at head {binding['head_sha']}")
    else:
        raise ValueError("unknown retained-session operation")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        # Never echo a remote response, acquisition file, or child stderr.
        print(f"real-work-retained: {error}", file=sys.stderr)
        sys.exit(1)
