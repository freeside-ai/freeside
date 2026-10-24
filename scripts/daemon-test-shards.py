#!/usr/bin/env python3
"""Discover, partition, and account for the daemon's default Go test families."""

import argparse
from collections import Counter
import hashlib
import json
import math
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent

# The daemon tests whose behaviour differs on macOS, the only ones a PR runs
# there; the full rest shard runs on macOS after the merge
# (devlog/2026-10-24-1255-macos-darwin-selection.md). A package is relative
# to the daemon module. `families` names top-level tests, or None takes the
# whole package, which suits only cheap packages. A listed package or family
# that no longer exists fails the shard: update the list, never drop the check.
# A new darwin-sensitive test is covered on PRs only once it is listed here.
DARWIN_SELECTION = [
    {"package": "internal/atomicfile", "families": None,
     "reason": "build tag: rename_noreplace_darwin.go"},
    {"package": "internal/operations", "families": None,
     "reason": "filesystem: setup resolves /var to /private"},
    {"package": "cmd/freeside-signet-dev", "families": None,
     "reason": "filesystem and network: case-folded store path, loopback-only listener"},
    {"package": "internal/export", "families": None,
     "reason": "build tag: *_unix_test.go"},
    {"package": "cmd/freesided", "families": [
        "TestPairingControlRejectsUnauthorizedPeers",
        "TestSetupCommandKeepsRegistrationCodeOutOfProcessArgumentsAndOutput",
        "TestUnderRootAcceptsAPFSFirmlinkAlias", "TestEnvironmentPaths",
        "TestListenPrivileged", "TestListenPrivilegedAcceptsOnlyTailscaleOwnedAddresses",
        "TestConnectionModeOfBoundListener", "TestLoopbackTwinAddr", "TestListenLoopbackTwin"],
     "reason": "build tag (pairing_peer_darwin.go, setup_process_other_test.go), "
               "filesystem (APFS firmlinks), network (loopback and Tailscale binds)"},
    {"package": "internal/publish", "families": [
        "TestNewKeystoreRejectsOverlap", "TestKeystoreWritesStayOutsideStateDir",
        "TestRejectMaterializationFilesystemCollisions",
        "TestMaterializationVerificationUsesRootedPaths"],
     "reason": "filesystem: case-insensitive APFS and rooted paths"},
    {"package": "internal/verify", "families": [
        "TestMaterializeRejectsCaseFoldingSymlinkCollision"],
     "reason": "filesystem: case-insensitive APFS"},
    {"package": "internal/importer", "families": [
        "TestDetectCollisions", "TestDetectCollisionsFileDirectory",
        "TestFoldedComponentsFullCaseFold", "TestDetectCollisionsFullFold",
        "TestDetectCollisionsIgnoresModifies", "TestDetectCollisionsDeterministicPartner",
        "TestImportCaseCollisionBlocksNothingButFlags",
        "TestCommitPlanPreboundsCaseFoldedDuplicateKeys",
        "TestManifestEntryCountCaseInsensitive"],
     "reason": "filesystem: case-insensitive APFS collision checks"},
    {"package": "internal/ward", "families": [
        "TestConnectProxyRegistryRequiresPublicAddresses", "TestPublicUnicast",
        "TestConnectProxyClientSubnetAdmission",
        "TestPromptScriptsHandleFreshVolumeAndRejectUnexpectedContents",
        "TestComposeClaudeInstructionsUsesRootedPaths", "TestStageSeedSourceUsesRootedPaths"],
     "reason": "network (egress.go bind rules), filesystem (runtime.GOOS path branches)"},
    {"package": "internal/exec/claude", "families": [
        "TestRuntimeDependencyCleanupDoesNotFollowReplacementSymlink"],
     "reason": "filesystem: the non-Linux mv branch"},
]


def require(condition, message):
    if not condition:
        raise ValueError(message)


def list_packages(go, daemon):
    packages = subprocess.check_output([go, "list", "./..."], cwd=daemon, text=True).splitlines()
    require(packages and len(packages) == len(set(packages)), "empty or duplicate packages")
    require(all(re.fullmatch(r"[\w./-]+", p) for p in packages), "malformed package inventory")
    return packages


def discovery_events(events, packages):
    inventory = {p: [] for p in packages}
    completed = set()
    for event in events:
        require(isinstance(event, dict), "discovery event is not an object")
        package = event.get("Package")
        require(package in inventory, "discovery returned an unknown package")
        require(event.get("Action") != "fail", "compiled discovery failed")
        if event.get("Action") in ("pass", "skip") and not event.get("Test"):
            require(package not in completed, "duplicate discovery completion")
            completed.add(package)
        for name in event.get("Output", "").splitlines():
            if name.startswith(("Test", "Example", "Fuzz")):
                require(name.isidentifier(), f"malformed discovered family: {name!r}")
                inventory[package].append(name)
    require(completed == set(packages), "incomplete compiled discovery")
    for names in inventory.values():
        require(len(names) == len(set(names)), "duplicate discovered family")
        names.sort()
    return inventory


def discover(go, daemon, integration_only=False, *, packages=None, rest_only=False, only=None):
    packages = list_packages(go, daemon) if packages is None else packages
    if integration_only:
        packages = [p for p in packages if p.endswith("/internal/integration")]
        require(len(packages) == 1, "missing or ambiguous integration package")
    if rest_only:
        packages = [p for p in packages if not p.endswith("/internal/integration")]
    if only is not None:
        require(set(only) <= set(packages), "discovery selection names an unknown package")
        packages = list(only)
    require(packages, "empty discovery selection")
    subset = integration_only or rest_only or only is not None
    listing = subprocess.check_output(
        [go, "test", "-json", "-list", "."] + (packages if subset else ["./..."]),
        cwd=daemon, text=True
    )
    return discovery_events((json.loads(line) for line in listing.splitlines()), packages)


def darwin_packages(packages):
    """Map each DARWIN_SELECTION entry to its import path and families."""
    selection = {}
    for entry in DARWIN_SELECTION:
        matches = [p for p in packages if p.endswith("/" + entry["package"])]
        require(len(matches) == 1, f"missing or ambiguous darwin package: {entry['package']}")
        # A repeated entry would silently replace the earlier one's families.
        require(matches[0] not in selection, f"duplicate darwin package: {entry['package']}")
        selection[matches[0]] = entry["families"]
    return selection


def darwin_families(selection, inventory):
    selected = {}
    for package, families in selection.items():
        if families is None:
            selected[package] = inventory[package]
            continue
        missing = sorted(set(families) - set(inventory[package]))
        require(not missing, f"darwin families not discovered in {package}: {', '.join(missing)}")
        selected[package] = sorted(families)
    return selected


def run_pattern(names):
    return "^(" + "|".join(re.escape(n) for n in names) + ")$"


def partition(names, count, weights):
    require(type(count) is int and count > 0, "shard count must be positive")
    require(names and len(names) == len(set(names)), "empty or duplicate families")
    require(isinstance(weights, dict), "weights must be an object")
    require(all(isinstance(k, str) and type(v) in (int, float) and math.isfinite(v) and v > 0
                for k, v in weights.items()), "weights must be finite positive seconds")
    bins = [[] for _ in range(count)]
    totals = [0.0] * count
    # Removed entries are harmless hints; new families get a unit weight.
    for name in sorted(names, key=lambda n: (-weights.get(n, 1), n)):
        index = min(range(count), key=lambda i: (totals[i], i))
        bins[index].append(name)
        totals[index] += weights.get(name, 1)
    validate_partition(names, bins)
    return [sorted(b) for b in bins]


def validate_partition(names, bins):
    require(all(bins), "empty shard selection")
    require(Counter(n for b in bins for n in b) == Counter(names),
            "shards must cover each discovered family exactly once")


def account(events, inventory):
    runs = Counter()
    cases = {}
    packages = {}
    for event in events:
        require(isinstance(event, dict), "execution event is not an object")
        package = event.get("Package")
        require(package in inventory, "execution returned an unselected package")
        name = event.get("Test")
        action = event.get("Action")
        require(action in ("start", "run", "pause", "cont", "output", "pass", "skip", "fail"),
                "unknown execution action")
        seconds = event.get("Elapsed", 0)
        require(type(seconds) in (int, float) and math.isfinite(seconds) and seconds >= 0,
                "invalid elapsed time")
        if name:
            require(name.split("/", 1)[0] in inventory[package],
                    f"execution returned an unselected family: {name}")
            key = (package, name)
            if action == "run":
                runs[key] += 1
            if action in ("pass", "skip", "fail"):
                require(key not in cases, "duplicate test outcome")
                require(action != "fail", f"test failed: {package}/{name}")
                cases[key] = {"package": package, "name": name, "outcome": action,
                              "seconds": event.get("Elapsed", 0)}
        elif action in ("pass", "skip", "fail"):
            require(package not in packages and action != "fail", "failed or duplicate package outcome")
            packages[package] = {"outcome": action, "seconds": event.get("Elapsed", 0)}
    require(set(packages) == set(inventory), "missing package completion")
    expected = {(p, n) for p, names in inventory.items() for n in names}
    require(expected <= set(cases), "missing discovered family outcome")
    require(set(runs) == set(cases) and all(n == 1 for n in runs.values()),
            "missing, duplicate, or incomplete test execution")
    return {"packages": packages, "cases": [cases[k] for k in sorted(cases)]}


def write_json(directory, name, value):
    if directory:
        (directory / name).write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def run(args):
    require(args.count > 0 and 1 <= args.index <= args.count, "invalid shard index/count")
    require(args.integration_count is None or args.integration_count > 0,
            "integration shard count must be positive")
    require(args.kind == "integration" or (args.index, args.count) == (1, 1),
            f"{args.kind} shard requires index=1 and count=1")
    require(args.kind != "integration" or args.integration_count in (None, args.count),
            "conflicting integration shard counts")
    directory = args.evidence_dir
    if directory:
        directory.mkdir(parents=True, exist_ok=True)
        # A report only becomes evidence after this invocation accounts for every family.
        (directory / "execution.json").unlink(missing_ok=True)
    packages = list_packages(args.go, args.daemon)
    write_json(directory, "packages.json", packages)
    darwin = darwin_packages(packages) if args.kind == "darwin" else None
    inventory = discover(args.go, args.daemon, args.kind == "integration",
                         packages=packages, rest_only=args.kind == "rest",
                         only=list(darwin) if args.kind == "darwin" else None)
    integration = [p for p in packages if p.endswith("/internal/integration")]
    require(len(integration) == 1, "missing or ambiguous integration package")
    package = integration[0]
    count = args.count if args.kind == "integration" else (args.integration_count or 3)
    bins = []
    if args.kind == "integration":
        weights = json.loads(args.weights.read_text())
        bins = partition(inventory[package], count, weights)
        selected = {package: bins[args.index - 1]}
    elif args.kind == "darwin":
        selected = darwin_families(darwin, inventory)
    else:
        selected = inventory
    require(selected, "empty package selection")
    write_json(directory, "inventory.json", inventory)
    write_json(directory, "partition.json", {"count": count, "integration": bins})
    base = [args.go, "test", "-json", "-count=1"]
    if args.kind == "integration":
        commands = [base + ["-run", run_pattern(selected[package]), package]]
    elif args.kind == "darwin":
        # One -run applies to every package in a command, and accounting
        # rejects an unselected family, so each filtered package runs alone.
        whole = [p for p, families in darwin.items() if families is None]
        commands = ([base + whole] if whole else []) + [
            base + ["-run", run_pattern(selected[p]), p]
            for p, families in darwin.items() if families is not None]
    else:
        commands = [base + list(selected)]
    print(f"shard {args.kind} {args.index}/{args.count}: {len(selected)} packages", file=sys.stderr)
    evidence = (directory / "tests.jsonl").open("w") if directory else None
    events = []
    invalid = None
    code = 0
    try:
        for command in commands:
            with subprocess.Popen(command, cwd=args.daemon, stdout=subprocess.PIPE, text=True) as child:
                for line in child.stdout:
                    sys.stdout.write(line)
                    if evidence:
                        evidence.write(line)
                    try:
                        events.append(json.loads(line))
                    except ValueError as error:
                        invalid = error
                code = child.wait()
            if code:
                break
    finally:
        if evidence:
            evidence.close()
    if code:
        return code if code > 0 else 128 - code
    require(invalid is None, "malformed JSON execution output")
    report = account(events, selected)
    report.update({"kind": args.kind, "index": args.index, "count": args.count,
                   "integration_count": count, "commands": commands, "inventory": selected})
    # The commands do not show the width; GOFLAGS reaches go test through the environment.
    report["goflags"] = os.environ.get("GOFLAGS", "")
    write_json(directory, "execution.json", report)
    print(f"accounted for {len(report['cases'])} default test cases", file=sys.stderr)
    return 0


def read_json(path):
    return json.loads(path.read_text())


def read_events(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


def run_attempt(identity):
    value = identity.get("GITHUB_RUN_ATTEMPT")
    if value == "":
        return 0  # Local evidence has no CI attempt.
    require(isinstance(value, str) and re.fullmatch(r"[1-9][0-9]*", value), "invalid run attempt")
    return int(value)


def latest_attempts(paths):
    # A failed-jobs rerun reruns only the failed shards, so the shards that
    # passed keep their evidence in an earlier attempt. Each shard counts its
    # latest attempt: the aggregator's gate already required every shard job
    # to succeed, and a successful job uploads its own attempt's evidence.
    candidates = {}
    for path in paths:
        report = read_json(path)
        attempt = run_attempt(read_json(path.parent / "metadata.json"))
        candidates.setdefault((report["kind"], report["index"], report["count"]), []).append(
            (attempt, path, report))
    kept = []
    for shard in candidates.values():
        attempts = [attempt for attempt, _, _ in shard]
        require(len(set(attempts)) == len(attempts), "duplicate shard evidence in one attempt")
        kept.append(max(shard, key=lambda candidate: candidate[0]))
    if os.environ.get("GITHUB_RUN_ATTEMPT"):
        current = int(os.environ["GITHUB_RUN_ATTEMPT"])
        require(all(attempt <= current for attempt, _, _ in kept), "evidence belongs to a later attempt")
    return [path for _, path, _ in kept], [report for _, _, report in kept]


def verify_reports(paths, shards="all"):
    # A platform's shard set is declared by the workflow: PRs run the darwin
    # selection alone on macOS and a push to main runs every shard there, so
    # each gate verifies full coverage of its declared set and still rejects
    # a shard outside it.
    require(shards in ("all", "rest", "integration", "darwin"), "invalid shard selection")
    require(paths, "missing shard reports")
    paths, reports = latest_attempts(paths)
    rest = [r for r in reports if r["kind"] == "rest"]
    if shards in ("integration", "darwin"):
        require(not rest, f"{shards}-only verification rejects a rest shard")
        count = reports[0]["integration_count"]
    else:
        require(len(rest) == 1, "require exactly one rest shard")
        count = rest[0]["integration_count"]
    require(type(count) is int and count > 0, "invalid integration count")
    if shards == "darwin":
        expected = [("darwin", 1, 1)]
    else:
        expected = [("rest", 1, 1)] if shards != "integration" else []
        if shards != "rest":
            expected += [("integration", i, count) for i in range(1, count + 1)]
    require(Counter((r["kind"], r["index"], r["count"]) for r in reports) == Counter(expected),
            "missing, duplicate, or unexpected shard")
    packages = read_json(paths[0].parent / "packages.json")
    require(isinstance(packages, list) and packages and len(set(packages)) == len(packages),
            "invalid package inventory")
    integration = [p for p in packages if p.endswith("/internal/integration")]
    require(len(integration) == 1, "missing or ambiguous integration package")
    package = integration[0]
    identity = read_json(paths[0].parent / "metadata.json")
    require(isinstance(identity, dict) and all(identity.get(k) for k in
            ("tested_commit", "production_tree", "go", "platform")), "missing tested identity")
    if os.environ.get("GITHUB_RUN_ID"):
        require(identity.get("GITHUB_RUN_ID") == os.environ["GITHUB_RUN_ID"], "evidence belongs to another run")
    if os.environ.get("GITHUB_SHA"):
        require(identity["tested_commit"] == os.environ["GITHUB_SHA"], "evidence tested another commit")
    inventory, cases, partitions = {}, {}, []
    for path, report in zip(paths, reports):
        directory = path.parent
        other = read_json(directory / "metadata.json")
        require(all(other.get(k) == identity.get(k) for k in
                    ("tested_commit", "production_tree", "go", "platform", "PR_HEAD", "PR_BASE",
                     "GITHUB_RUN_ID", "ImageOS")),
                "evidence identities differ")
        require(read_json(directory / "packages.json") == packages, "inconsistent package inventory")
        require(report["integration_count"] == count, "inconsistent shard count")
        discovered = read_json(directory / "inventory.json")
        selected = report["inventory"]
        if report["kind"] == "rest":
            require(set(discovered) == set(packages) - {package} and selected == discovered,
                    "rest shard omitted a package or family")
        elif report["kind"] == "darwin":
            darwin = darwin_packages(packages)
            require(set(discovered) == set(darwin) and selected == darwin_families(darwin, discovered),
                    "darwin shard omitted a package or family")
        else:
            require(set(discovered) == {package}, "invalid integration inventory")
            assignment = read_json(directory / "partition.json")
            require(assignment["count"] == count and len(assignment["integration"]) == count,
                    "invalid partition count")
            bins = assignment["integration"]
            validate_partition(discovered[package], bins)
            require(selected == {package: bins[report["index"] - 1]}, "stale or incorrect assignment")
            partitions.append(bins)
        for p, names in discovered.items():
            require(p not in inventory or inventory[p] == names, "inconsistent compiled discovery")
            inventory[p] = names
        # Re-account raw events instead of trusting a saved success or case list.
        actual = account(read_events(directory / "tests.jsonl"), selected)
        require(actual["cases"] == report["cases"] and actual["packages"] == report["packages"],
                "execution report differs from raw events")
        for case in actual["cases"]:
            key = (case["package"], case["name"])
            require(key not in cases, "duplicate shard case")
            cases[key] = case["outcome"]
    require(all(p == partitions[0] for p in partitions), "inconsistent shard partitions")
    if shards == "darwin":
        covered = set(darwin_packages(packages))
    else:
        covered = {"all": set(packages), "rest": set(packages) - {package}, "integration": {package}}[shards]
    require(set(inventory) == covered, "missing package discovery")
    print(f"PASS: accounted for {len(cases)} cases in {len(packages)} packages")
    return inventory, cases, identity


def compare(args):
    inventory, observed, identity = verify_reports(args.reports)
    # The independent compiled baseline inventory catches omissions shared by
    # the candidate's discovery and execution, including a whole package.
    baseline_packages = (args.baseline.parent / "packages.txt").read_text().splitlines()
    require(baseline_packages and len(set(baseline_packages)) == len(baseline_packages),
            "invalid baseline package inventory")
    expected_inventory = discovery_events(
        read_events(args.baseline.parent / "discovery.jsonl"), baseline_packages)
    baseline_identity = read_json(args.baseline.parent / "metadata.json")
    require(all(identity.get(k) and identity[k] == baseline_identity.get(k) for k in
                ("production_tree", "go", "platform", "ImageOS")),
            "baseline production, toolchain, or runner family differs")
    require(expected_inventory == inventory, "compiled inventories differ from baseline")
    baseline = account(read_events(args.baseline), expected_inventory)
    expected = {(c["package"], c["name"]): c["outcome"] for c in baseline["cases"]}
    require(expected == observed, "shard cases/outcomes differ from successful baseline")
    print(f"PASS: preserved all {len(expected)} cases and {len(inventory)} packages")


def metadata(args):
    def output(*command):
        directory = ROOT / "daemon" if command[0] == "go" else ROOT
        return subprocess.check_output(command, cwd=directory, text=True).strip()
    tree = output("git", "ls-tree", "-r", "HEAD", "daemon")
    tree = "\n".join(line for line in tree.splitlines() if not line.endswith("\tdaemon/README.md"))
    value = {"tested_commit": output("git", "rev-parse", "HEAD"),
             "production_tree": hashlib.sha256(tree.encode()).hexdigest(),
             "go": output("go", "version"), "git": output("git", "--version"),
             "platform": output("go", "env", "GOOS", "GOARCH").splitlines()}
    for key in ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "PR_HEAD", "PR_BASE", "ImageOS",
                "ImageVersion", "CACHE_HIT", "CACHE_KEY"):
        value[key] = os.environ.get(key, "")
    args.evidence_dir.mkdir(parents=True, exist_ok=True)
    write_json(args.evidence_dir, "metadata.json", value)


def gate(needs, expected, skipped=()):
    # A child that runs only on the other event (the macOS push job on a PR)
    # must be present and skipped, never absent, failed, or quietly run.
    require(set(needs) == set(expected) | set(skipped) and not set(expected) & set(skipped),
            "missing or unexpected required child job")
    require(all(isinstance(value, dict) for value in needs.values()), "malformed child job result")
    require(all(needs[name].get("result") == "success" for name in expected),
            "every required child job must succeed")
    require(all(needs[name].get("result") == "skipped" for name in skipped),
            "every other-event child job must be skipped")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    shard = commands.add_parser("run")
    shard.add_argument("--kind", choices=("rest", "integration", "darwin"), required=True)
    shard.add_argument("--index", type=int, required=True, help="one-based shard index")
    shard.add_argument("--count", type=int, required=True)
    shard.add_argument("--integration-count", type=int)
    shard.add_argument("--go", default="go")
    shard.add_argument("--daemon", type=Path, default=ROOT / "daemon")
    shard.add_argument("--weights", type=Path, default=ROOT / "scripts/daemon-test-shards.json")
    shard.add_argument("--evidence-dir", type=Path)
    comparison = commands.add_parser("compare")
    comparison.add_argument("--baseline", type=Path, required=True)
    comparison.add_argument("reports", type=Path, nargs="+")
    verification = commands.add_parser("verify")
    verification.add_argument("--shards", choices=("all", "rest", "integration", "darwin"), default="all",
                              help="the shard set the workflow declares for this gate")
    verification.add_argument("reports", type=Path, nargs="+")
    identity = commands.add_parser("metadata")
    identity.add_argument("--evidence-dir", type=Path, required=True)
    final = commands.add_parser("gate")
    final.add_argument("--needs", required=True)
    final.add_argument("--expected", nargs="+", required=True)
    final.add_argument("--skipped", nargs="*", default=[], help="children that must have been skipped")
    args = parser.parse_args()
    try:
        if args.command == "run":
            return run(args)
        if args.command == "compare":
            compare(args)
        elif args.command == "verify":
            verify_reports(args.reports, args.shards)
        elif args.command == "metadata":
            metadata(args)
        else:
            gate(json.loads(args.needs), args.expected, args.skipped)
    except (ValueError, KeyError, TypeError, OSError, subprocess.CalledProcessError) as error:
        print(f"daemon-test-shards: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
