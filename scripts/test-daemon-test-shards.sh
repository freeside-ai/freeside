#!/usr/bin/env bash
# Hermetic tests of compiled discovery, coverage accounting, and CI gates.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
python3 - "$ROOT" <<'PY'
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

# Fixture evidence belongs to synthetic runs, independent of the host CI run.
for variable in ('GITHUB_SHA', 'GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT'):
    os.environ.pop(variable, None)

root = Path(sys.argv[1])
sys.dont_write_bytecode = True
path = root / 'scripts/daemon-test-shards.py'
spec = importlib.util.spec_from_file_location('shards', path)
shards = importlib.util.module_from_spec(spec)
spec.loader.exec_module(shards)
integration = 'module/internal/integration'
inventory = {integration: ['ExampleThing', 'FuzzThing', 'TestA', 'TestNew'], 'module/rest': ['TestRest']}


def events(selected):
    result = []
    for package, names in selected.items():
        for name in names:
            result += [{'Package': package, 'Test': name, 'Action': 'run'},
                       {'Package': package, 'Test': name, 'Action': 'pass', 'Elapsed': 1}]
        result.append({'Package': package, 'Action': 'pass', 'Elapsed': 2})
    return result


# A `go` stand-in serving SHARD_FIXTURE's {package: families}: it lists the
# named packages (or all for ./...) and, with SHARD_EXECUTE=yes, runs the
# families a -run pattern selects in each named package.
FAKE_GO = '''#!/usr/bin/env python3
import json, os, sys
import re
import signal
data = json.load(open(os.environ['SHARD_FIXTURE']))
if sys.argv[1] == 'list':
    print('\\n'.join(data)); sys.exit(0)
if '-list' in sys.argv:
    for p, names in data.items():
        if './...' not in sys.argv and p not in sys.argv: continue
        for n in names: print(json.dumps({'Package': p, 'Action': 'output', 'Output': n+'\\n'}))
        print(json.dumps({'Package': p, 'Action': 'pass'}))
    sys.exit(0)
if os.environ.get('SHARD_EXECUTE') == 'signal': os.kill(os.getpid(), signal.SIGTERM)
if os.environ.get('SHARD_EXECUTE') == 'filtered-fail':
    if '-run' in sys.argv: sys.exit(9)
elif os.environ.get('SHARD_EXECUTE') != 'yes': sys.exit(7)
selector = sys.argv[sys.argv.index('-run')+1] if '-run' in sys.argv else '.'
for p, names in data.items():
    if p not in sys.argv: continue
    for n in names:
        if not re.search(selector, n): continue
        for name in ([n, n+'/seed#0'] if n.startswith('Fuzz') else [n]):
            print(json.dumps({'Package': p, 'Test': name, 'Action': 'run'}))
            print(json.dumps({'Package': p, 'Test': name, 'Action': 'pass'}))
    print(json.dumps({'Package': p, 'Action': 'pass'}))
'''


def darwin_fixture():
    # Every declared darwin package, each with a family the selection leaves
    # out, beside packages the selection must never reach.
    data = {integration: ['TestA'], 'module/elsewhere': ['TestElsewhere']}
    for entry in shards.DARWIN_SELECTION:
        data['module/' + entry['package']] = (entry['families'] or ['TestWhole']) + ['TestUnlisted']
    return data

class Coverage(unittest.TestCase):
    def test_partition_exact_union_and_stable_new_removed_names(self):
        names = inventory[integration]
        bins = shards.partition(names, 2, {'TestA': 100, 'RemovedName': 500})
        self.assertEqual(sorted(sum(bins, [])), names)
        self.assertEqual(len(sum(bins, [])), len(set(sum(bins, []))))
        self.assertEqual(bins, shards.partition(list(reversed(names)), 2, {'TestA': 100}))
        self.assertIn('TestNew', sum(bins, []))
        for name in ['ExampleThing', 'FuzzThing']:
            self.assertIn(name, sum(bins, []))

    def test_bad_partitions_and_weights(self):
        for names, count, weights in [([], 2, {}), (['TestA'] * 2, 1, {}),
                                     (['TestA'], 2, {}), (['TestA'], 0, {}),
                                     (['TestA'], 1, {'TestA': 0}),
                                     (['TestA'], 1, {'TestA': True}),
                                     (['TestA'], 1, {'TestA': float('inf')})]:
            with self.subTest(names=names, count=count, weights=weights), self.assertRaises(ValueError):
                shards.partition(names, count, weights)
        for bins in [[['TestA'], ['TestA']], [['TestA'], []], [['TestA'], ['TestX']]]:
            with self.subTest(bins=bins), self.assertRaises(ValueError):
                shards.validate_partition(['TestA', 'TestB'], bins)

    def test_discovery_from_compiled_output(self):
        listing = []
        for package, names in inventory.items():
            listing += [{'Package': package, 'Action': 'output', 'Output': name + '\n'}
                        for name in names + ['BenchmarkNotDefault']]
            listing.append({'Package': package, 'Action': 'pass'})
        with patch.object(shards.subprocess, 'check_output', side_effect=[
            '\n'.join(inventory), '\n'.join(map(json.dumps, listing))
        ]) as child:
            self.assertEqual(shards.discover('go', root), inventory)
            self.assertEqual(child.call_args_list[1].args[0], ['go', 'test', '-json', '-list', '.', './...'])

    def test_integration_discovery_compiles_only_its_package(self):
        selected = {integration: inventory[integration]}
        listing = [{'Package': integration, 'Action': 'output', 'Output': name + '\n'}
                   for name in selected[integration]] + [{'Package': integration, 'Action': 'pass'}]
        with patch.object(shards.subprocess, 'check_output', side_effect=[
            '\n'.join(inventory), '\n'.join(map(json.dumps, listing))
        ]) as child:
            self.assertEqual(shards.discover('go', root, True), selected)
            self.assertEqual(child.call_args_list[1].args[0],
                             ['go', 'test', '-json', '-list', '.', integration])

    def test_discovery_rejects_missing_duplicate_malformed_and_failed(self):
        good = {'Package': integration, 'Action': 'output', 'Output': 'TestA\n'}
        done = {'Package': integration, 'Action': 'pass'}
        for listing in [[good], [good, good, done],
                        [dict(good, Output='Test bad name\n'), done],
                        [dict(good, Package='unknown'), done]]:
            with self.subTest(listing=listing), patch.object(shards.subprocess, 'check_output',
                    side_effect=[integration, '\n'.join(map(json.dumps, listing))]), self.assertRaises(ValueError):
                shards.discover('go', root)
        with patch.object(shards.subprocess, 'check_output', return_value=integration + '\n' + integration), \
                self.assertRaises(ValueError):
            shards.discover('go', root)
        with patch.object(shards.subprocess, 'check_output', side_effect=subprocess.CalledProcessError(7, 'go')), \
                self.assertRaises(subprocess.CalledProcessError):
            shards.discover('go', root)

    def test_execution_accounts_for_examples_fuzz_seeds_and_default_skips(self):
        data = events(inventory)
        data += [{'Package': integration, 'Test': 'FuzzThing/seed#0', 'Action': 'run'},
                 {'Package': integration, 'Test': 'FuzzThing/seed#0', 'Action': 'pass'}]
        for event in data:
            if event.get('Test') == 'TestNew' and event['Action'] == 'pass':
                event['Action'] = 'skip'
        report = shards.account(data, inventory)
        self.assertEqual(len(report['cases']), 6)
        self.assertEqual(next(c for c in report['cases'] if c['name'] == 'TestNew')['outcome'], 'skip')

    def test_execution_cannot_pass_with_missing_duplicate_foreign_or_failed_case(self):
        data = events(inventory)
        bad = [data[2:], data[:-1], data + data[:2],
               data + [{'Package': 'foreign', 'Action': 'pass'}],
               data + [{'Package': integration, 'Test': 'TestForeign', 'Action': 'run'}]]
        failure = [dict(e, Action='fail') if e.get('Test') == 'TestA' and e['Action'] == 'pass' else e for e in data]
        bad.append(failure)
        for attempt in bad:
            with self.subTest(attempt=attempt), self.assertRaises(ValueError):
                shards.account(attempt, inventory)

    def test_gate_requires_every_named_dependency_success(self):
        expected = ['build', 'tests']
        shards.gate({k: {'result': 'success'} for k in expected}, expected)
        for result in ['failure', 'cancelled', 'skipped', None, 'in_progress']:
            with self.subTest(result=result), self.assertRaises(ValueError):
                shards.gate({'build': {'result': 'success'}, 'tests': {'result': result}}, expected)
        for needs in [{}, {'build': {'result': 'success'}},
                      {k: {'result': 'success'} for k in expected + ['unexpected']}]:
            with self.subTest(needs=needs), self.assertRaises(ValueError):
                shards.gate(needs, expected)
        # The other event's child must be present and skipped: a PR gate
        # names the push-only job, a push gate names the PR-only job.
        shards.gate({'tests': {'result': 'success'}, 'main': {'result': 'skipped'}}, ['tests'], ['main'])
        for needs in [{'tests': {'result': 'success'}},
                      {'tests': {'result': 'success'}, 'main': {'result': 'success'}},
                      {'tests': {'result': 'success'}, 'main': {'result': 'failure'}},
                      {'tests': {'result': 'skipped'}, 'main': {'result': 'skipped'}},
                      {'tests': {'result': 'success'}, 'main': 'skipped'}]:
            with self.subTest(needs=needs), self.assertRaises(ValueError):
                shards.gate(needs, ['tests'], ['main'])
        with self.assertRaises(ValueError):
            shards.gate({'tests': {'result': 'success'}}, ['tests'], ['tests'])

    def test_workflow_wires_complete_matrices_and_tested_gate_predicate(self):
        workflow = (root / '.github/workflows/daemon-ci.yml').read_text()
        jobs = dict(re.findall(r'^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:|\Z)', workflow, re.M | re.S))
        rows = r'- \{kind: (\w+), index: (\d+), count: (\d+)\}'
        for platform in ['linux', 'macos']:
            gate = jobs[platform]
            needs = re.search(r'^    needs: \[(.*?)\]', gate, re.M).group(1).split(', ')
            self.assertEqual(set(needs), ({'linux-build', 'linux-tests'} if platform == 'linux'
                                         else {'macos-tests', 'macos-main'}))
            self.assertIn('    if: always()', gate)
            self.assertIn('toJSON(needs)', gate)
            self.assertIn('daemon-test-shards.py gate', gate)
            self.assertIn('runs-on: ubuntu-latest', gate)
            tests = jobs[platform + '-tests']
            self.assertIn('fail-fast: false', tests)
            self.assertIn('fetch-depth: 2', tests)
            self.assertIn('bash scripts/check.sh daemon test', tests)
            # A PR's macOS job runs the declared darwin selection; the full
            # rest shard runs there only after the merge, in macos-main.
            single = 'rest' if platform == 'linux' else 'darwin'
            self.assertEqual([r for r in re.findall(rows, tests) if r[0] in ('rest', 'darwin')],
                             [(single, '1', '1')])
            self.assertIn('DAEMON_TEST_INTEGRATION_SHARDS: 3', tests)
            # Evidence from any attempt is safe to read only after the gate
            # has required every child's current result to succeed, and only
            # because a successful shard always uploads its own attempt.
            self.assertIn('          pattern: daemon-' + platform + '-*-${{ github.run_id }}-*\n', gate)
            self.assertLess(gate.index('daemon-test-shards.py gate'), gate.index('actions/download-artifact'))
            upload = re.search(r'^      - name: Upload test evidence\n(.*?)(?=^      - |\Z)', tests, re.M | re.S).group(1)
            self.assertIn('        if: always()\n', upload)
            self.assertIn('          if-no-files-found: error\n', upload)
            self.assertIn('          name: daemon-' + platform + '-${{ matrix.kind }}-${{ matrix.index }}-'
                          '${{ github.run_id }}-${{ github.run_attempt }}\n', upload)
            self.assertNotIn('go-build-${{ runner.os }}-${{ matrix.index }}', tests)
        # Linux runs the rest shard and every integration shard on both events.
        linux = jobs['linux-tests']
        integration_rows = [(int(i), int(n)) for kind, i, n in re.findall(rows, linux) if kind == 'integration']
        self.assertEqual(integration_rows, [(i, 3) for i in range(1, 4)])
        self.assertIn('--expected linux-build linux-tests', jobs['linux'])
        self.assertIn('daemon-test-shards.py verify evidence/*/execution.json', jobs['linux'])
        self.assertIn("github.event_name == 'push' && matrix.index == 1", linux)
        self.assertNotIn("github.event_name != 'push'", linux)
        # macOS: a PR runs the darwin selection alone, restores the rest
        # namespace, and saves no cache; a push to main runs the full rest
        # shard and the three integration shards in one job, warms the cache
        # with every test binary, and is the only macOS cache writer.
        macos_tests = jobs['macos-tests']
        self.assertEqual(re.findall(rows, macos_tests), [('darwin', '1', '1')])
        self.assertRegex(macos_tests, r'key: daemon-build-v2-.*-rest-.*\$\{\{ github.sha \}\}\n')
        self.assertNotIn('${{ matrix.kind }}-${{ hashFiles', macos_tests)
        self.assertIn("    if: github.event_name != 'push'\n", macos_tests)
        self.assertIn('bash scripts/check.sh daemon build', macos_tests)
        self.assertIn('runs-on: macos-26', macos_tests)
        self.assertNotIn('actions/cache/save', macos_tests)
        main = jobs['macos-main']
        self.assertIn("    if: github.event_name == 'push'\n", main)
        self.assertIn('runs-on: macos-26', main)
        self.assertIn('fetch-depth: 2', main)
        self.assertLess(main.index("go test -count=1 -run '^$' ./..."), main.index('bash scripts/check.sh daemon test'))
        # The cache this job saves is the one the PR darwin shard and the
        # convergence job restore, so it runs their build too.
        self.assertLess(main.index('run: bash scripts/check.sh daemon build\n'),
                        main.index("go test -count=1 -run '^$' ./..."))
        shard_steps = re.findall(r'DAEMON_TEST_SHARD_KIND: (\w+)\n *DAEMON_TEST_SHARD_INDEX: (\d+)\n'
                                 r' *DAEMON_TEST_SHARD_COUNT: (\d+)\n *DAEMON_TEST_INTEGRATION_SHARDS: (\d+)\n'
                                 r' *DAEMON_TEST_EVIDENCE_DIR: \$\{\{ github.workspace \}\}/evidence/([\w-]+)\n'
                                 r' *run: bash scripts/check.sh daemon test\n', main)
        self.assertEqual(shard_steps, [('rest', '1', '1', '3', 'rest')] +
                         [('integration', str(i), '3', '3', f'integration-{i}') for i in range(1, 4)])
        self.assertEqual(main.count('bash scripts/check.sh daemon test'), 4)
        self.assertIn('for shard in rest integration-1 integration-2 integration-3; do\n', main)
        self.assertIn('metadata --evidence-dir "evidence/$shard"', main)
        upload = re.search(r'^      - name: Upload test evidence\n(.*?)(?=^      - )', main, re.M | re.S).group(1)
        self.assertIn('        if: always()\n', upload)
        self.assertIn('          if-no-files-found: error\n', upload)
        self.assertIn('          name: daemon-macos-main-${{ github.run_id }}-${{ github.run_attempt }}\n', upload)
        self.assertIn("        if: github.event_name == 'push'\n        uses: actions/cache/save@v4", main)
        self.assertRegex(main, r'key: daemon-build-v2-.*-rest-.*\$\{\{ github.sha \}\}\n')
        # Each main-push cache writer trims the restored cache to the entries
        # its own run used before saving, so a saved entry holds the tree's
        # outputs rather than a rolling history. Jobs that save nothing trim
        # nothing: there it would only cost time.
        trim = "Trim the build cache to this job's outputs"
        trim_run = (
            '        env:\n'
            '          GOCACHE_DIR: ${{ steps.go-cache.outputs.build }}\n'
            '          JOB_START: ${{ steps.go-cache.outputs.start }}\n'
            '        run: |\n'
            '          # Go bumps an entry\'s mtime when it uses one at least an hour old,\n'
            '          # so everything this job compiled or read is newer than an hour\n'
            '          # before it started; the rest is history from earlier pushes.\n'
            '          age=$(( ($(date +%s) - JOB_START) / 60 + 61 ))\n'
            '          find "$GOCACHE_DIR" -mindepth 2 -maxdepth 2 -mmin +"$age" -exec rm -rf {} +\n'
            '          du -sk "$GOCACHE_DIR" >evidence/cache-trimmed.txt\n')
        push = "github.event_name == 'push'"
        for name, record, upload, if_push in [
                ('linux-build', 'Record static-job cache size', 'Upload static-job evidence', push),
                ('linux-tests', 'Record cache size after tests', 'Upload test evidence', push + ' && matrix.index == 1'),
                ('macos-main', 'Record cache size after tests', 'Upload test evidence', push)]:
            steps = re.findall(r'^      - (.*?)\n(.*?)(?=^      - |\Z)', jobs[name], re.M | re.S)
            titles = [title.removeprefix('name: ') for title, _ in steps]
            bodies = dict(zip(titles, (body for _, body in steps)))
            self.assertEqual(titles.count(trim), 1, name)
            self.assertLess(titles.index(record), titles.index(trim), name)
            self.assertLess(titles.index(trim), titles.index(upload), name)
            self.assertLess(titles.index(upload), titles.index('Save Go build output'), name)
            # The whole step is pinned: its slack, depth, and target are each
            # the difference between trimming history and deleting used entries.
            self.assertEqual(bodies[trim], '        if: ' + if_push + '\n' + trim_run, name)
            self.assertIn('        if: ' + if_push + '\n', bodies['Save Go build output'], name)
            self.assertIn('echo "start=$(date +%s)" >>"$GITHUB_OUTPUT"', bodies['Resolve Go cache paths'], name)
        self.assertNotIn(trim, macos_tests)
        gate = jobs['macos']
        self.assertIn("EXPECTED: ${{ github.event_name == 'push' && 'macos-main' || 'macos-tests' }}", gate)
        self.assertIn("SKIPPED: ${{ github.event_name == 'push' && 'macos-tests' || 'macos-main' }}", gate)
        self.assertIn('gate --needs "$NEEDS" --expected "$EXPECTED" --skipped "$SKIPPED"', gate)
        self.assertIn("SHARDS: ${{ github.event_name == 'push' && 'all' || 'darwin' }}", gate)
        self.assertIn('find evidence -name execution.json -print0 | xargs -0 python3 scripts/daemon-test-shards.py '
                      'verify --shards "$SHARDS"', gate)
        self.assertIn('  workflow_call:', workflow)
        self.assertNotIn('race', workflow)
        caller = (root / '.github/workflows/pull-request.yml').read_text()
        selector = re.search(r'          select_component daemon (.*?)(?=\n          select_component)',
                             caller, re.S).group(1)
        for helper in ('scripts/daemon-test-shards.py', 'scripts/daemon-test-shards.json'):
            self.assertIn(helper, selector)
            self.assertIn('      - "' + helper + '"', workflow)
        # The PR's macOS Swift tests run in the convergence job when app is
        # selected; app-ci.yml itself requests no macOS job on push and no
        # macOS test job at all, so a PR asks for three macOS jobs, not five.
        self.assertIn("    with:\n      app_tests: ${{ needs.select.outputs.app == 'true' }}\n", caller)
        convergence = (root / '.github/workflows/convergence-ci.yml').read_text()
        swift_build_cache = ("          path: app/.build\n"
                             "          key: swift-build-v1-${{ runner.os }}-${{ runner.arch }}-"
                             "${{ steps.swift-key.outputs.image }}-${{ steps.swift-key.outputs.xcode }}-"
                             "${{ steps.swift-key.outputs.toolchain }}-"
                             "${{ hashFiles('app/**') }}\n")
        self.assertIn('    inputs:\n      app_tests:\n', convergence)
        self.assertNotIn('\n  push:', convergence)
        self.assertNotIn('actions/cache/save', convergence)
        self.assertNotIn(trim, convergence)
        self.assertIn("CONVERGENCE_ALL_TESTS: ${{ inputs.app_tests && '1' || '' }}", convergence)
        self.assertIn('-rest-${{ hashFiles', convergence)
        # The Swift build cache (#2291): an exact key over all of app/ with
        # no fallback, and the build skipped only on that hit and never for
        # an app PR, whose --all-tests run must compile its own changes.
        swift_build = re.search(r'^      - name: Restore the Swift build\n(.*?)(?=^      (?:- |#))',
                                convergence, re.M | re.S).group(1)
        self.assertIn('        id: swift-build\n        uses: actions/cache/restore@v4\n', swift_build)
        self.assertIn(swift_build_cache, swift_build)
        self.assertNotIn('restore-keys', swift_build)
        self.assertIn("CONVERGENCE_SKIP_BUILD: ${{ steps.swift-build.outputs.cache-hit == 'true' && "
                      "!inputs.app_tests && '1' || '' }}", convergence)
        self.assertLess(convergence.index('- name: Restore the Swift build\n'),
                        convergence.index('- name: Run the convergence pass'))
        app = (root / '.github/workflows/app-ci.yml').read_text()
        app_jobs = dict(re.findall(r'^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:|\Z)', app, re.M | re.S))
        self.assertEqual({name for name, body in app_jobs.items() if 'runs-on: macos' in body}, {'apps'})
        self.assertIn("    if: github.event_name != 'push'\n", app_jobs['apps'])
        self.assertNotIn('swift test', app_jobs['apps'])
        # The SwiftPM mirror: generate writes it on push, convergence only
        # restores it, and both name the same workspace-relative path and
        # key prefix, or the entries would not share a cache version.
        mirror = ("          path: .swiftpm-mirror\n"
                  "          key: swiftpm-mirror-v1-${{ hashFiles('app/Package.resolved') }}\n"
                  "          restore-keys: swiftpm-mirror-v1-\n")
        generate = app_jobs['generate']
        for job in (convergence, generate):
            restore = re.search(r'^      - name: Restore the SwiftPM mirror\n(.*?)(?=^      - )', job, re.M | re.S).group(1)
            self.assertIn('        uses: actions/cache/restore@v4\n', restore)
            self.assertIn(mirror, restore)
        self.assertIn('ln -s "$PWD/.swiftpm-mirror" ~/Library/Caches/org.swift.swiftpm/repositories\n', convergence)
        self.assertIn('ln -s "$PWD/.swiftpm-mirror" ~/.cache/org.swift.swiftpm/repositories\n', generate)
        # The save reads the restore step's primary key by id; a renamed id
        # leaves the key empty, and cache/save then only warns.
        self.assertIn('        id: swiftpm-mirror\n        uses: actions/cache/restore@v4\n', generate)
        self.assertIn("        if: github.event_name == 'push' && steps.swiftpm-mirror.outputs.cache-hit != 'true'\n"
                      "        uses: actions/cache/save@v4\n        with:\n          path: .swiftpm-mirror\n"
                      "          key: ${{ steps.swiftpm-mirror.outputs.cache-primary-key }}\n", generate)
        self.assertNotIn('actions/cache/save', app_jobs['linux'])

    def test_required_gate_rejects_selected_skips_and_missing_children(self):
        caller = (root / '.github/workflows/pull-request.yml').read_text()
        command = caller[caller.index('          jq -e \'keys == '):].split('<<<"$NEEDS"')[0]
        command = '\n'.join(line[10:] for line in command.splitlines()) + '<<<"$NEEDS" >/dev/null'
        components = ['daemon', 'app', 'api', 'docs', 'scripts', 'convergence', 'vocabulary']
        needs = {c: {'result': 'skipped'} for c in components}
        needs['select'] = {'result': 'success', 'outputs': {c: 'false' for c in components}}
        needs['select']['outputs']['daemon'] = 'true'
        needs['daemon']['result'] = 'success'
        needs['commit-messages'] = {'result': 'success'}
        needs['migration-history'] = {'result': 'success'}
        def result(value):
            return subprocess.run(['bash', '-c', command], env=dict(os.environ, NEEDS=json.dumps(value)),
                                  capture_output=True).returncode
        self.assertEqual(result(needs), 0)
        for bad in ('skipped', 'failure', 'cancelled', 'unknown'):
            self.assertNotEqual(result(dict(needs, daemon={'result': bad})), 0)
            self.assertNotEqual(result(dict(needs, **{'migration-history': {'result': bad}})), 0)
        self.assertNotEqual(result({k: v for k, v in needs.items() if k != 'daemon'}), 0)
        self.assertNotEqual(result({k: v for k, v in needs.items() if k != 'migration-history'}), 0)
        self.assertNotEqual(result(dict(needs, unexpected={'result': 'success'})), 0)

    def test_helper_and_weight_only_changes_select_daemon(self):
        caller = (root / '.github/workflows/pull-request.yml').read_text()
        body = caller.split('        run: |\n', 1)[1].split('\n  daemon:', 1)[0]
        script = '\n'.join(line[10:] for line in body.splitlines())
        # macOS ships Bash 3.2. Supply only the selector's mapfile call in
        # this fixture; hosted execution uses the native Ubuntu builtin.
        script = '''if (( BASH_VERSINFO[0] < 4 )); then
mapfile() {
  [[ $1 == -t && $2 == files ]] || return 1
  files=()
  while IFS= read -r line; do files[${#files[@]}]=$line; done
}
fi
''' + script
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fake = directory / 'gh'
            fake.write_text('#!/bin/sh\nprintf "%s\\n" "$CHANGED_PATH"\n')
            fake.chmod(0o755)
            for path, expected in [('scripts/daemon-test-shards.py', 'true'),
                                   ('scripts/daemon-test-shards.json', 'true'),
                                   ('scripts/ci-run-durations.sh', 'false')]:
                output = directory / 'outputs'
                output.write_text('')
                env = dict(os.environ, PATH=str(directory) + os.pathsep + os.environ['PATH'],
                           CHANGED_PATH=path, CHANGED_FILES='1', GITHUB_OUTPUT=str(output),
                           GITHUB_REPOSITORY='freeside-ai/freeside', PR='1')
                result = subprocess.run(['bash', '-c', script], env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('daemon=' + expected + '\n', output.read_text())

    def test_real_child_failure_and_invalid_selection(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fixture = directory / 'fixture.json'
            fixture.write_text(json.dumps(inventory))
            fake = directory / 'go'
            fake.write_text(FAKE_GO)
            fake.chmod(0o755)
            base = [sys.executable, str(path), 'run', '--go', str(fake), '--daemon', str(directory)]
            env = dict(os.environ, SHARD_FIXTURE=str(fixture))
            def call(*selection):
                return subprocess.run(base + list(selection), env=env, capture_output=True, text=True).returncode
            self.assertEqual(call('--kind', 'rest', '--index', '1', '--count', '1'), 7)
            env['SHARD_EXECUTE'] = 'signal'
            self.assertEqual(call('--kind', 'rest', '--index', '1', '--count', '1'), 143)
            del env['SHARD_EXECUTE']
            self.assertEqual(call('--kind', 'integration', '--index', '1', '--count', '2',
                                  '--integration-count', '3'), 1)
            self.assertEqual(call('--kind', 'rest', '--index', '1', '--count', '1',
                                  '--integration-count', '0'), 1)
            for selection in [('integration', '0', '2'), ('integration', '3', '2'),
                              ('integration', '1', '0'), ('rest', '1', '2'), ('integration', '1', '9')]:
                with self.subTest(selection=selection):
                    self.assertEqual(call('--kind', selection[0], '--index', selection[1], '--count', selection[2]), 1)
            env['SHARD_EXECUTE'] = 'yes'
            reports = []
            for kind, index, count in [('rest', 1, 1), ('integration', 1, 2), ('integration', 2, 2)]:
                output = directory / f'{kind}-{index}'
                env.pop('GOFLAGS', None)
                if kind == 'rest':
                    env['GOFLAGS'] = '-p=6'
                self.assertEqual(call('--kind', kind, '--index', str(index), '--count', str(count),
                                      '--integration-count', '2',
                                      '--evidence-dir', str(output)), 0)
                (output / 'metadata.json').write_text(json.dumps({
                    'tested_commit': 'head', 'production_tree': 'production',
                    'go': 'go version test', 'platform': ['darwin', 'arm64'], 'ImageOS': 'macos26',
                    'GITHUB_RUN_ID': '1', 'GITHUB_RUN_ATTEMPT': '1'}))
                reports.append(output / 'execution.json')
            # The evidence names the width that produced it, empty when unset.
            self.assertEqual([shards.read_json(r)['goflags'] for r in reports], ['-p=6', '', ''])
            baseline = events(inventory) + [
                {'Package': integration, 'Test': 'FuzzThing/seed#0', 'Action': 'run'},
                {'Package': integration, 'Test': 'FuzzThing/seed#0', 'Action': 'pass'}]
            source = directory / 'baseline.jsonl'
            source.write_text('\n'.join(map(json.dumps, baseline)))
            (directory / 'metadata.json').write_text((reports[0].parent / 'metadata.json').read_text())
            (directory / 'packages.txt').write_text('\n'.join(inventory))
            listing = []
            for package, names in inventory.items():
                listing += [{'Package': package, 'Action': 'output', 'Output': n + '\n'} for n in names]
                listing.append({'Package': package, 'Action': 'pass'})
            (directory / 'discovery.jsonl').write_text('\n'.join(map(json.dumps, listing)))
            command = [sys.executable, str(path), 'compare', '--baseline', str(source)]
            self.assertEqual(subprocess.run(command + list(map(str, reports)), capture_output=True).returncode, 0)
            self.assertNotEqual(subprocess.run(command + list(map(str, reports[:-1])), capture_output=True).returncode, 0)
            self.assertNotEqual(subprocess.run(command + list(map(str, reports + reports[:1])), capture_output=True).returncode, 0)
            # Reports must agree with raw output; stale success cannot hide a
            # truncated stream, a malformed event, or an unexecuted family.
            raw = reports[-1].parent / 'tests.jsonl'
            original = raw.read_text()
            for broken in ('', original.rsplit('\n', 2)[0], original + 'not JSON\n',
                           original + '[]\n', original + original):
                raw.write_text(broken)
                self.assertNotEqual(subprocess.run(command + list(map(str, reports)), capture_output=True).returncode, 0)
            raw.write_text(original)
            identity = reports[-1].parent / 'metadata.json'
            saved = identity.read_text()
            identity.write_text(saved.replace('production', 'different'))
            self.assertNotEqual(subprocess.run(command + list(map(str, reports)), capture_output=True).returncode, 0)
            identity.write_text(saved)
            with patch.dict(os.environ, GITHUB_SHA='head', GITHUB_RUN_ID='1', GITHUB_RUN_ATTEMPT='1'):
                shards.verify_reports(reports)
                # A declared shard set verifies its own full coverage and
                # rejects a shard outside it or a hole inside it.
                shards.verify_reports(reports[:1], 'rest')
                shards.verify_reports(reports[1:], 'integration')
                with self.assertRaisesRegex(ValueError, 'missing, duplicate, or unexpected shard'):
                    shards.verify_reports(reports, 'rest')
                with self.assertRaisesRegex(ValueError, 'exactly one rest shard'):
                    shards.verify_reports(reports[1:], 'rest')
                with self.assertRaisesRegex(ValueError, 'rejects a rest shard'):
                    shards.verify_reports(reports, 'integration')
                with self.assertRaisesRegex(ValueError, 'missing, duplicate, or unexpected shard'):
                    shards.verify_reports(reports[1:2], 'integration')
                with self.assertRaisesRegex(ValueError, 'duplicate shard evidence in one attempt'):
                    shards.verify_reports(reports[1:] + reports[1:2], 'integration')
                with self.assertRaises(ValueError):
                    shards.verify_reports(reports, 'other')
                rest_inventory = reports[0].parent / 'inventory.json'
                saved_inventory = rest_inventory.read_text()
                rest_inventory.write_text(json.dumps({integration: inventory[integration]}))
                with self.assertRaisesRegex(ValueError, 'rest shard omitted'):
                    shards.verify_reports(reports[:1], 'rest')
                rest_inventory.write_text(saved_inventory)
                verify = [sys.executable, str(path), 'verify']
                self.assertEqual(subprocess.run(verify + ['--shards', 'rest', str(reports[0])],
                                                capture_output=True).returncode, 0)
                self.assertNotEqual(subprocess.run(verify + ['--shards', 'integration', str(reports[0])],
                                                   capture_output=True).returncode, 0)
                self.assertNotEqual(subprocess.run(verify + ['--shards', 'integration'],
                                                   capture_output=True).returncode, 0)
            with patch.dict(os.environ, GITHUB_SHA='different-head'):
                with self.assertRaisesRegex(ValueError, 'another commit'):
                    shards.verify_reports(reports)
            with patch.dict(os.environ, GITHUB_RUN_ID='2'):
                with self.assertRaisesRegex(ValueError, 'another run'):
                    shards.verify_reports(reports)
            # A failed-jobs rerun leaves the passed shards' evidence in an
            # earlier attempt; each shard counts only its latest attempt.
            def rerun(report, name, **identity):
                shutil.copytree(report.parent, directory / name)
                metadata = directory / name / 'metadata.json'
                metadata.write_text(json.dumps(dict(json.loads(metadata.read_text()), **identity)))
                return directory / name / 'execution.json'
            retried = rerun(reports[-1], 'retried', GITHUB_RUN_ATTEMPT='2')
            twin = rerun(reports[-1], 'twin', GITHUB_RUN_ATTEMPT='2')
            foreign = rerun(reports[-1], 'foreign', GITHUB_RUN_ID='2', GITHUB_RUN_ATTEMPT='2')
            local = rerun(reports[-1], 'local', GITHUB_RUN_ATTEMPT='')
            invalid = [rerun(reports[-1], 'invalid-' + value, GITHUB_RUN_ATTEMPT=value)
                       for value in ('x', '0', '-1', 'missing')]
            metadata = invalid[-1].parent / 'metadata.json'
            metadata.write_text(json.dumps({k: v for k, v in json.loads(metadata.read_text()).items()
                                            if k != 'GITHUB_RUN_ATTEMPT'}))
            raw.write_text('')  # The superseded attempt's broken evidence is never read.
            with patch.dict(os.environ, GITHUB_SHA='head', GITHUB_RUN_ID='1', GITHUB_RUN_ATTEMPT='2'):
                shards.verify_reports(reports + [retried])
                shards.verify_reports([retried] + reports)
                shards.verify_reports(reports[:-1] + [local])
                with self.assertRaisesRegex(ValueError, 'exactly one rest shard'):
                    shards.verify_reports([retried])
                with self.assertRaisesRegex(ValueError, 'missing, duplicate, or unexpected shard'):
                    shards.verify_reports(reports[:-1])
                with self.assertRaisesRegex(ValueError, 'duplicate shard evidence in one attempt'):
                    shards.verify_reports(reports + [retried, twin])
                for report in invalid:
                    with self.subTest(attempt=report.parent.name), \
                            self.assertRaisesRegex(ValueError, 'invalid run attempt'):
                        shards.verify_reports(reports + [report])
                with self.assertRaisesRegex(ValueError, 'identities differ'):
                    shards.verify_reports(reports + [foreign])
                with self.assertRaisesRegex(ValueError, 'another run'):
                    shards.verify_reports([foreign] + reports)
            with patch.dict(os.environ, GITHUB_SHA='head', GITHUB_RUN_ID='1', GITHUB_RUN_ATTEMPT='1'):
                with self.assertRaisesRegex(ValueError, 'later attempt'):
                    shards.verify_reports(reports + [retried])
            raw.write_text(original)
            # Broken latest evidence fails; it never falls back to an older attempt.
            (retried.parent / 'tests.jsonl').write_text('')
            with patch.dict(os.environ, GITHUB_SHA='head', GITHUB_RUN_ID='1', GITHUB_RUN_ATTEMPT='2'):
                with self.assertRaises(ValueError):
                    shards.verify_reports(reports + [retried])
            source.write_text(source.read_text().rsplit('\n', 2)[0])
            self.assertNotEqual(subprocess.run(command + list(map(str, reports)), capture_output=True).returncode, 0)

    def test_darwin_selection_runs_declared_families_and_fails_closed(self):
        selection = {'module/' + e['package']: e['families'] for e in shards.DARWIN_SELECTION}
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fixture = directory / 'fixture.json'
            fake = directory / 'go'
            fake.write_text(FAKE_GO)
            fake.chmod(0o755)
            env = dict(os.environ, SHARD_FIXTURE=str(fixture), SHARD_EXECUTE='yes')
            def call(data, kind, output, count='1'):
                fixture.write_text(json.dumps(data))
                result = subprocess.run([sys.executable, str(path), 'run', '--go', str(fake),
                                         '--daemon', str(directory), '--kind', kind, '--index', '1',
                                         '--count', count, '--integration-count', '1',
                                         '--evidence-dir', str(output)], env=env, capture_output=True, text=True)
                if result.returncode == 0:
                    (output / 'metadata.json').write_text(json.dumps({
                        'tested_commit': 'head', 'production_tree': 'production',
                        'go': 'go version test', 'platform': ['darwin', 'arm64'], 'ImageOS': 'macos26',
                        'GITHUB_RUN_ID': '1', 'GITHUB_RUN_ATTEMPT': '1'}))
                return result
            data = darwin_fixture()
            darwin = directory / 'darwin'
            result = call(data, 'darwin', darwin)
            self.assertEqual(result.returncode, 0, result.stderr)
            report = json.loads((darwin / 'execution.json').read_text())
            # Whole packages share one command; each filtered package runs
            # alone, because one -run pattern applies to every package.
            base = [str(fake), 'test', '-json', '-count=1']
            whole = [p for p, families in selection.items() if families is None]
            self.assertEqual(report['commands'], [base + whole] + [
                base + ['-run', '^(' + '|'.join(sorted(families)) + ')$', p]
                for p, families in selection.items() if families is not None])
            self.assertEqual(set(json.loads((darwin / 'inventory.json').read_text())), set(selection))
            expected = {p: sorted(families or data[p]) for p, families in selection.items()}
            self.assertEqual(report['inventory'], expected)
            self.assertEqual({(c['package'], c['name']) for c in report['cases']},
                             {(p, n) for p, names in expected.items() for n in names})
            # A listed package or family that discovery cannot find fails the
            # shard instead of shrinking it.
            missing_package = {p: n for p, n in data.items() if p != 'module/internal/verify'}
            result = call(missing_package, 'darwin', directory / 'no-package')
            self.assertEqual(result.returncode, 1)
            self.assertIn('missing or ambiguous darwin package: internal/verify', result.stderr)
            missing_family = dict(data, **{'module/internal/ward':
                                           [n for n in data['module/internal/ward'] if n != 'TestPublicUnicast']})
            result = call(missing_family, 'darwin', directory / 'no-family')
            self.assertEqual(result.returncode, 1)
            self.assertIn('darwin families not discovered in module/internal/ward: TestPublicUnicast', result.stderr)
            self.assertEqual(call(data, 'darwin', directory / 'split', count='2').returncode, 1)
            with patch.object(shards, 'DARWIN_SELECTION', shards.DARWIN_SELECTION + shards.DARWIN_SELECTION[:1]), \
                    self.assertRaisesRegex(ValueError, 'duplicate darwin package'):
                shards.darwin_packages(list(data))
            # A filtered package that fails after the whole-package command
            # passed still fails the shard and leaves no execution report.
            env['SHARD_EXECUTE'] = 'filtered-fail'
            failed = directory / 'filtered-fail'
            self.assertEqual(call(data, 'darwin', failed).returncode, 9)
            self.assertFalse((failed / 'execution.json').exists())
            self.assertIn('module/internal/atomicfile', (failed / 'tests.jsonl').read_text())
            env['SHARD_EXECUTE'] = 'yes'
            rest = directory / 'rest'
            self.assertEqual(call(data, 'rest', rest).returncode, 0)
            stray = directory / 'integration'
            self.assertEqual(call(data, 'integration', stray).returncode, 0)
            reports = [p / 'execution.json' for p in (darwin, rest, stray)]
            with patch.dict(os.environ, GITHUB_SHA='head', GITHUB_RUN_ID='1', GITHUB_RUN_ATTEMPT='1'):
                shards.verify_reports(reports[:1], 'darwin')
                with self.assertRaisesRegex(ValueError, 'darwin-only verification rejects a rest shard'):
                    shards.verify_reports(reports[:2], 'darwin')
                with self.assertRaisesRegex(ValueError, 'missing, duplicate, or unexpected shard'):
                    shards.verify_reports([reports[0], reports[2]], 'darwin')
                # A full-coverage gate never accepts the darwin selection.
                with self.assertRaisesRegex(ValueError, 'exactly one rest shard'):
                    shards.verify_reports(reports[:1])
                with self.assertRaisesRegex(ValueError, 'missing, duplicate, or unexpected shard'):
                    shards.verify_reports(reports)
                discovered = darwin / 'inventory.json'
                saved = discovered.read_text()
                discovered.write_text(json.dumps(dict(json.loads(saved), **{'module/internal/ward':
                                      missing_family['module/internal/ward']})))
                with self.assertRaisesRegex(ValueError, 'darwin families not discovered'):
                    shards.verify_reports(reports[:1], 'darwin')
                discovered.write_text(saved)
                execution = reports[0]
                saved = execution.read_text()
                narrowed = json.loads(saved)
                narrowed['inventory']['module/internal/verify'] = []
                execution.write_text(json.dumps(narrowed))
                with self.assertRaisesRegex(ValueError, 'darwin shard omitted a package or family'):
                    shards.verify_reports(reports[:1], 'darwin')
                execution.write_text(saved)
                verify = [sys.executable, str(path), 'verify']
                self.assertEqual(subprocess.run(verify + ['--shards', 'darwin', str(reports[0])],
                                                capture_output=True).returncode, 0)
                self.assertNotEqual(subprocess.run(verify + ['--shards', 'darwin', str(reports[1])],
                                                   capture_output=True).returncode, 0)
                self.assertNotEqual(subprocess.run(verify + ['--shards', 'bogus', str(reports[0])],
                                                   capture_output=True).returncode, 0)

    def test_cohort_timing_includes_trigger_queues_children_and_attempts(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fake = directory / 'gh'
            fake.write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args = ' '.join(sys.argv)
name = 'jobs.json' if '/jobs?' in args else ('run.json' if '/attempts/' in args else 'original.json')
print((Path(os.environ['TIMING_FIXTURE']) / name).read_text())
''')
            fake.chmod(0o755)
            env = dict(os.environ, PATH=str(directory) + os.pathsep + os.environ['PATH'],
                       TIMING_FIXTURE=str(directory))
            def stamp(seconds):
                return '2026-10-19T00:%02d:%02dZ' % divmod(seconds, 60)
            def job(name, start, end, conclusion='success'):
                return {'run_id': 1, 'run_attempt': 1, 'name': name, 'status': 'completed',
                        'started_at': stamp(start), 'completed_at': stamp(end), 'conclusion': conclusion,
                        'steps': [{'name': 'setup', 'status': 'completed', 'conclusion': 'success',
                                   'started_at': stamp(start), 'completed_at': stamp(start + 1)}]}
            sample = {'cohort': 'baseline', 'run': 1, 'attempt': 1}
            run = {'id': 1, 'run_attempt': 1, 'head_sha': 'head', 'created_at': stamp(0),
                   'run_started_at': stamp(2), 'event': 'pull_request', 'status': 'completed',
                   'conclusion': 'success', 'pull_requests': []}
            original = {'id': 1, 'created_at': stamp(0)}
            jobs = [job('select components', 2, 5), job('daemon / linux (build, test, vet, lint)', 10, 100),
                    job('daemon / macos (build, test)', 15, 130), job('required', 135, 140)]
            def report(current=jobs):
                (directory / 'run.json').write_text(json.dumps(run))
                (directory / 'original.json').write_text(json.dumps(original))
                (directory / 'jobs.json').write_text(json.dumps([{'jobs': current}]))
                (directory / 'samples.json').write_text(json.dumps([sample]))
                result = subprocess.run(['bash', str(root / 'scripts/ci-run-durations.sh'),
                                         '--cohort', str(directory / 'samples.json'), '--out', str(directory / 'out')],
                                        env=env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                return json.loads((directory / 'out/report.json').read_text())
            result = report()['samples'][0]
            self.assertTrue(result['complete'])
            self.assertEqual(result['daemon_seconds'], 130)
            self.assertEqual(result['linux_seconds'], 100)
            self.assertEqual(result['required_seconds'], 140)
            self.assertEqual(result['initial_queue_seconds'], 2)
            self.assertEqual(result['internal_queue_seconds']['macos (build, test)'], 10)
            self.assertEqual(result['daemon_runner_seconds'], 205)
            self.assertEqual(result['total_runner_seconds'], 213)
            self.assertFalse(report(jobs[:-1])['samples'][0]['complete'])
            self.assertFalse(report(jobs[1:])['samples'][0]['complete'])
            run.update(status='in_progress', conclusion=None)
            pending = report(jobs + [dict(job('convergence / tests', 10, 150), status='in_progress',
                                         completed_at=None, conclusion=None)])
            self.assertFalse(pending['samples'][0]['complete'])
            self.assertIsNone(pending['samples'][0]['total_runner_seconds'])
            self.assertEqual(pending['medians'], {})
            run.update(status='completed', conclusion='success')
            for bad in ('failure', 'cancelled', 'skipped', 'unknown'):
                broken = [dict(j, conclusion=bad) if 'macos' in j['name'] else j for j in jobs]
                self.assertFalse(report(broken)['samples'][0]['complete'])
            for broken in (jobs[:-2] + jobs[-1:], jobs + jobs[1:2],
                           [dict(j, completed_at=None) if 'macos' in j['name'] else j for j in jobs]):
                self.assertFalse(report(broken)['samples'][0]['complete'])
            sample['excluded_reason'] = 'Actions incident'
            self.assertFalse(report()['samples'][0]['complete'])
            del sample['excluded_reason']
            sample.update(cohort='candidate', integration_shards=3)
            children = [job('select components', 2, 5), job('daemon / linux (build, vet, lint)', 10, 62),
                        job('daemon / linux (build, test, vet, lint)', 65, 70),
                        job('daemon / macos (build, test)', 85, 90), job('required', 95, 100)]
            for platform in ('linux', 'macos'):
                children.append(job(f'daemon / {platform} (test, rest 1/1)', 10, 60))
                for index in range(1, 4):
                    children.append(job(f'daemon / {platform} (test, integration {index}/3)', 10, 60))
            result = report(children)['samples'][0]
            self.assertTrue(result['complete'])
            self.assertEqual(result['daemon_seconds'], 90)
            self.assertFalse(report(children[:-1])['samples'][0]['complete'])
            self.assertFalse(report([children[2], children[3]])['samples'][0]['complete'])
            # A fresh trigger for attempt 2, never the original created_at.
            sample.update(attempt=2, triggered_at=stamp(200), trigger_source='recorded API rerun request')
            run['run_attempt'] = 2
            run['created_at'] = stamp(201)
            for child in children:
                child['run_attempt'] = 2
                for timed in [child] + child['steps']:
                    for key in ('started_at', 'completed_at'):
                        minutes, seconds = map(int, timed[key][14:19].split(':'))
                        timed[key] = stamp(minutes * 60 + seconds + 200)
            result = report(children)['samples'][0]
            self.assertTrue(result['complete'])
            self.assertEqual(result['daemon_seconds'], 90)
            del sample['triggered_at']
            self.assertFalse(report(children)['samples'][0]['complete'])
            sample['triggered_at'] = stamp(230)
            self.assertFalse(report(children)['samples'][0]['complete'])
            original['created_at'] = stamp(100)
            sample['triggered_at'] = stamp(99)
            self.assertIn('trigger precedes run creation', report(children)['samples'][0]['error'])
            del sample['triggered_at']
            self.assertFalse(report(children)['samples'][0]['complete'])


unittest.main(argv=['shard-regressions'], verbosity=2)
PY
