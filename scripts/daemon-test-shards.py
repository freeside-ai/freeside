#!/usr/bin/env python3
"""Discover, partition, and account for the daemon's default Go test families."""

import argparse
from collections import Counter
import json
import math
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent


def require(condition, message):
    if not condition:
        raise ValueError(message)


def discover(go, daemon, integration_only=False):
    packages = subprocess.check_output([go, "list", "./..."], cwd=daemon, text=True).splitlines()
    require(packages and len(packages) == len(set(packages)), "empty or duplicate packages")
    require(all(re.fullmatch(r"[\w./-]+", p) for p in packages), "malformed package inventory")
    if integration_only:
        packages = [p for p in packages if p.endswith("/internal/integration")]
        require(len(packages) == 1, "missing or ambiguous integration package")
    listing = subprocess.check_output(
        [go, "test", "-json", "-list", "."] + (packages if integration_only else ["./..."]),
        cwd=daemon, text=True
    )
    inventory = {p: [] for p in packages}
    completed = set()
    for line in listing.splitlines():
        event = json.loads(line)
        package = event.get("Package")
        require(package in inventory, "discovery returned an unknown package")
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
        package = event.get("Package")
        require(package in inventory, "execution returned an unselected package")
        name = event.get("Test")
        action = event.get("Action")
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
    require(args.kind != "rest" or (args.index, args.count) == (1, 1),
            "rest shard requires index=1 and count=1")
    require(args.kind != "integration" or args.integration_count in (None, args.count),
            "conflicting integration shard counts")
    directory = args.evidence_dir
    if directory:
        directory.mkdir(parents=True, exist_ok=True)
        # A report only becomes evidence after this invocation accounts for every family.
        (directory / "execution.json").unlink(missing_ok=True)
    inventory = discover(args.go, args.daemon, args.kind == "integration")
    integration = [p for p in inventory if p.endswith("/internal/integration")]
    require(len(integration) == 1, "missing or ambiguous integration package")
    package = integration[0]
    weights = json.loads(args.weights.read_text())
    count = args.count if args.kind == "integration" else (args.integration_count or 2)
    bins = partition(inventory[package], count, weights)
    selected = ({package: bins[args.index - 1]} if args.kind == "integration" else
                {p: names for p, names in inventory.items() if p != package})
    require(selected, "empty package selection")
    write_json(directory, "inventory.json", inventory)
    write_json(directory, "partition.json", {"count": count, "integration": bins})
    command = [args.go, "test", "-json", "-count=1"]
    if args.kind == "integration":
        command += ["-run", "^(" + "|".join(re.escape(n) for n in selected[package]) + ")$"]
    command += list(selected)
    print(f"shard {args.kind} {args.index}/{args.count}: {len(selected)} packages", file=sys.stderr)
    evidence = (directory / "tests.jsonl").open("w") if directory else None
    events = []
    invalid = None
    try:
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
    finally:
        if evidence:
            evidence.close()
    if code:
        return code if code > 0 else 128 - code
    require(invalid is None, "malformed JSON execution output")
    report = account(events, selected)
    report.update({"kind": args.kind, "index": args.index, "count": args.count,
                   "command": command, "inventory": selected})
    write_json(directory, "execution.json", report)
    print(f"accounted for {len(report['cases'])} default test cases", file=sys.stderr)
    return 0


def compare(args):
    baseline = [json.loads(line) for line in args.baseline.read_text().splitlines()]
    # Baseline is unfiltered default execution; names include all nested cases and fuzz seeds.
    expected = {(e["Package"], e["Test"]): e["Action"] for e in baseline
                if e.get("Test") and e["Action"] in ("pass", "skip")}
    require(expected, "empty baseline execution")
    require(not any(e["Action"] == "fail" for e in baseline), "failed baseline execution")
    observed = {}
    package_counts = Counter()
    for path in args.reports:
        report = json.loads(path.read_text())
        for package in report["packages"]:
            package_counts[package] += 1
        for case in report["cases"]:
            key = (case["package"], case["name"])
            require(key not in observed, f"duplicate shard case: {key}")
            observed[key] = case["outcome"]
    require(expected == observed, "shard cases/outcomes differ from successful baseline")
    baseline_packages = {e["Package"] for e in baseline
                         if not e.get("Test") and e["Action"] in ("pass", "skip")}
    require(set(package_counts) == baseline_packages, "shard package union differs from baseline")
    print(f"PASS: preserved all {len(expected)} cases and {len(baseline_packages)} packages")


def gate(needs, expected):
    require(set(needs) == set(expected), "missing or unexpected required child job")
    require(all(isinstance(value, dict) and value.get("result") == "success"
                for value in needs.values()), "every required child job must succeed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    shard = commands.add_parser("run")
    shard.add_argument("--kind", choices=("rest", "integration"), required=True)
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
    final = commands.add_parser("gate")
    final.add_argument("--needs", required=True)
    final.add_argument("--expected", nargs="+", required=True)
    args = parser.parse_args()
    try:
        if args.command == "run":
            return run(args)
        if args.command == "compare":
            compare(args)
        else:
            gate(json.loads(args.needs), args.expected)
    except (ValueError, KeyError, TypeError, OSError, subprocess.CalledProcessError) as error:
        print(f"daemon-test-shards: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
