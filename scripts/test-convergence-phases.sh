#!/usr/bin/env bash
# convergence-phases.py over log excerpts from a real app run (job
# 133764437345, compile and test lines dropped), and ci-run-durations.sh's
# --all-branches, --created, and job_id column, with a stand-in gh; no network.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
PYTHONDONTWRITEBYTECODE=1 python3 -I - "$root" <<'PY'
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

root = Path(sys.argv[1])
spec = importlib.util.spec_from_file_location('phases', root / 'scripts/convergence-phases.py')
phases = importlib.util.module_from_spec(spec)
spec.loader.exec_module(phases)

JOB = 'convergence / convergence (real daemon + Swift client)'

TWO_SLICES = '''\
2026-11-17T02:11:15.4896970Z Cache not found for input keys: swift-build-v1-macOS-ARM64
2026-11-17T02:11:15.5021180Z ##[group]Run bash scripts/check.sh convergence
2026-11-17T02:11:15.5022160Z bash scripts/check.sh convergence
2026-11-17T02:11:15.5237400Z ##[endgroup]
2026-11-17T02:11:15.6263200Z == convergence run
2026-11-17T02:11:15.6264470Z + (cd /Users/runner/work/freeside/freeside && bash scripts/run-convergence.sh --all-tests)
2026-11-17T02:11:15.7575420Z building freeside-signet-dev
2026-11-17T02:11:56.2387290Z harness ready: api=http://127.0.0.1:49204 control=http://127.0.0.1:49205
2026-11-17T02:11:56.2390350Z running: swift build --build-tests --only-use-versions-from-resolved-file
2026-11-17T02:12:08.9721050Z Fetching https://github.com/mattpolzin/OpenAPIKit from cache
2026-11-17T02:12:46.6564960Z Building for debugging...
2026-11-17T02:17:51.0827930Z [486 / 488] FreesideCoreTests-product
2026-11-17T02:17:51.7505990Z Build complete! (305.76 sec)
2026-11-17T02:17:56.9632180Z cloned app/.build 2 times in 4s
2026-11-17T02:17:56.9797370Z 02:17:56 starting: swift test --only-use-versions-from-resolved-file --skip-build --skip ScreenshotRegressionTests
2026-11-17T02:17:56.9887270Z 02:17:56 starting: FREESIDE_SCREENSHOT_SHARD=0/2 swift test --only-use-versions-from-resolved-file --skip-build --scratch-path /tmp/a
2026-11-17T02:17:56.9999730Z 02:17:56 starting: FREESIDE_SCREENSHOT_SHARD=1/2 swift test --only-use-versions-from-resolved-file --skip-build --scratch-path /tmp/b
2026-11-17T02:23:08.6964900Z 02:23:08 all runs finished
2026-11-17T02:23:08.6966740Z ==> main.log <==
2026-11-17T02:23:08.7035560Z ◇ Test run started.
2026-11-17T02:23:09.0310410Z ✔ Suite TaskPromptHistoryTests passed after 5.425 seconds.
2026-11-17T02:23:09.0310600Z ✔ Test run with 524 tests in 40 suites passed after 5.427 seconds.
2026-11-17T02:23:09.3365780Z ✔ Test run with 1325 tests in 96 suites passed after 154.046 seconds.
2026-11-17T02:23:09.3389460Z ✔ Test run with 23 tests in 1 suite passed after 1.934 seconds.
2026-11-17T02:23:09.3696720Z ✔ Test run with 297 tests in 21 suites passed after 2.077 seconds.
2026-11-17T02:23:09.3697020Z ==> shard-0.log <==
2026-11-17T02:23:09.3698350Z ✔ Test surfacesMatchRecordedPixels() passed after 97.965 seconds.
2026-11-17T02:23:09.3703450Z ✔ Suite ScreenshotRegressionTests passed after 288.265 seconds.
2026-11-17T02:23:09.3703640Z ✔ Test run with 12 tests in 1 suite passed after 288.266 seconds.
2026-11-17T02:23:09.3703720Z ==> shard-1.log <==
2026-11-17T02:23:09.3704520Z ✔ Test surfacesMatchRecordedPixels() passed after 98.060 seconds.
2026-11-17T02:23:09.3704710Z ✔ Suite ScreenshotRegressionTests passed after 98.060 seconds.
2026-11-17T02:23:09.3705130Z ✔ Test run with 1 test in 1 suite passed after 98.060 seconds.
2026-11-17T02:23:12.6206240Z PASS: convergence run
2026-11-17T02:23:12.6935620Z ##[group]Run echo "cache-hit="
2026-11-17T02:23:14.2009800Z Post job cleanup.
'''

TWO_SLICES_PHASES = {'harness': 40.736611, 'loading': 50.417461, 'build': 305.094103,
                     'clone': 5.212619, 'tests': 311.716753, 'main': 163.484,
                     'slice_0': 288.266, 'slice_1': 98.06}


def one_slice():
    """TWO_SLICES as a one-slice run: no selector, and no shard-1 log."""
    lines = []
    in_shard_1 = False
    for line in TWO_SLICES.splitlines():
        if 'SHARD=1/2' in line:
            continue
        if line.endswith('==> shard-1.log <=='):
            in_shard_1 = True
            continue
        if in_shard_1 and 'passed after' in line:
            continue
        line = line.replace('cloned app/.build 2 times', 'cloned app/.build 1 times')
        lines.append(line.replace('FREESIDE_SCREENSHOT_SHARD=0/2 ', ''))
    return '\n'.join(lines) + '\n'


def as_gh_log(text):
    """The job and step columns `gh run view --log` adds, and its BOM."""
    return '﻿' + ''.join(f'{JOB}\tUNKNOWN STEP\t{line}\n' for line in text.splitlines())


def parse(text):
    return phases.parse_log(phases.log_text(as_gh_log(text)))


def without(text, needle):
    return ''.join(line + '\n' for line in text.splitlines() if needle not in line)


class ParseLog(unittest.TestCase):
    def assertPhases(self, got, want):
        self.assertEqual(sorted(got), sorted(want))
        for key, value in want.items():
            self.assertAlmostEqual(got[key], value, places=5, msg=key)

    def test_two_slices(self):
        self.assertPhases(parse(TWO_SLICES), TWO_SLICES_PHASES)

    def test_one_slice(self):
        want = dict(TWO_SLICES_PHASES)
        del want['slice_1']
        self.assertPhases(parse(one_slice()), want)

    def test_failed_run_raises(self):
        failed = TWO_SLICES.replace(
            '✔ Test run with 1 test in 1 suite passed after 98.060 seconds.',
            '✘ Test run with 1 test in 1 suite failed after 98.060 seconds with 1 issue.').replace(
            'PASS: convergence run',
            'run-convergence: swift test failed in shard-1.log; see the logs above')
        with self.assertRaisesRegex(phases.LogError, 'failed run'):
            parse(failed)
        with self.assertRaisesRegex(phases.LogError, 'failed run: run-convergence'):
            parse(TWO_SLICES.replace('PASS: convergence run', 'run-convergence: screenshot slice 1 did not run to a pass'))

    def test_each_missing_marker_names_its_phase(self):
        for needle, phase in [('##[group]Run bash scripts/check.sh convergence', 'harness'),
                              ('harness ready: api=', 'harness'),
                              ('running: swift build', 'loading'),
                              ('Building for ', 'loading'),
                              ('Build complete!', 'build'),
                              ('cloned app/.build', 'clone'),
                              (' starting: ', 'tests'),
                              ('all runs finished', 'tests')]:
            with self.subTest(needle=needle), self.assertRaisesRegex(phases.LogError, f"phase '{phase}': missing"):
                parse(without(TWO_SLICES, needle))

    def test_harness_build_line_is_optional(self):
        # #3293 may skip the harness build on a cache hit.
        self.assertAlmostEqual(parse(without(TWO_SLICES, 'building freeside-signet-dev'))['harness'], 40.736611, places=5)

    def test_duplicated_marker_raises(self):
        line = next(l for l in TWO_SLICES.splitlines() if 'Build complete!' in l)
        with self.assertRaisesRegex(phases.LogError, "phase 'build': marker 'built' appears 2 times"):
            parse(TWO_SLICES.replace(line, line + '\n' + line))

    def test_reversed_timestamps_raise(self):
        reversed_build = TWO_SLICES.replace('2026-11-17T02:17:51.7505990Z Build complete!',
                                            '2026-11-17T02:12:40.0000000Z Build complete!')
        with self.assertRaisesRegex(phases.LogError, "phase 'build': timestamps go backward"):
            parse(reversed_build)

    def test_process_count_must_match_slices(self):
        with self.assertRaisesRegex(phases.LogError, "phase 'tests': 2 processes started for 2 slices"):
            parse(without(TWO_SLICES, 'SHARD=1/2'))

    def test_process_logs_must_number_zero_to_k_minus_one(self):
        with self.assertRaisesRegex(phases.LogError, 'process logs'):
            parse(TWO_SLICES.replace('==> shard-1.log <==', '==> shard-2.log <=='))
        with self.assertRaisesRegex(phases.LogError, 'process logs'):
            parse(without(TWO_SLICES, '==> shard-1.log <=='))

    def test_process_log_without_passing_summary_raises(self):
        with self.assertRaisesRegex(phases.LogError, 'process shard-1: no passing test run summary'):
            parse(without(TWO_SLICES, 'Test run with 1 test in 1 suite'))


HEADER = 'kind\trun\tattempt\tsha\tjob\tstep\tconclusion\tseconds\tsamples\tjob_id\n'


def tsv_job(run, job_id, conclusion='success', app='success', name=JOB, pass_step=True, setup=2):
    # The skipped step's seconds are nonzero only so the test sees it left out.
    steps = [('Set up job', 'success', setup), ('Restore Go build output', 'success', 12),
             ('Verify pinned swift-format version', app, 0 if app == 'skipped' else 1),
             ('Lint (scripts/check.sh)', 'skipped', 9), ('Restore the Swift build', 'success', 20),
             ('Run the convergence pass (scripts/check.sh)', conclusion, 700),
             ('Complete job', 'success', 1)]
    if not pass_step:
        del steps[5]
    rows = [f'job\t{run}\t1\tsha\t{name}\t\t{conclusion}\t800\t\t{job_id}\n']
    rows += [f'step\t{run}\t1\tsha\t{name}\t{step}\t{result}\t{seconds}\t\t{job_id}\n'
             for step, result, seconds in steps]
    return ''.join(rows)


TSV = HEADER + tsv_job(5, 50) + tsv_job(5, 51, name='apps / apps (macOS)') + \
    tsv_job(4, 40, app='skipped') + tsv_job(3, 30, conclusion='failure') + tsv_job(2, 20) + \
    f'median\t\t\t\t{JOB}\t\tsuccess\t800\t3\t\n'


class SelectJobs(unittest.TestCase):
    def test_app_runs_newest_first(self):
        self.assertEqual(phases.select_jobs(TSV, 2),
                         [{'run': '5', 'job_id': '50', 'total': 800.0, 'setup': 35.0},
                          {'run': '2', 'job_id': '20', 'total': 800.0, 'setup': 35.0}])
        self.assertEqual([j['run'] for j in phases.select_jobs(TSV, 1)], ['5'])

    def test_job_without_the_pass_step_raises(self):
        with self.assertRaisesRegex(ValueError, "run 6 job 60: no step 'Run the convergence pass"):
            phases.select_jobs(HEADER + tsv_job(6, 60, pass_step=False), 1)

    def test_unfinished_setup_step_raises(self):
        with self.assertRaisesRegex(ValueError, "run 6 job 60: phase 'setup' has an unfinished step"):
            phases.select_jobs(HEADER + tsv_job(6, 60, setup=''), 1)

    def test_fewer_than_n_names_the_count(self):
        with self.assertRaisesRegex(ValueError, 'found 2 app convergence jobs in 4 runs, need 3'):
            phases.select_jobs(TSV, 3)


FAKE_GH = '''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
fixtures = Path(os.environ['FAKE_GH_DIR'])
with open(fixtures / 'calls', 'a') as calls:
    calls.write(json.dumps(sys.argv[1:]) + '\\n')
args = sys.argv[1:]
if args[:2] == ['run', 'list']:
    print(json.dumps([{'databaseId': 7, 'headSha': 'abc', 'createdAt': '2026-11-12T00:00:00Z'}]))
elif args[0] == 'api':
    print((fixtures / 'jobs.json').read_text())
elif args[:2] == ['run', 'view']:
    sys.stdout.write((fixtures / ('log-' + args[args.index('--job') + 1])).read_text())
else:
    sys.exit('unexpected gh call: ' + ' '.join(args))
'''


def api_job(job_id, app='success'):
    def stamp(seconds):
        return '2026-11-12T00:%02d:%02dZ' % divmod(seconds, 60)
    names = [('Set up job', 'success', 0, 2), ('Verify pinned swift-format version', app, 2, 3),
             ('Run the convergence pass (scripts/check.sh)', 'success', 3, 700)]
    return {'id': job_id, 'run_id': 7, 'run_attempt': 1, 'name': JOB, 'status': 'completed',
            'conclusion': 'success', 'started_at': stamp(0), 'completed_at': stamp(780),
            'steps': [{'name': n, 'status': 'completed', 'conclusion': c,
                       'started_at': stamp(a), 'completed_at': stamp(b)} for n, c, a, b in names]}


class WithStandInGh(unittest.TestCase):
    def setUp(self):
        self.directory = Path(tempfile.mkdtemp())
        fake = self.directory / 'gh'
        fake.write_text(FAKE_GH)
        fake.chmod(0o755)
        (self.directory / 'jobs.json').write_text(json.dumps({'jobs': [api_job(99), api_job(98, app='skipped')]}))
        (self.directory / 'log-99').write_text(as_gh_log(TWO_SLICES))
        self.env = dict(os.environ, PATH=str(self.directory) + os.pathsep + os.environ['PATH'],
                        FAKE_GH_DIR=str(self.directory))

    def tearDown(self):
        subprocess.run(['rm', '-rf', str(self.directory)], check=True)

    def run_in_root(self, *command):
        (self.directory / 'calls').write_text('')
        result = subprocess.run(list(command), cwd=root, env=self.env, capture_output=True, text=True)
        calls = [json.loads(line) for line in (self.directory / 'calls').read_text().splitlines()]
        return result, calls

    def durations(self, *options):
        return self.run_in_root('bash', 'scripts/ci-run-durations.sh', 'pull request', *options)

    def test_default_call_keeps_branch_main(self):
        result, calls = self.durations()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls[0][:6], ['run', 'list', '--workflow', 'pull request', '--branch', 'main'])

    def test_all_branches_drops_branch_and_created_passes_through(self):
        result, calls = self.durations('--all-branches', '--created', '2026-11-11..2026-11-16', '--limit', '9')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls[0], ['run', 'list', '--workflow', 'pull request', '--created',
                                    '2026-11-11..2026-11-16', '--limit', '9',
                                    '--json', 'databaseId,headSha,createdAt'])
        result, calls = self.durations('--all-branches')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('--branch', calls[0])

    def test_all_branches_with_branch_is_a_usage_error(self):
        for options in [('--all-branches', '--branch', 'main'), ('--branch', 'main', '--all-branches')]:
            result, calls = self.durations(*options)
            self.assertEqual(result.returncode, 2, options)
            self.assertEqual(calls, [])

    def test_job_id_is_the_last_column(self):
        result, _ = self.durations('--all-branches')
        rows = [line.split('\t') for line in result.stdout.splitlines()]
        self.assertEqual(rows[0][-1], 'job_id')
        self.assertTrue(all(len(row) == len(rows[0]) for row in rows))
        self.assertEqual({row[-1] for row in rows if row[0] in ('job', 'step')}, {'99', '98'})
        self.assertEqual({row[-1] for row in rows if row[0] == 'median'}, {''})

    def test_command_reports_phases_of_app_jobs(self):
        result, calls = self.run_in_root(sys.executable, '-I', 'scripts/convergence-phases.py',
                                         '--jobs', '1', '--scan', '5', '--created', '2026-11-11..2026-11-16')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(['run', 'view', '--repo', 'freeside-ai/freeside', '--job', '99', '--log'], calls)
        self.assertIn('--created', calls[0])
        rows = [line.split('\t') for line in result.stdout.splitlines()]
        self.assertEqual(rows[0], ['run', 'job_id', 'total', 'setup', 'harness', 'loading', 'build',
                                   'clone', 'tests', 'main', 'slice_0', 'slice_1'])
        self.assertEqual(rows[1], ['7', '99', '780.0', '3.0', '40.7', '50.4', '305.1', '5.2', '311.7',
                                   '163.5', '288.3', '98.1'])
        summary = {row[0]: row[1:] for row in rows[3:]}
        self.assertEqual(summary['build'], ['305.1', '305.1', '305.1', '1'])

    def test_command_fails_when_too_few_jobs(self):
        result, _ = self.run_in_root(sys.executable, '-I', 'scripts/convergence-phases.py', '--jobs', '2')
        self.assertEqual(result.returncode, 1)
        self.assertIn('found 1 app convergence jobs in 1 runs, need 2', result.stderr)
        self.assertEqual(result.stdout, '')

    def test_command_fails_on_an_unreadable_log(self):
        (self.directory / 'log-99').write_text(as_gh_log(without(TWO_SLICES, 'Build complete!')))
        result, _ = self.run_in_root(sys.executable, '-I', 'scripts/convergence-phases.py', '--jobs', '1')
        self.assertEqual(result.returncode, 1)
        self.assertIn("run 7 job 99: phase 'build': missing marker 'built'", result.stderr)
        self.assertEqual(result.stdout, '')


report = unittest.main(argv=['test-convergence-phases'], exit=False, verbosity=1).result
sys.exit(0 if report.wasSuccessful() else 1)
PY
