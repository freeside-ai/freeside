#!/usr/bin/env bash
# Hermetic coverage of rebuild argv and the production launch/receipt wiring.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
python3 - "$root" "$BASH" <<'PY'
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile

root = Path(sys.argv[1])
shell = sys.argv[2]
source = (root / 'scripts/run-real-work.sh').read_text()


def extract(start, end):
    assert source.count(start) == source.count(end) == 1, (start, end)
    first = source.index(start)
    last = source.index(end, first)
    return source[first:last]


# Execute the shipped statements, so removing launch wiring or receipt names
# breaks this suite even if the helper's isolated behavior still passes.
setup = extract('source "$repo_root/scripts/real-work-rebuild.sh"\n',
                '# An early, readable refusal')
assert source.index(setup) < source.index('rig_acquisition=')
launch = extract('"$workdir/freesided" \\\n', 'daemon_pid=$!')
preflight = extract('if [[ -n "${FREESIDE_REAL_RUN_BUILD_PROXY:-}" ]]; then\n',
                    'if [[ -n "$retained_session" ]]; then\n  receipt_args=')
receipt = extract('  receipt_args=("$build_version"',
                  '  if [[ "$(cat "$retained_session/status")" == recovery-required &&')
prefix = 'FREESIDE_REAL_RUN_'
base, proxy, dns = [prefix + suffix for suffix in ('BASE_BUILD_REF', 'BUILD_PROXY', 'BUILD_DNS')]


def check(values, expected, error=None):
    with tempfile.TemporaryDirectory() as temp:
        work = Path(temp)
        # A glob match and harmless shell expressions expose accidental expansion.
        (work / 'glob-match').touch()
        stub = work / 'freesided'
        stub.write_text('#!/usr/bin/env python3\nimport json, os, sys\n'
                        'open(os.environ["ARGV_FILE"], "w").write(json.dumps(sys.argv[1:]))\n')
        stub.chmod(0o700)
        env = {key: value for key, value in os.environ.items()
               if not key.startswith('FREESIDE_')}
        env.update({key: 'fixture' for key in re.findall(r'\$(FREESIDE_\w+)', launch)})
        env.update(values)
        env['ARGV_FILE'] = str(work / 'argv.json')
        locals_ = {key: 'fixture' for key in re.findall(r'\$([a-z_]\w*)', launch + receipt)}
        locals_.update(repo_root=str(root), workdir=temp)
        # Arrays are nonempty sentinels, except the rebuild array under test.
        script = 'set -euo pipefail\n' + '\n'.join(
            key + '=' + shlex.quote(value) for key, value in locals_.items()) + '\n'
        script += ('judgment_args=(-fixture-judgment)\nmanual_submission_args=(-fixture-manual)\n'
                   'required=(LEGACY_REQUIRED)\njudgment_names=(LEGACY_JUDGMENT)\n'
                   'preflight_args=(-fixture-preflight)\n'
                   'real_work_installed_daemon() { printf "%s" fixture; }\n')
        script += setup + preflight + receipt
        script += 'printf "%s\\n" "${preflight_args[@]}" > preflight.txt\n'
        script += 'printf "%s\\n" "${receipt_args[@]}" > receipt.txt\n'
        script += launch + 'wait $!\n'
        result = subprocess.run([shell, '-c', script], cwd=work, env=env,
                                capture_output=True, text=True)
        if error:
            assert result.returncode == 2, result.stderr
            assert error in result.stderr, result.stderr
            assert not (work / 'argv.json').exists()
            assert not (work / 'preflight.txt').exists()
            assert not (work / 'receipt.txt').exists()
            return
        assert result.returncode == 0, result.stderr
        argv = json.loads((work / 'argv.json').read_text())
        # The complete expected prefix proves order, exact values and boundaries.
        assert argv[:2 + len(expected)] == ['-fixture-judgment', '-fixture-manual'] + expected, argv
        rest = argv[2 + len(expected):]
        assert not set(rest) & {'-base-build-ref', '-build-proxy', '-build-dns'}, rest
        preflight_argv = (work / 'preflight.txt').read_text().splitlines()
        assert preflight_argv == ['-fixture-preflight'] + (
            ['-build-proxy', values[proxy]] if values.get(proxy) else []), preflight_argv
        receipt_names = (work / 'receipt.txt').read_text().splitlines()
        assert receipt_names.count(proxy) == 1
        for name in (base, dns):
            assert receipt_names.count(name) == int(bool(values.get(name))), receipt_names
        assert not (work / 'evaluated').exists()


check({}, [])
check({base: '', proxy: '', dns: ''}, [])
check({proxy: 'http://proxy:3128'}, [])
check({base: 'approved:local'}, ['-base-build-ref', 'approved:local'])
check({base: 'approved:local', proxy: 'http://proxy:3128'},
      ['-base-build-ref', 'approved:local', '-build-proxy', 'http://proxy:3128'])
check({base: 'approved:local', dns: '1.1.1.1'},
      ['-base-build-ref', 'approved:local', '-build-dns', '1.1.1.1'])
check({base: 'approved:local', proxy: 'http://proxy:3128', dns: ' 1.1.1.1\t2001:db8::1  8.8.8.8 '},
      ['-base-build-ref', 'approved:local', '-build-proxy', 'http://proxy:3128',
       '-build-dns', '1.1.1.1', '-build-dns', '2001:db8::1', '-build-dns', '8.8.8.8'])
literal = 'tag with spaces * $(touch evaluated) `touch evaluated` ; \\ "'
literal_base = 'tag*[$HOME];$(true)`true`\\"'
check({base: literal_base, proxy: literal, dns: '* $HOME \\ [abc] ; $(true)'},
      ['-base-build-ref', literal_base, '-build-proxy', literal,
       '-build-dns', '*', '-build-dns', '$HOME', '-build-dns', '\\',
       '-build-dns', '[abc]', '-build-dns', ';', '-build-dns', '$(true)'])
check({base: 'base-tag-', dns: '1.1.1.1 1.1.1.1'},
      ['-base-build-ref', 'base-tag-', '-build-dns', '1.1.1.1', '-build-dns', '1.1.1.1'])
for invalid in ('-base:tag', 'base tag', 'base\ttag', 'base\rtag', 'base\ntag',
                'base@sha256:digest', 'https://base:tag'):
    check({base: invalid}, [], base)
for invalid in ('-1.1.1.1', '--help', '1.1.1.1 -2001:db8::1'):
    check({base: 'approved:local', dns: invalid}, [], dns)
check({dns: '1.1.1.1'}, [], base)
check({base: '', proxy: 'http://proxy:3128', dns: '2001:db8::1'}, [], base)
check({base: 'approved:local', dns: '1.1.1.1\n8.8.8.8'}, [], 'one line')
check({base: 'approved:local', dns: '1.1.1.1\r8.8.8.8'}, [], 'one line')
print('PASS: rebuild argv, daemon launch, preflight compatibility, early refusal, and receipt names')
PY
