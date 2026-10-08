#!/usr/bin/env bash
# Hermetic reporter regressions: synthetic events and executable stand-ins.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
PYTHONDONTWRITEBYTECODE=1 python3 - "$root/scripts/daemon-test-report.py" <<'PY'
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile

script = Path(sys.argv[1])
spec = importlib.util.spec_from_file_location("report", script)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def event(package, action, test=None, elapsed=None, **fields):
    result = dict(Package=package, Action=action, **fields)
    if test is not None:
        result["Test"] = test
    if elapsed is not None:
        result["Elapsed"] = elapsed
    return result


events = [event("a", "start"), event("a", "run", "TestParent"),
          event("a", "pass", "TestParent/Child", 9),
          event("a", "pass", "TestParent", 2), event("a", "skip", "TestSkipped", 0),
          event("a", "fail", "TestFailed", 1), event("a", "fail", elapsed=3),
          event("empty", "skip", elapsed=0),
          dict(Action="build-output", ImportPath="broken", Output="compiler error\n"),
          dict(Action="build-fail", ImportPath="broken"),
          event("broken", "fail", elapsed=0, FailedBuild="broken"),
          event("timeout", "run", "TestStuck"),
          event("timeout", "output", Output="panic: test timed out after 10m0s\n"),
          event("timeout", "fail", elapsed=600), event("partial", "start")]
packages, bad = module.read_events([json.dumps(e) for e in events] + ["not json", "[]"])
assert bad == 2
assert packages["a"]["seconds"] == 3
assert packages["a"]["tests"] == {
    "TestParent": dict(outcome="pass", seconds=2),
    "TestSkipped": dict(outcome="skip", seconds=0),
    "TestFailed": dict(outcome="fail", seconds=1)}
assert packages["broken"]["outcome"] == "build-fail"
assert packages["timeout"]["outcome"] == "timeout"
assert packages["partial"]["outcome"] == "incomplete"
assert packages["timeout"]["tests"]["TestStuck"]["outcome"] == "unfinished"
report = module.render_report({}, packages, {"a": {"hardened": 2, "other": 4}}, 1, True)
assert "package\ta\tfail\t3.000\t1\t1\t1\t0\t2\t4" in report
assert "test\ta\tTestParent\tpass\t2.000" in report
assert "TestFailed" not in report and "Child" not in report
assert "test\ttimeout\tTestStuck\tunfinished\t-" in report
assert "TestFailed" in module.render_report({}, packages, {}, 0, False)

processes = """10 1 python3 report.py
11 10 /usr/local/go/bin/go test ./...
12 11 /tmp/own.test -test.timeout=10m
13 12 /tmp/child.test
20 1 /usr/local/go/bin/go test ./...
21 20 /tmp/foreign.test
22 1 /usr/local/go/bin/go build ./...
23 1 /usr/local/bin/gopls
24 1 sh -c go test ./...
25 1 /usr/local/go/bin/go -C /tmp/module test ./...
26 1 /usr/local/go/bin/go -C=/tmp/module test ./...
27 1 /usr/local/go/bin/go -C /tmp/module build ./...
"""
assert module.foreign_tests(processes, 10) == [20, 21, 25, 26]
assert module.foreign_tests("12 11 /tmp/own.test\n11 10 go test ./...\n", 10) == []

# Pin the classification contract instead of silently miscounting after a change.
runner = (script.parent.parent / "daemon/internal/gitrun/runner.go").read_text()
assert re.search(r'func Baseline\(\) \[\]string\s*\{\s*return \[\]string\s*\{\s*"-c",\s*"core.hooksPath=/dev/null"', runner)


def executable(path, source):
    path.write_text(source)
    path.chmod(0o755)
    return path


with tempfile.TemporaryDirectory(prefix="report suite ") as temporary:
    root = Path(temporary).resolve()
    wrapped = root / "wrapped"
    wrapped.mkdir()
    forwarded = root / "arguments.jsonl"
    git = executable(root / "stand-in git", f"#!{sys.executable}\n"
                     "import json,sys\n"
                     f"with open({str(forwarded)!r}, 'a') as out: out.write(json.dumps(sys.argv[1:])+'\\n')\n"
                     "sys.exit(3)\n")
    launcher = module.write_launchers(wrapped, git)
    wrapper = wrapped / "wrapper"
    # The forwarding target is baked in, so replacement environments still work.
    for arguments in (["-c", "core.hooksPath=/dev/null", "status", "a b"], ["show", "a b"]):
        assert subprocess.run([str(wrapper), *arguments], env={"PATH": "/usr/bin:/bin"}).returncode == 3
    assert (wrapped / "spawns").read_text().splitlines() == ["hardened", "other"]
    assert [json.loads(line) for line in forwarded.read_text().splitlines()] == [
        ["-c", "core.hooksPath=/dev/null", "status", "a b"], ["show", "a b"]]
    # The count file contains neither arguments nor credential-bearing environment.
    assert "a b" not in (wrapped / "spawns").read_text()
    binary = executable(root / "binary", "#!/bin/sh\nexec git show 'a b'\n")
    assert subprocess.run([str(launcher), str(binary)], cwd=root).returncode == 3
    assert module.git_counts(wrapped, {str(root): "package"})["package"] == {"other": 1}
    package_dir = next(wrapped.glob("package.*"))
    assert (package_dir / "directory").read_text().strip() == str(root)

    daemon = root / "daemon"
    daemon.mkdir()
    for name in ("a", "b"):
        (daemon / name).mkdir()
    bin_dir = root / "bin"
    bin_dir.mkdir()
    calls = root / "go-calls.jsonl"
    test_binary = executable(bin_dir / "test-binary", "#!/bin/sh\n"
                             "git -c core.hooksPath=/dev/null status >/dev/null\n"
                             "git show >/dev/null\n")
    fake_git = executable(bin_dir / "git", "#!/bin/sh\ncase $1 in\n"
                          "rev-parse) echo baseline-commit ;;\n"
                          "status) : ;;\n--version) echo 'git stand-in' ;;\nesac\n")
    executable(bin_dir / "ps", "#!/bin/sh\n"
               "if [ \"${REPORT_SHARED-}\" = yes ]; then printf '999999 1 go test ./...\\n'; fi\n")
    fake_go = executable(bin_dir / "go", f"#!{sys.executable}\n" + r'''
import json, os, shlex, subprocess, sys
from pathlib import Path
arguments = sys.argv[1:]
with open(os.environ['REPORT_CALLS'], 'a') as out:
    out.write(json.dumps(arguments)+'\n')
if arguments[0] == 'version':
    print('go stand-in')
elif arguments[0] == 'list':
    # A build error must reach go test rather than aborting the report preflight.
    if os.environ.get('REPORT_SCENARIO') == 'build' and '-e' not in arguments:
        sys.exit(1)
    for name in ('a', 'b'):
        if os.environ.get('REPORT_SCENARIO') == 'missing':
            print(f'./missing-{name}\t')
        else:
            print(f'example/{name}\t{Path.cwd() / name}')
elif arguments[0] == 'test':
    scenario = os.environ.get('REPORT_SCENARIO', 'pass')
    targets = [arguments[-1]] if arguments[-1].startswith(('example/', './missing-')) else ['example/a', 'example/b']
    for package in targets:
        def emit(action, **extra):
            print(json.dumps(dict(Package=package, Action=action, **extra)), flush=True)
        emit('start')
        if scenario in ('build', 'missing'):
            print(json.dumps(dict(Action='build-output', ImportPath=package, Output='bad build\n')))
            emit('fail', FailedBuild=package, Elapsed=0)
            continue
        emit('run', Test='TestTop')
        command = [os.environ['REPORT_BINARY']]
        if '-exec' in arguments:
            command = shlex.split(arguments[arguments.index('-exec')+1]) + command
        assert subprocess.call(command, cwd=Path.cwd()/package.split('/')[-1]) == 0
        if scenario == 'timeout':
            emit('output', Output='panic: test timed out after 10m0s\n')
            emit('fail', Elapsed=600)
        elif scenario != 'partial':
            emit('fail' if scenario == 'fail' else 'pass', Test='TestTop', Elapsed=1)
            emit('fail' if scenario == 'fail' else 'pass', Elapsed=2)
    sys.exit(0 if scenario in ('pass', 'partial') else 1)
else:
    sys.exit(9)
''')
    env = dict(os.environ, PATH=f"{bin_dir}:{os.environ['PATH']}", REPORT_CALLS=str(calls),
               REPORT_BINARY=str(test_binary), REPORT_SCENARIO="pass", REPORT_SHARED="no")

    def run(scenario="pass", *options, shared=False):
        calls.write_text("")
        destination = root / "output"
        result = subprocess.run([sys.executable, str(script), "--go", str(fake_go),
                                 "--git", str(fake_git), "--daemon", str(daemon),
                                 "--out", str(destination), *options],
                                env=dict(env, REPORT_SCENARIO=scenario, REPORT_SHARED="yes" if shared else "no"),
                                text=True, capture_output=True)
        if result.returncode not in (0, 1):
            raise AssertionError(result.stderr)
        assert result.stdout == (destination / "report.txt").read_text(), result.stderr
        assert (destination / "events.jsonl").read_text()
        return result, [json.loads(line) for line in calls.read_text().splitlines()]

    result, invocations = run()
    assert result.returncode == 0, result.stderr
    assert "shared\tunshared\n" in result.stdout and "sampling_errors\t0\n" in result.stdout
    assert "package\texample/a\tpass\t2.000\t1\t0\t0\t0\t1\t1" in result.stdout
    test_calls = [call for call in invocations if call[0] == 'test']
    assert len(test_calls) == 1 and test_calls[0][:4] == ['test', '-json', '-count=1', '-exec']
    assert '-e' in next(call for call in invocations if call[0] == 'list')
    assert test_calls[0][-1] == './...'
    _, invocations = run("pass", "--one-at-a-time", "./internal/gitrun")
    test_calls = [call for call in invocations if call[0] == 'test']
    assert [call[-1] for call in test_calls] == ['example/a', 'example/b']
    assert invocations[1][-1] == './internal/gitrun'  # go list retains package patterns.
    result, invocations = run("missing", "--one-at-a-time", "./missing-a", "./missing-b")
    assert result.returncode == 1
    test_calls = [call for call in invocations if call[0] == 'test']
    assert [call[-1] for call in test_calls] == ['./missing-a', './missing-b']
    for package in ('./missing-a', './missing-b'):
        assert f"package\t{package}\tbuild-fail\t" in result.stdout
    result, invocations = run("pass", "--no-git-count")
    assert result.returncode == 0
    assert '-exec' not in next(call for call in invocations if call[0] == 'test')
    assert "git_hardened\t-\n" in result.stdout
    result, _ = run(shared=True)
    assert "shared\tshared; cannot serve as a baseline\n" in result.stdout
    for scenario, outcome in (("fail", "fail"), ("build", "build-fail"),
                              ("timeout", "timeout"), ("partial", "incomplete")):
        result, _ = run(scenario)
        assert result.returncode == 1, (scenario, result.stderr)
        assert f"package\texample/a\t{outcome}\t" in result.stdout
        if scenario in ('timeout', 'partial'):
            assert "TestTop\tunfinished\t-" in result.stdout
    for options in (["--tests", "-1"], ["--unknown"], ["--go", str(root / "missing")]):
        result = subprocess.run([sys.executable, str(script), *options], text=True, capture_output=True)
        assert result.returncode == 2

print("daemon-test-report: all hermetic regressions passed")
PY
