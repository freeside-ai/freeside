#!/usr/bin/env bash
# file-unit-batch.py against a stand-in gh on PATH and a stand-in
# trackercollect named by TRACKERCOLLECT; no network.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
python3 -I -B - "$root" <<'PY'
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

root = Path(sys.argv[1])
spec = importlib.util.spec_from_file_location('batch', root / 'scripts/file-unit-batch.py')
batch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(batch)

# Issues live in issues.json ({number: {title, labels, body}}), numbered from
# 100. A file named fail-create holds a title whose create exits 1; save-mode
# holds `crlf` (CRLF and trailing blank lines) or `mangle` (one word changed).
FAKE_GH = '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
fixtures = Path(os.environ['FAKE_DIR'])
with open(fixtures / 'calls', 'a') as calls:
    calls.write(json.dumps(sys.argv[1:]) + '\\n')
args = sys.argv[1:]
store = fixtures / 'issues.json'
issues = json.loads(store.read_text()) if store.exists() else {}
def option(name):
    return args[args.index(name) + 1]
if args[:2] == ['issue', 'create']:
    fail = fixtures / 'fail-create'
    if fail.exists() and fail.read_text() == option('--title'):
        sys.exit('could not create')
    number = str(100 + len(issues))
    labels = [args[i + 1] for i, arg in enumerate(args) if arg == '--label']
    issues[number] = {'title': option('--title'), 'labels': labels, 'body': option('--body')}
    print('https://github.com/' + option('--repo') + '/issues/' + number)
elif args[:2] == ['issue', 'edit']:
    body = Path(option('--body-file')).read_text()
    mode = fixtures / 'save-mode'
    mode = mode.read_text() if mode.exists() else ''
    if mode == 'crlf':
        body = body.replace('\\n', '\\r\\n') + '\\r\\n\\r\\n  \\n'
    elif mode == 'mangle' and args[2] == '101':
        body = body.replace('Objective', 'Goal')
    issues[args[2]]['body'] = body
elif args[:2] == ['issue', 'view']:
    print(json.dumps({'body': issues[args[2]]['body']}))
else:
    sys.exit('unexpected gh call: ' + ' '.join(args))
store.write_text(json.dumps(issues))
'''

# Writes unit.json from tc.json: its exit code, and its entries with an issue
# of "self" replaced by the queried number. "no_json" skips the file.
FAKE_TC = '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
fixtures = Path(os.environ['FAKE_DIR'])
args = sys.argv[1:]
with open(fixtures / 'tc-calls', 'a') as calls:
    calls.write(json.dumps(args) + '\\n')
config = json.loads((fixtures / 'tc.json').read_text())
number = int(args[args.index('--issue') + 1])
out = Path(args[args.index('--out') + 1])
if config['exit'] != 1 and not config.get('no_json'):
    out.mkdir(parents=True)
    unknown = [dict(entry, issue=number if entry['issue'] == 'self' else entry['issue'])
               for entry in config.get('unknown_relationships', [])]
    (out / 'unit.json').write_text(json.dumps({
        'issue': number, 'unknown_relationships': unknown, 'unknowns': config.get('unknowns', [])}))
sys.exit(config['exit'])
'''

UNITS = [
    {'name': 'store', 'title': 'Add the store record', 'labels': ['kind:contract']},
    {'name': 'engine', 'title': 'Read the record in the engine', 'labels': ['kind:feat', 'deferral']},
    {'name': 'app', 'title': 'Show the record', 'labels': []},
]

DRAFTS = {
    'store': '### Objective\n\nThe record.\n\n### Dependencies\n\nnone\n',
    'engine': '### Objective\n\nRead it.\n\n### Dependencies\n\nstarts-after #{{store}}: the record\n',
    'app': '### Objective\n\nShow it; see #{{engine}}.\n\n### Dependencies\n\nstarts-after #{{store}}, #{{engine}}\n',
}


class FileUnitBatch(unittest.TestCase):
    def setUp(self):
        self.directory = Path(tempfile.mkdtemp())
        self.fixtures = self.directory / 'fixtures'
        self.input = self.directory / 'batch'
        self.fixtures.mkdir()
        self.input.mkdir()
        for name, text in [('gh', FAKE_GH), ('trackercollect', FAKE_TC)]:
            (self.fixtures / name).write_text(text)
            (self.fixtures / name).chmod(0o755)
        self.collect(exit=0)
        self.write_input(UNITS, DRAFTS)
        self.env = dict(os.environ, PATH=str(self.fixtures) + os.pathsep + os.environ['PATH'],
                        FAKE_DIR=str(self.fixtures), TRACKERCOLLECT=str(self.fixtures / 'trackercollect'))

    def tearDown(self):
        subprocess.run(['rm', '-rf', str(self.directory)], check=True)

    def write_input(self, units, drafts):
        (self.input / 'manifest.json').write_text(json.dumps({'units': units}))
        for name, text in drafts.items():
            (self.input / f'{name}.md').write_text(text)

    def collect(self, **config):
        (self.fixtures / 'tc.json').write_text(json.dumps(config))

    def file(self, *options):
        """Run the script; return (result, gh calls)."""
        (self.fixtures / 'calls').write_text('')
        result = subprocess.run([sys.executable, '-I', str(root / 'scripts/file-unit-batch.py'),
                                 str(self.input), *options],
                                env=self.env, capture_output=True, text=True)
        calls = [json.loads(line) for line in (self.fixtures / 'calls').read_text().splitlines()]
        return result, calls

    def issues(self):
        return json.loads((self.fixtures / 'issues.json').read_text())

    def filed(self):
        state = json.loads((self.input / 'filed.json').read_text())
        self.assertEqual(state['repo'], 'freeside-ai/freeside')
        return state['issues']

    def record(self, issues, repo='freeside-ai/freeside'):
        (self.input / 'filed.json').write_text(json.dumps({'repo': repo, 'issues': issues}))

    def test_units_citing_each_other_are_created_then_filled(self):
        result, calls = self.file()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([call[:2] for call in calls],
                         [['issue', 'create']] * 3 + [['issue', 'edit']] * 3 + [['issue', 'view']] * 3)
        self.assertEqual(self.filed(), {'store': 100, 'engine': 101, 'app': 102})
        issues = self.issues()
        self.assertEqual(issues['101']['labels'], ['kind:feat', 'deferral'])
        self.assertEqual(issues['101']['body'], DRAFTS['engine'].replace('{{store}}', '100'))
        self.assertIn('Show it; see #101.', issues['102']['body'])
        self.assertIn('starts-after #100, #101\n', issues['102']['body'])
        self.assertIn('engine\t#101\thttps://github.com/freeside-ai/freeside/issues/101\t'
                      'read-back: ok\ttrackercollect: ok', result.stdout)
        tc_calls = [json.loads(line) for line in (self.fixtures / 'tc-calls').read_text().splitlines()]
        self.assertEqual([call[:5] for call in tc_calls],
                         [['unit', '--repo', 'github.com/freeside-ai/freeside', '--issue', str(n)]
                          for n in (100, 101, 102)])

    def test_create_bodies_are_stubs(self):
        result, calls = self.file('--repo', 'example/other')
        self.assertEqual(result.returncode, 0, result.stderr)
        creates = [call for call in calls if call[:2] == ['issue', 'create']]
        self.assertTrue(all(call[call.index('--body') + 1] == batch.STUB for call in creates))
        self.assertTrue(all(call[call.index('--repo') + 1] == 'example/other' for call in calls))

    def test_rerun_after_a_failed_create_creates_only_the_rest(self):
        (self.fixtures / 'fail-create').write_text('Read the record in the engine')
        result, calls = self.file()
        self.assertEqual(result.returncode, 1)
        self.assertIn('creating engine', result.stderr)
        self.assertIn('created #100 store', result.stdout)
        self.assertEqual(self.filed(), {'store': 100})
        self.assertNotIn(['issue', 'edit'], [call[:2] for call in calls])

        (self.fixtures / 'fail-create').unlink()
        result, calls = self.file()
        self.assertEqual(result.returncode, 0, result.stderr)
        creates = [call[call.index('--title') + 1] for call in calls if call[:2] == ['issue', 'create']]
        self.assertEqual(creates, ['Read the record in the engine', 'Show the record'])
        self.assertEqual(self.filed(), {'store': 100, 'engine': 101, 'app': 102})
        self.assertEqual(len(self.issues()), 3)
        self.assertIn('starts-after #100, #101\n', self.issues()['102']['body'])

    def test_bad_input_makes_no_forge_call(self):
        cases = {
            'unknown placeholder': (UNITS, dict(DRAFTS, app='see #{{engin}}\n'), '{{engin}} names no unit'),
            'literal braces': (UNITS, dict(DRAFTS, app='a {{ word }}\n'), '{{ word }} names no unit'),
            'missing draft': (UNITS + [{'name': 'docs', 'title': 'Docs', 'labels': []}], DRAFTS, 'docs.md'),
            'repeated name': (UNITS + [dict(UNITS[0])], DRAFTS, "'store' repeats"),
            'bad name': ([{'name': 'Store', 'title': 'T', 'labels': []}], {'Store': ''}, 'must match'),
            'missing labels': ([{'name': 'store', 'title': 'T'}], DRAFTS, 'exactly'),
        }
        for case, (units, drafts, message) in cases.items():
            with self.subTest(case):
                for draft in self.input.glob('*.md'):
                    draft.unlink()
                self.write_input(units, drafts)
                result, calls = self.file()
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn(message, result.stderr)
                self.assertEqual(calls, [])
                self.assertFalse((self.input / 'filed.json').exists())

    def test_recorded_name_missing_from_the_manifest_is_an_input_error(self):
        self.record({'store': 100, 'gone': 7})
        result, calls = self.file()
        self.assertEqual(result.returncode, 2)
        self.assertIn("records 'gone'", result.stderr)
        self.assertEqual(calls, [])

    def test_numbers_recorded_for_another_repo_are_an_input_error(self):
        self.record({'store': 100}, repo='me/sandbox')
        result, calls = self.file()
        self.assertEqual(result.returncode, 2)
        self.assertIn("records issues in 'me/sandbox', not 'freeside-ai/freeside'", result.stderr)
        self.assertEqual(calls, [])

    def test_repo_must_be_owner_and_name(self):
        result, calls = self.file('--repo', 'github.com/freeside-ai/freeside')
        self.assertEqual(result.returncode, 2)
        self.assertIn('expected OWNER/NAME', result.stderr)
        self.assertEqual(calls, [])

    def test_unwritable_input_directory_fails_before_any_create(self):
        self.input.chmod(0o555)
        try:
            result, calls = self.file()
        finally:
            self.input.chmod(0o755)
        self.assertEqual(result.returncode, 1)
        self.assertIn('filed.json', result.stderr)
        self.assertEqual(calls, [])

    def test_failed_trackercollect_build_writes_nothing(self):
        (self.fixtures / 'go').write_text('#!/bin/sh\necho no toolchain >&2\nexit 1\n')
        (self.fixtures / 'go').chmod(0o755)
        del self.env['TRACKERCOLLECT']
        result, calls = self.file()
        self.assertEqual(result.returncode, 1)
        self.assertIn('building trackercollect: exit 1: no toolchain', result.stderr)
        self.assertEqual(calls, [])
        self.assertFalse((self.input / 'filed.json').exists())

    def test_unresolved_placeholder_fails_before_any_save(self):
        with self.assertRaisesRegex(batch.RunError, r'no body saved: app\.md: \{\{engine\}\}'):
            batch.resolve_all(DRAFTS, {'store': 100})

    def test_line_endings_and_trailing_blank_lines_pass_read_back(self):
        (self.fixtures / 'save-mode').write_text('crlf')
        result, _ = self.file()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('\r\n', self.issues()['100']['body'])

    def test_changed_word_fails_read_back_naming_the_issue(self):
        (self.fixtures / 'save-mode').write_text('mangle')
        result, _ = self.file()
        self.assertEqual(result.returncode, 1)
        self.assertIn('#101 engine: saved body differs from the draft', result.stderr)
        self.assertIn('+### Goal', result.stderr)
        self.assertIn('read-back: differs', result.stdout)
        self.assertNotIn('#100 store: saved body differs', result.stderr)

    def test_unknown_relationship_on_a_new_issue_fails(self):
        self.collect(exit=3, unknown_relationships=[
            {'issue': 'self', 'line': 'after #100', 'reason': 'untyped line'}])
        result, _ = self.file()
        self.assertEqual(result.returncode, 1)
        self.assertIn("#101 engine: UNKNOWN relationship on #101: untyped line: 'after #100'",
                      result.stderr)
        self.assertIn('engine\t#101\thttps://github.com/freeside-ai/freeside/issues/101\t'
                      'read-back: ok\ttrackercollect: failed', result.stdout)

    def test_entries_on_other_issues_are_printed_and_pass(self):
        for code in (3, 2):
            with self.subTest(exit=code):
                for path in (self.fixtures / 'issues.json', self.input / 'filed.json'):
                    path.unlink(missing_ok=True)
                self.collect(exit=code, unknown_relationships=[
                    {'issue': 7, 'line': 'exclusive-with', 'reason': 'no target'}],
                    unknowns=[{'code': 'page-cap', 'subject': 'open PRs', 'detail': 'cap reached'}])
                result, _ = self.file()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('(not failing) UNKNOWN relationship on #7: no target', result.stdout)
                self.assertIn('(not failing) UNKNOWN page-cap open PRs: cap reached', result.stdout)

    def test_trackercollect_hard_failure_fails(self):
        for case, config in {'exit 1': {'exit': 1}, 'no unit.json': {'exit': 0, 'no_json': True}}.items():
            with self.subTest(case):
                for path in (self.fixtures / 'issues.json', self.input / 'filed.json'):
                    path.unlink(missing_ok=True)
                self.collect(**config)
                result, _ = self.file()
                self.assertEqual(result.returncode, 1)
                self.assertIn('trackercollect unit #100', result.stderr)
                self.assertIn('trackercollect unit #102', result.stderr)
                self.assertIn('app\t#102\thttps://github.com/freeside-ai/freeside/issues/102\t'
                              'read-back: ok\ttrackercollect: failed', result.stdout)

    def test_dry_run_touches_nothing(self):
        result, calls = self.file('--dry-run')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, [])
        self.assertFalse((self.input / 'filed.json').exists())
        self.assertFalse((self.fixtures / 'tc-calls').exists())
        self.assertIn('=== engine (not created): Read the record in the engine', result.stdout)
        self.assertIn('labels: kind:feat, deferral', result.stdout)
        self.assertIn('starts-after #{{store}}, #{{engine}}', result.stdout)

    def test_dry_run_shows_recorded_numbers(self):
        self.record({'store': 100})
        result, calls = self.file('--dry-run')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, [])
        self.assertIn('=== store #100: Add the store record', result.stdout)
        self.assertIn('starts-after #100, #{{engine}}', result.stdout)
        self.assertEqual(self.filed(), {'store': 100})


report = unittest.main(argv=['test-file-unit-batch'], exit=False, verbosity=1).result
sys.exit(0 if report.wasSuccessful() else 1)
PY
