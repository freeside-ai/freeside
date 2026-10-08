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
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

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

    def test_workflow_wires_complete_matrices_and_tested_gate_predicate(self):
        workflow = (root / '.github/workflows/daemon-ci.yml').read_text()
        jobs = dict(re.findall(r'^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:|\Z)', workflow, re.M | re.S))
        for platform in ['linux', 'macos']:
            gate = jobs[platform]
            needs = re.search(r'^    needs: \[(.*?)\]', gate, re.M).group(1).split(', ')
            self.assertEqual(set(needs), {platform + '-build', platform + '-tests'})
            self.assertIn('    if: always()', gate)
            self.assertIn('toJSON(needs)', gate)
            self.assertIn('daemon-test-shards.py gate', gate)
            self.assertIn('--expected ' + ' '.join(needs), gate)
            self.assertIn('runs-on: ubuntu-latest', gate)
            tests = jobs[platform + '-tests']
            self.assertIn('fail-fast: false', tests)
            self.assertIn('fetch-depth: 2', tests)
            self.assertIn('bash scripts/check.sh daemon test', tests)
            rows = re.findall(r'- \{kind: (\w+), index: (\d+), count: (\d+)\}', tests)
            self.assertEqual([r for r in rows if r[0] == 'rest'], [('rest', '1', '1')])
            integration_rows = [(int(i), int(n)) for kind, i, n in rows if kind == 'integration']
            count = integration_rows[0][1]
            self.assertEqual(integration_rows, [(i, count) for i in range(1, count + 1)])
            self.assertEqual(len(rows), count + 1)
            self.assertIn('DAEMON_TEST_INTEGRATION_SHARDS: ' + str(count), tests)

    def test_real_child_failure_and_invalid_selection(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fixture = directory / 'fixture.json'
            fixture.write_text(json.dumps(inventory))
            fake = directory / 'go'
            fake.write_text('''#!/usr/bin/env python3
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
if os.environ.get('SHARD_EXECUTE') != 'yes': sys.exit(7)
selector = sys.argv[sys.argv.index('-run')+1] if '-run' in sys.argv else '.'
for p, names in data.items():
    if p not in sys.argv: continue
    for n in names:
        if not re.search(selector, n): continue
        for name in ([n, n+'/seed#0'] if n.startswith('Fuzz') else [n]):
            print(json.dumps({'Package': p, 'Test': name, 'Action': 'run'}))
            print(json.dumps({'Package': p, 'Test': name, 'Action': 'pass'}))
    print(json.dumps({'Package': p, 'Action': 'pass'}))
''')
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
                self.assertEqual(call('--kind', kind, '--index', str(index), '--count', str(count),
                                      '--evidence-dir', str(output)), 0)
                reports.append(output / 'execution.json')
            baseline = events(inventory) + [
                {'Package': integration, 'Test': 'FuzzThing/seed#0', 'Action': 'run'},
                {'Package': integration, 'Test': 'FuzzThing/seed#0', 'Action': 'pass'}]
            source = directory / 'baseline.jsonl'
            source.write_text('\n'.join(map(json.dumps, baseline)))
            command = [sys.executable, str(path), 'compare', '--baseline', str(source)]
            self.assertEqual(subprocess.run(command + list(map(str, reports)), capture_output=True).returncode, 0)
            self.assertNotEqual(subprocess.run(command + list(map(str, reports[:-1])), capture_output=True).returncode, 0)
            self.assertNotEqual(subprocess.run(command + list(map(str, reports + reports[:1])), capture_output=True).returncode, 0)

    def test_whole_gate_timing_includes_children_and_rejects_false_green(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fake = directory / 'gh'
            fake.write_text('''#!/usr/bin/env python3
import json, os, sys
url = sys.argv[-1]
def stamp(seconds): return '2026-10-08T00:%02d:%02dZ' % divmod(seconds, 60)
if sys.argv[1:3] == ['run', 'list']:
    print(json.dumps([{'databaseId': 1, 'headSha': 'head', 'createdAt': stamp(0)}])); sys.exit()
attempt = 1 if '/attempts/1' in url else 2
if '/jobs?' in url:
    jobs = []
    entries = [('linux (build, test, vet, lint)', 10, 100, 'success'),
               ('macos (build, test)', 15, 130, 'success')]
    if attempt == 2 and not os.environ.get('GATE_ONLY'):
        entries += [('linux (test, integration 1/2)', 5, 150, 'failure')]
    if attempt == 1 and os.environ.get('GATE_ONLY'):
        entries += [('linux (test, integration 1/2)', 10, 90, 'success')]
    for name, start, end, result in entries:
        jobs.append({'run_id': 1, 'run_attempt': attempt, 'name': name, 'status': 'completed',
                     'started_at': stamp(start), 'completed_at': stamp(end), 'conclusion': result, 'steps': []})
    print(json.dumps({'jobs': jobs}))
else:
    print(json.dumps({'id': 1, 'run_attempt': attempt, 'head_sha': 'head', 'created_at': stamp(0),
                     'conclusion': 'success' if attempt == 1 else 'failure', 'pull_requests': []}))
''')
            fake.chmod(0o755)
            env = dict(os.environ, PATH=str(directory) + os.pathsep + os.environ['PATH'])
            result = subprocess.run(['bash', str(root / 'scripts/ci-run-durations.sh'), 'daemon CI',
                                     '--attempts', 'all', '--whole-gate'], env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            rows = [r.split('\t') for r in result.stdout.splitlines()]
            spans = [r for r in rows if r[0] == 'whole-gate']
            self.assertEqual([(r[2], r[6], r[7]) for r in spans], [('1', 'true', '120'), ('2', 'false', '125')])
            medians = [r for r in rows if r[0] == 'whole-gate-median']
            self.assertEqual(medians[0][-2:], ['120', '1'])
            env['GATE_ONLY'] = 'yes'
            result = subprocess.run(['bash', str(root / 'scripts/ci-run-durations.sh'), 'daemon CI',
                                     '--attempts', 'all', '--whole-gate'], env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            spans = [r.split('\t') for r in result.stdout.splitlines() if r.startswith('whole-gate\t')]
            self.assertEqual([r[6] for r in spans], ['true', 'false'])


unittest.main(argv=['shard-regressions'], verbosity=2)
PY
