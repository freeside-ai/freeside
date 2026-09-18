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
    submission_id = submit.get("submission_id")
    if submission_id != identity.get("submission_id"):
        raise ValueError("retained submission identity disagrees with composition")
    saved_identity = session / "submission-id"
    if submission_id is not None or saved_identity.exists():
        if not saved_identity.is_file() or saved_identity.read_text().strip() != submission_id:
            raise ValueError("retained submission identity is missing or changed")
    if session_mode(session) == "client-target":
        # The seed's identity still binds submit.json to the approved
        # composition, because the composition was approved for the seed.
        for key in ("run_id", "implementation_invocation_id"):
            identity_key = "implementation_run_id" if key == "run_id" else key
            if submit[key] != identity[identity_key]:
                raise ValueError(f"retained seed {key} disagrees with original composition")
        # The retained run and invocation are the selected client target's, not
        # the seed's, so they bind to target.json. A session that never selected
        # a target is not resumable.
        target_path = session / "target.json"
        if not target_path.is_file():
            raise ValueError("client-target session has no saved target; start a new client-target session")
        target = read_json(target_path)
        for file, key in (("implementation-run", "implementation_run_id"),
                          ("implementation-invocation", "implementation_invocation_id")):
            if (session / file).read_text().strip() != target[key]:
                raise ValueError(f"retained {file} disagrees with the saved client target")
        return
    for file, key in (("implementation-run", "run_id"),
                      ("implementation-invocation", "implementation_invocation_id")):
        if (session / file).read_text().strip() != submit[key]:
            raise ValueError(f"retained {file} disagrees with original submission")
        identity_key = "implementation_run_id" if key == "run_id" else key
        if submit[key] != identity[identity_key]:
            raise ValueError(f"retained {file} disagrees with original composition")


def session_mode(session):
    mode_file = session / "mode"
    return mode_file.read_text().strip() if mode_file.is_file() else ""


def approved_composition(session):
    original = session / "retained-composition.json"
    return original if original.is_file() else session / "composition-manifest.json"


def manual_submission_path(session):
    """Verify presence and bytes before reusing a retained startup snapshot."""
    path = session / "submission-inputs/manual-submission.json"
    marker = session / "manual-submission-input.json"
    if not marker.exists():
        if path.exists():
            raise ValueError("manual submission snapshot has no input receipt")
        return ""  # Sessions predating this input remain disabled.
    receipt = read_json(marker)
    digest = hashlib.sha256(path.read_bytes()).hexdigest() if path.exists() else None
    if receipt != {"sha256": digest}:
        raise ValueError("retained manual submission configuration changed or is missing")
    return str(path) if digest is not None else ""


def stage_manual_submission(explicit, retained, destination):
    """An explicit input replaces policy only for a newly reviewed restart."""
    previous = manual_submission_path(Path(retained)) if retained else ""
    source = explicit or previous
    body = None
    if source:
        with Path(source).open("rb") as input_file:
            body = input_file.read((4 << 20) + 1)
    if body is not None and len(body) > 4 << 20:
        raise ValueError("manual submission configuration exceeds 4 MiB")
    receipt = {"sha256": hashlib.sha256(body).hexdigest() if body is not None else None}
    if (retained and ((Path(retained) / "runtime-upgrade-started").exists()
                     or (Path(retained) / "runtime-upgrade-inherited").exists())
            and (Path(retained) / "status").read_text().strip() == "recovery-required"):
        # An interrupted attempt may resume only its already-recorded input,
        # including absence. A fresh completed-session restart may replace it.
        old_marker = Path(retained) / "manual-submission-input.json"
        old = read_json(old_marker) if old_marker.exists() else {"sha256": None}
        if receipt != old:
            raise ValueError("interrupted upgrade manual submission input changed")
    directory = destination / "submission-inputs"
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    path = directory / "manual-submission.json"
    if body is not None:
        with path.open("xb") as output:
            os.fchmod(output.fileno(), 0o600)
            output.write(body)
    marker = destination / "manual-submission-input.json"
    with marker.open("x") as output:
        os.fchmod(output.fileno(), 0o600)
        output.write(json.dumps(receipt) + "\n")
    return str(path) if body is not None else ""


def upgrade_receipt(version, inputs, names, manual_config=""):
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
    if values.get("FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN"):
        paths_for_judgment = (
            Path(values["FREESIDE_REAL_RUN_JUDGMENT_CLAUDE_BIN"]),
            Path(values["FREESIDE_REAL_RUN_REVIEW_INPUT_ROOT"]) /
            values["FREESIDE_REAL_RUN_JUDGMENT_AUTH_SNAPSHOT"],
        )
        content.extend(hashlib.sha256(path.read_bytes()).hexdigest()
                       for path in paths_for_judgment)
    canonical = json.dumps({"values": values, "content": content}, sort_keys=True).encode()
    receipt = {"version": version, "inputs_digest": hashlib.sha256(canonical).hexdigest()}
    # Preserve compatibility with an older receipt when this input is absent.
    if manual_config:
        receipt["manual_submission_digest"] = hashlib.sha256(Path(manual_config).read_bytes()).hexdigest()
    return receipt


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
                  "review_instructions_digest", "judgment_configuration_digest"):
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
    if action == "stage-manual":
        print(stage_manual_submission(sys.argv[2], sys.argv[3], Path(sys.argv[4])))
    elif action == "check-manual":
        print(manual_submission_path(Path(sys.argv[2])))
    elif action == "validate":
        validate_session(Path(sys.argv[2]))
    elif action == "identity":
        session, key = Path(sys.argv[2]), sys.argv[3]
        if session_mode(session) == "client-target":
            # The resumed identity is the selected target's, from target.json.
            remap = {"run_id": "implementation_run_id"}
            value = read_json(session / "target.json").get(remap.get(key, key), "")
        else:
            value = read_json(session / "submit.json").get(key, "")
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
        names = sys.argv[8:]
        manual_config = ""
        if names[:1] == ["--manual-submission-config"]:
            manual_config, names = names[1], names[2:]
        receipt = upgrade_receipt(sys.argv[3], sys.argv[4:8], names, manual_config)
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
