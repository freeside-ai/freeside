#!/usr/bin/env python3
"""Measure daemon tests, git calls, and competing test processes (stdlib only)."""

import argparse
from collections import Counter
import json
import os
from pathlib import Path
import platform
import shlex
import shutil
import subprocess
import sys
import tempfile
import threading
import time


def read_events(lines):
    packages, bad_lines = {}, 0
    for line in lines:
        try:
            event = json.loads(line)
            if not isinstance(event, dict):
                raise ValueError("event is not an object")
        except ValueError:
            bad_lines += 1
            continue
        name = event.get("Package")
        if not name:  # Compiler events have ImportPath instead of Package.
            continue
        package = packages.setdefault(name, {"outcome": "incomplete", "seconds": None,
                                            "tests": {}, "timeout": False})
        action, test = event.get("Action"), event.get("Test")
        if event.get("Output", "").startswith("panic: test timed out after"):
            package["timeout"] = True
        if test:
            if "/" in test:
                continue
            row = package["tests"].setdefault(test, {"outcome": "unfinished", "seconds": None})
            if action in ("pass", "fail", "skip"):
                row.update(outcome=action, seconds=event.get("Elapsed", 0))
        elif action in ("pass", "fail", "skip"):
            outcome = "build-fail" if event.get("FailedBuild") else action
            if package["timeout"]:
                outcome = "timeout"
            package.update(outcome=outcome, seconds=event.get("Elapsed", 0))
    return packages, bad_lines


def foreign_tests(process_text, own_pid):
    processes = {}
    for line in process_text.splitlines():
        fields = line.strip().split(None, 2)
        if len(fields) == 3:
            processes[int(fields[0])] = (int(fields[1]), fields[2])
    own = {own_pid}
    while True:
        children = {pid for pid, (parent, _) in processes.items() if parent in own}
        if children <= own:
            break
        own |= children
    found = []
    for pid, (_, command) in processes.items():
        words = command.split()
        program = Path(words[0]).name
        arguments = words[1:]
        if arguments[:1] == ["-C"]:
            arguments = arguments[2:]
        elif arguments and arguments[0].startswith("-C="):
            arguments = arguments[1:]
        if pid not in own and (program.endswith(".test")
                               or (program == "go" and arguments[:1] == ["test"])):
            found.append(pid)
    return found


def sample_machine():
    processes = subprocess.check_output(["ps", "-A", "-o", "pid=,ppid=,args="], text=True)
    return {"load": os.getloadavg(), "foreign": foreign_tests(processes, os.getpid())}


def write_launchers(root, git):
    wrapper = root / "wrapper"
    wrapper.write_text("#!/bin/sh\n"
                       "case \"${1-} ${2-}\" in\n"
                       "  '-c core.hooksPath=/dev/null') k=hardened ;;\n"
                       "  *) k=other ;;\n"
                       "esac\n"
                       "printf '%s\\n' \"$k\" >>\"${0%/*}/spawns\" || exit 1\n"
                       f"exec {shlex.quote(str(git))} \"$@\"\n")
    launcher = root / "launch"
    launcher.write_text("#!/bin/sh\nset -eu\n"
                        f"d=$(mktemp -d {shlex.quote(str(root / 'package.XXXXXX'))})\n"
                        f"cp {shlex.quote(str(wrapper))} \"$d/git\"\n"
                        "printf '%s\\n' \"$PWD\" >\"$d/directory\"\n"
                        "PATH=\"$d:$PATH\"\nexport PATH\nexec \"$@\"\n")
    wrapper.chmod(0o755)
    launcher.chmod(0o755)
    return launcher


def git_counts(root, directories):
    counts = {}
    for directory in root.glob("package.*"):
        package = directories[(directory / "directory").read_text().strip()]
        log = directory / "spawns"
        counts.setdefault(package, Counter()).update(log.read_text().splitlines() if log.exists() else [])
    return counts


def render_report(metadata, packages, counts, limit, counting):
    lines = [f"{key}\t{value}" for key, value in metadata.items()]
    lines.append("kind\tpackage\toutcome\tseconds\tpassed\tfailed\tskipped\tunfinished\tgit_hardened\tgit_other")
    tests = []
    for name, package in sorted(packages.items(), key=lambda item: item[1]["seconds"] or 0, reverse=True):
        outcomes = Counter(test["outcome"] for test in package["tests"].values())
        calls = counts.get(name, Counter())
        seconds = "-" if package["seconds"] is None else f'{package["seconds"]:.3f}'
        values = ["package", name, package["outcome"], seconds]
        values += [str(outcomes[key]) for key in ("pass", "fail", "skip", "unfinished")]
        values += [str(calls[key]) if counting else "-" for key in ("hardened", "other")]
        lines.append("\t".join(values))
        tests.extend((name, test, row) for test, row in package["tests"].items())
    tests.sort(key=lambda item: item[2]["seconds"] or 0, reverse=True)
    lines.append("kind\tpackage\ttest\toutcome\tseconds")
    for name, test, row in tests[:limit or None]:
        seconds = "-" if row["seconds"] is None else f'{row["seconds"]:.3f}'
        lines.append(f'test\t{name}\t{test}\t{row["outcome"]}\t{seconds}')
    # Unfinished tests must remain visible even beyond the slow-test limit.
    for name, test, row in tests[limit:] if limit else []:
        if row["outcome"] == "unfinished":
            lines.append(f"test\t{name}\t{test}\tunfinished\t-")
    return "\n".join(lines) + "\n"


def output(command, cwd):
    return subprocess.check_output(command, cwd=cwd, text=True).strip()


def executable(value, parser):
    path = shutil.which(value)
    if not path:
        parser.error(f"executable not found: {value}")
    return Path(path).absolute()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--one-at-a-time", action="store_true")
    parser.add_argument("--no-git-count", action="store_true")
    parser.add_argument("--git", help="git binary (default: first git on PATH)")
    parser.add_argument("--go", default="go")
    parser.add_argument("--daemon", type=Path, default=Path(__file__).resolve().parent.parent / "daemon")
    parser.add_argument("--out", type=Path, help="retain events.jsonl and report.txt")
    parser.add_argument("--tests", type=int, default=30, help="slow-test rows, 0 for all (default: 30)")
    parser.add_argument("packages", nargs="*", default=["./..."])
    args = parser.parse_args()
    if args.tests < 0 or not args.daemon.is_dir():
        parser.error("--tests must be nonnegative and --daemon must be a directory")
    go, git = executable(args.go, parser), executable(args.git or "git", parser)
    daemon = args.daemon.resolve()
    counting = not args.no_git_count
    metadata = {"commit": output([str(git), "rev-parse", "HEAD"], daemon),
                "daemon_dirty": bool(output([str(git), "status", "--porcelain", "--", "."], daemon)),
                "machine": platform.platform(), "cores": os.cpu_count(),
                "mode": "one-at-a-time" if args.one_at_a_time else "default",
                "go": output([str(go), "version"], daemon), "git": git,
                "git_version": output([str(git), "--version"], daemon),
                "git_count": "on" if counting else "off"}
    # Keep package-load errors for go test's JSON failure report.
    listed = subprocess.check_output(
        [str(go), "list", "-e", "-f", "{{.ImportPath}}\t{{.Dir}}", *args.packages], cwd=daemon, text=True)
    # Load-error records can have an empty directory, including the last field.
    records = [line.split("\t", 1) for line in listed.splitlines()]
    directories = {directory: package for package, directory in records if directory}
    targets = [package for package, _ in records] if args.one_at_a_time else args.packages
    samples, sampling_errors, stop = [], [], threading.Event()

    def sample():
        try:
            samples.append(sample_machine())
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            sampling_errors.append(str(error))

    def monitor():
        while not stop.wait(5):
            sample()

    with tempfile.TemporaryDirectory(prefix="daemon-test-report-") as temporary:
        root = Path(temporary)
        destination = args.out or root
        destination.mkdir(parents=True, exist_ok=True)
        env = os.environ.copy()
        launcher = write_launchers(root, git) if counting else None
        if not counting and args.git:
            # A symlink selects the requested binary without a counting wrapper.
            (root / "git").symlink_to(git)
            env["PATH"] = f'{root}{os.pathsep}{env.get("PATH", "")}'
        sample()
        started = time.monotonic()
        thread = threading.Thread(target=monitor, daemon=True)
        thread.start()
        returncodes = []
        with (destination / "events.jsonl").open("w") as raw:
            try:
                for target in targets if args.one_at_a_time else [targets]:
                    command = [str(go), "test", "-json", "-count=1"]
                    if launcher:
                        command += ["-exec", shlex.quote(str(launcher))]
                    command += [target] if isinstance(target, str) else target
                    with subprocess.Popen(command, cwd=daemon, env=env, stdout=subprocess.PIPE,
                                          text=True) as process:
                        for line in process.stdout:
                            raw.write(line)
                            try:
                                event = json.loads(line)
                                if (isinstance(event, dict) and not event.get("Test")
                                        and event.get("Package") and event.get("Action") in ("pass", "fail", "skip")):
                                    print(f'{event["Package"]}: {event["Action"]}', file=sys.stderr, flush=True)
                            except ValueError:
                                pass
                        returncodes.append(process.wait())
            finally:
                wall = time.monotonic() - started
                stop.set()
                thread.join()
                sample()
        packages, bad_lines = read_events((destination / "events.jsonl").read_text().splitlines())
        counts = git_counts(root, directories) if counting else {}
        failed = (not packages or any(returncodes)
                  or any(package["outcome"] not in ("pass", "skip") for package in packages.values()))
        shared = any(sample["foreign"] for sample in samples)
        loads = [sample["load"] for sample in samples]
        metadata.update(wall_seconds=f"{wall:.3f}", result="fail" if failed else "pass",
                        shared="shared; cannot serve as a baseline" if shared else "unshared",
                        sampling_errors=len(sampling_errors), samples=len(samples), non_json_lines=bad_lines)
        if sampling_errors:
            metadata["baseline"] = "unavailable; process sampling failed"
            print("Process sampling failed: " + sampling_errors[0], file=sys.stderr)
        if loads:
            for key, load in (("load_start", loads[0]), ("load_end", loads[-1]),
                              ("load_peak", tuple(max(values) for values in zip(*loads)))):
                metadata[key] = ",".join(f"{value:.2f}" for value in load)
        metadata["git_hardened"] = sum(calls["hardened"] for calls in counts.values()) if counting else "-"
        metadata["git_other"] = sum(calls["other"] for calls in counts.values()) if counting else "-"
        report = render_report(metadata, packages, counts, args.tests, counting)
        (destination / "report.txt").write_text(report)
        print(report, end="")
    return int(failed)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"daemon-test-report: {error}", file=sys.stderr)
        sys.exit(1)
