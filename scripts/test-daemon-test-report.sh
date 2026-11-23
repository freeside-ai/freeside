#!/usr/bin/env bash
# Hermetic reporter regressions: synthetic events and executable stand-ins.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
PYTHONDONTWRITEBYTECODE=1 python3 - "$root/scripts/daemon-test-report.py" <<'PY'
import importlib.util
import json
import os
from pathlib import Path
import fcntl
import platform
import re
import shlex
import subprocess
import sys
import tempfile
import time

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

# The slot limit and GOMAXPROCS derivation match scripts/check.sh's cases.
assert module.slot_limit({}) == 1 and module.slot_limit({"CI": "true"}) == 0
assert module.slot_limit({"CI": "true", "DAEMON_TEST_MAX_RUNS": "2"}) == 2
assert module.slot_limit({"DAEMON_TEST_MAX_RUNS": "abc"}) is None
assert module.slot_limit({"DAEMON_TEST_MAX_RUNS": ""}) is None
for cores, limit, expected in ((10, 0, None), (10, 1, None), (10, 2, "5"), (10, 3, "4"),
                               (10, 20, "2"), (10, 9223372036854775808, "2"), (None, 2, None)):
    assert module.derived_gomaxprocs(cores, limit, {}) == expected, (cores, limit)
assert module.derived_gomaxprocs(10, 3, {"GOMAXPROCS": "7"}) == "7"
assert module.state_directory({"HOME": "/h"}) == Path("/h/.cache/freeside/daemon-test")
assert module.state_directory({"HOME": "/h", "XDG_CACHE_HOME": "/x"}) == Path("/x/freeside/daemon-test")
assert module.state_directory({"HOME": "/h", "DAEMON_TEST_STATE_DIR": "/s"}) == Path("/s")

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
    test_env = root / "go-test-env.json"
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
elif arguments[0] == 'env':
    print('\n'.join(os.environ['REPORT_GO_ENV'] + '/' + name for name in ('cache', 'modcache', 'path', 'env')))
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
    tmpdir, home = os.environ.get('TMPDIR'), os.environ.get('HOME')
    with open(os.environ['REPORT_TEST_ENV'], 'w') as out:
        json.dump(dict(TMPDIR=tmpdir, GOMAXPROCS=os.environ.get('GOMAXPROCS'),
                       tmpdir_empty=bool(tmpdir) and os.path.isdir(tmpdir) and not os.listdir(tmpdir),
                       HOME=home, home_empty=bool(home) and os.path.isdir(home) and not os.listdir(home),
                       go=[os.environ.get(name) for name in ('GOCACHE', 'GOMODCACHE', 'GOPATH', 'GOENV')]), out)
    for scratch in (tmpdir, home):
        if scratch and scratch.endswith('.noindex') and os.environ.get('LEAVE_READ_ONLY'):
            # A test its timeout killed leaves read-only directories behind.
            os.makedirs(os.path.join(scratch, 'left/ro'))
            open(os.path.join(scratch, 'left/ro/f'), 'w').close()
            os.chmod(os.path.join(scratch, 'left/ro'), 0o500)
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
    # The stand-in sandbox-exec records each call's arguments, one call per
    # line, then runs the command after its -f and -D options; SANDBOX_FAIL
    # fails it as a nested sandbox (nested) or another way (other).
    sandbox_calls = root / "sandbox-calls"
    executable(bin_dir / "sandbox-exec", "#!/bin/bash\n"
               "case ${SANDBOX_FAIL:-} in\n"
               "  nested) echo 'sandbox-exec: sandbox_apply: Operation not permitted' >&2; exit 71 ;;\n"
               "  other) echo 'sandbox-exec: profile parse error' >&2; exit 65 ;;\n"
               "esac\n"
               f"printf '%s\\n' \"$*\" >>{shlex.quote(str(sandbox_calls))}\n"
               "while [[ $1 == -f || $1 == -D ]]; do shift 2; done\n"
               "exec \"$@\"\n")
    go_env = root / "go-env"
    caller_home = root / "caller home"
    caller_home.mkdir()
    # Every case takes its slot from temporary state, never the user's; an
    # explicit limit keeps CI's limit-off default from skipping the slot.
    state = root / "state"
    env = dict(os.environ, PATH=f"{bin_dir}:{os.environ['PATH']}", REPORT_CALLS=str(calls),
               REPORT_TEST_ENV=str(test_env), REPORT_BINARY=str(test_binary),
               REPORT_SCENARIO="pass", REPORT_SHARED="no", REPORT_GO_ENV=str(go_env),
               HOME=str(caller_home), DAEMON_TEST_STATE_DIR=str(state), DAEMON_TEST_MAX_RUNS="1")
    for name in list(env):
        if name == "GOMAXPROCS" or name == "DAEMON_TEST_SANDBOX" or (
                name.startswith("FREESIDE_") and "_LIVE" in name[len("FREESIDE"):]):
            del env[name]
    go_values = [str(go_env / name) for name in ("cache", "modcache", "path", "env")]

    def run(scenario="pass", *options, shared=False, extra=None):
        calls.write_text("")
        destination = root / "output"
        result = subprocess.run([sys.executable, str(script), "--go", str(fake_go),
                                 "--git", str(fake_git), "--daemon", str(daemon),
                                 "--out", str(destination), *options],
                                env=dict(env, REPORT_SCENARIO=scenario, REPORT_SHARED="yes" if shared else "no",
                                         **(extra or {})),
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
    assert len(test_calls) == 1
    assert test_calls[0][:6] == ['test', '-json', '-count=1', '-timeout', '30m', '-exec']
    slot_tmp = str(state / "slots/1.tmp.noindex")
    slot_home = str(state / "slots/1.home.noindex")
    assert json.loads(test_env.read_text()) == dict(TMPDIR=slot_tmp, GOMAXPROCS=None, tmpdir_empty=True,
                                                    HOME=slot_home, home_empty=True, go=go_values)
    assert not Path(slot_tmp).exists() and not Path(slot_home).exists()
    # On macOS the test child starts under the profile, after one probe.
    if platform.system() == "Darwin":
        resolved = os.path.realpath(state / "slots")
        prefix = (f"-f {script.parent / 'daemon-test.sb'} -D REAL_HOME={os.path.realpath(caller_home)} "
                  f"-D GOCACHE={os.path.realpath(go_env / 'cache')} -D GOMODCACHE={os.path.realpath(go_env / 'modcache')} "
                  f"-D SLOT_HOME={resolved}/1.home.noindex -D SLOT_TMP={resolved}/1.tmp.noindex "
                  f"-D SLOT_DECOY={resolved}/1.decoy")
        lines = sandbox_calls.read_text().splitlines()
        assert len(lines) == 2 and lines[0] == prefix + " /usr/bin/true", lines
        assert lines[1].startswith(f"{prefix} {fake_go} test -json"), lines
    else:
        assert not sandbox_calls.exists()
    assert "slot_limit\t1\n" in result.stdout and "gomaxprocs\tdefault\n" in result.stdout
    assert f"tmpdir\t{slot_tmp}\n" in result.stdout
    stale = Path(slot_tmp) / "stale/ro"
    stale.mkdir(parents=True)
    (stale / "f").write_text("")
    stale.chmod(0o500)
    stale = Path(slot_home) / "stale/ro"
    stale.mkdir(parents=True)
    (stale / "f").write_text("")
    stale.chmod(0o500)
    result, _ = run(extra=dict(LEAVE_READ_ONLY="1"))
    assert result.returncode == 0, result.stderr
    seen = json.loads(test_env.read_text())
    assert seen["tmpdir_empty"] and seen["home_empty"]
    assert not Path(slot_tmp).exists() and not Path(slot_home).exists()
    # A live-suite variable keeps the caller's HOME and skips the sandbox.
    if sandbox_calls.exists():
        sandbox_calls.unlink()
    result, _ = run(extra=dict(FREESIDE_WARD_LIVE_TEST="1"))
    assert result.returncode == 0 and "FREESIDE_WARD_LIVE_TEST is set" in result.stderr, result.stderr
    seen = json.loads(test_env.read_text())
    assert seen["HOME"] == str(caller_home) and seen["TMPDIR"] == slot_tmp and seen["go"] == [None] * 4
    assert not sandbox_calls.exists()
    for value in ("2", ""):
        refused = subprocess.run([sys.executable, str(script), "--go", str(fake_go), "--git", str(fake_git),
                                  "--daemon", str(daemon)], env=dict(env, DAEMON_TEST_SANDBOX=value),
                                 text=True, capture_output=True)
        assert refused.returncode == 2 and "DAEMON_TEST_SANDBOX must be 0 or 1" in refused.stderr
    try:
        module.remove_tree(root / "outside.home.noindex")
        raise AssertionError("remove_tree accepted a path outside a slots directory")
    except ValueError:
        pass
    outside = root / "outside"
    (outside / "ro").mkdir(parents=True)
    outside.chmod(0o500)
    os.symlink(outside, slot_tmp)
    module.remove_tree(Path(slot_tmp))
    assert not os.path.lexists(slot_tmp) and (outside / "ro").is_dir()
    assert outside.stat().st_mode & 0o777 == 0o500
    outside.chmod(0o700)
    result, _ = run(extra=dict(DAEMON_TEST_MAX_RUNS="2"))
    derived = module.derived_gomaxprocs(os.cpu_count(), 2, {})
    assert json.loads(test_env.read_text())["GOMAXPROCS"] == derived
    assert f"gomaxprocs\t{derived or 'default'}\n" in result.stdout
    result, _ = run(extra=dict(DAEMON_TEST_MAX_RUNS="3", GOMAXPROCS="7"))
    assert json.loads(test_env.read_text())["GOMAXPROCS"] == "7"
    assert "slot_limit\t3\n" in result.stdout and "gomaxprocs\t7\n" in result.stdout
    off = root / "state-off"
    result, _ = run(extra=dict(DAEMON_TEST_MAX_RUNS="0", DAEMON_TEST_STATE_DIR=str(off), TMPDIR=str(root)))
    assert result.returncode == 0 and not off.exists()
    assert json.loads(test_env.read_text())["TMPDIR"] == str(root)
    assert json.loads(test_env.read_text())["HOME"] == str(caller_home)

    # The sandbox prefix on any platform, through the stand-in.
    probe_env = dict(env)
    paths = dict(REAL_HOME=caller_home, GOCACHE=go_env, GOMODCACHE=go_env,
                 SLOT_HOME=root, SLOT_TMP=root, SLOT_DECOY=root / "absent")
    profile = script.parent / "daemon-test.sb"
    assert module.sandbox_prefix("Linux", probe_env, profile, paths) == []
    assert module.sandbox_prefix("Darwin", dict(probe_env, DAEMON_TEST_SANDBOX="0"), profile, paths) == []
    prefix = module.sandbox_prefix("Darwin", probe_env, profile, paths)
    assert prefix[:3] == ["sandbox-exec", "-f", str(profile)] and len(prefix) == 15, prefix
    assert f"SLOT_DECOY={os.path.realpath(root)}/absent" in prefix
    assert module.sandbox_prefix("Darwin", dict(probe_env, SANDBOX_FAIL="nested"), profile, paths) == []
    try:
        module.sandbox_prefix("Darwin", dict(probe_env, SANDBOX_FAIL="other"), profile, paths)
        raise AssertionError("a failed probe did not raise")
    except OSError as error:
        assert "profile parse error" in str(error)
    assert "slot_limit\t0\n" in result.stdout and f"tmpdir\t{root}\n" in result.stdout

    # A held slot makes the report wait, without starting go, until it is released.
    (state / "slots").mkdir(parents=True, exist_ok=True)
    calls.write_text("")
    errors = root / "waiting.err"
    with open(state / "slots/1", "a") as held:
        fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
        held.truncate(0)
        held.write("run PID 1: suite\n")
        held.flush()
        with open(errors, "w") as stderr:
            waiting = subprocess.Popen([sys.executable, str(script), "--go", str(fake_go),
                                        "--git", str(fake_git), "--daemon", str(daemon)],
                                       env=env, stdout=subprocess.DEVNULL, stderr=stderr)
        deadline = time.monotonic() + 30
        while "slot 1: " not in errors.read_text():
            assert time.monotonic() < deadline and waiting.poll() is None, errors.read_text()
            time.sleep(0.1)
        notice = errors.read_text()
        assert "limit 1" in notice and str(state) in notice and "slot 1: run PID 1: suite" in notice
        assert "DAEMON_TEST_MAX_RUNS=0" in notice
        time.sleep(0.5)
        assert calls.read_text() == "", calls.read_text()
    assert waiting.wait(timeout=30) == 0, errors.read_text()
    assert any(json.loads(line)[0] == 'test' for line in calls.read_text().splitlines())
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
        result = subprocess.run([sys.executable, str(script), *options], env=env, text=True, capture_output=True)
        assert result.returncode == 2
    result = subprocess.run([sys.executable, str(script), "--go", str(fake_go), "--git", str(fake_git),
                             "--daemon", str(daemon)], env=dict(env, DAEMON_TEST_MAX_RUNS="abc"),
                            text=True, capture_output=True)
    assert result.returncode == 2 and "must be a non-negative integer" in result.stderr

print("daemon-test-report: all hermetic regressions passed")
PY
