#!/usr/bin/env python3
"""Phase medians of the convergence job on app pull requests (#3290).

Usage, from the repository root:

    python3 -I scripts/convergence-phases.py [--jobs N] [--scan M] [--created RANGE]

Reads the latest M pull request runs (default 200) through
`scripts/ci-run-durations.sh "pull request" --all-branches`, keeps the
newest N (default 20) successful convergence jobs that ran the app suites,
and prints one row per job and then the median, minimum, maximum, and sample
count of each phase. RANGE is a `gh run list --created` range, such as
2026-11-11..2026-11-16. Requires authenticated gh and jq.

App runs only. The convergence job runs the app suites only when its caller
sets `app_tests`, and the step `Verify pinned swift-format version` runs only
then: a job counts when that step concluded `success`, not `skipped`. Mixing
the two kinds would report neither (778 s against 153 s at the 2026-11-16
audit).

Phases. Job total is the job's seconds from the jobs API. Setup is the sum of
the job's non-skipped steps before `Run the convergence pass
(scripts/check.sh)`. Inside that step each phase runs from one line's
timestamp in the job log to the next (`gh run view --job ID --log`):

    harness   `##[group]Run bash scripts/check.sh convergence` to `harness ready: api=`
    loading   `running: swift build` to `Building for `
    build     `Building for ` to `Build complete!`
    clone     `Build complete!` to `cloned app/.build K times in`
    tests     the first `HH:MM:SS starting: ` to `HH:MM:SS all runs finished`

The harness phase starts at the step's own group line, so it holds the
harness build whether or not the log prints `building freeside-signet-dev`.
Every log since the screenshot slices (7823db061, 2026-10-25) carries these
lines, and an older log fails on the missing clone line; `run-convergence.sh`
prints no timing line of its own.

Process rows are test time, not wall time. `run-convergence.sh` prints the
main process's log and each screenshot slice's log only after all of them
finish (`==> main.log <==`, `==> shard-I.log <==`), so their line timestamps
are print times. Main and slice I are each the sum of that log's
`Test run with ... passed after S seconds.` lines. They leave out `swift test`
startup, so they don't sum to the test phase, which is wall time.

Fails loudly, exit 1: fewer than N qualifying jobs, a missing or repeated
marker, timestamps out of phase order, a process log with no passing
summary, slice logs not numbered 0 to K-1, or a failed test run or
`run-convergence:` error line. It never reports a short or missing phase.
"""

import argparse
from datetime import datetime
import re
import statistics
import subprocess
import sys

REPOSITORY = 'freeside-ai/freeside'
JOB = 'convergence / convergence (real daemon + Swift client)'
APP_STEP = 'Verify pinned swift-format version'
PASS_STEP = 'Run the convergence pass (scripts/check.sh)'

# Marker lines in the order they appear, each matched against a log line's
# text after its timestamp. `starting` repeats once per process; the parser
# takes the first and checks the count against the clone line.
MARKERS = [
    ('step', re.compile(r'##\[group\]Run bash scripts/check\.sh convergence$')),
    ('ready', re.compile(r'harness ready: api=')),
    ('build_command', re.compile(r'running: swift build ')),
    ('compiling', re.compile(r'Building for ')),
    ('built', re.compile(r'Build complete!')),
    ('cloned', re.compile(r'cloned app/\.build (\d+) times in ')),
    ('starting', re.compile(r'\d\d:\d\d:\d\d starting: ')),
    ('finished', re.compile(r'\d\d:\d\d:\d\d all runs finished$')),
]
PHASES = [('harness', 'step', 'ready'), ('loading', 'build_command', 'compiling'),
          ('build', 'compiling', 'built'), ('clone', 'built', 'cloned'),
          ('tests', 'starting', 'finished')]
PHASE_OF = {marker: phase for phase, start, end in reversed(PHASES) for marker in (end, start)}
SECTION = re.compile(r'==> (main|shard-\d+)\.log <==$')
PASSED = re.compile(r'✔ Test run with .* passed after ([0-9.]+) seconds')
FAILED = re.compile(r'✘ Test run with |run-convergence: ')
STAMP = re.compile(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?Z ?(.*)$')


class LogError(ValueError):
    pass


def log_text(raw):
    """Strip `gh run view --log`'s job and step columns from each line."""
    lines = []
    for line in raw.splitlines():
        fields = line.split('\t', 2)
        lines.append(fields[2] if len(fields) == 3 else line)
    if lines:
        lines[0] = lines[0].lstrip('\ufeff')
    return lines


def stamped(line):
    """(seconds since the epoch, text) of one log line, or None if unstamped."""
    match = STAMP.match(line.lstrip('\ufeff'))
    if not match:
        return None
    # GitHub writes seven fractional digits; datetime takes at most six.
    fraction = (match.group(2) or '')[:6].ljust(6, '0')
    when = datetime.fromisoformat(match.group(1) + '.' + fraction + '+00:00')
    return when.timestamp(), match.group(3)


def parse_log(lines):
    """Phase seconds from a convergence job log's text lines.

    Returns {'harness', 'loading', 'build', 'clone', 'tests', 'main',
    'slice_0', ..., 'slice_K-1'}. Raises LogError naming the phase on any log
    it can't read in full.
    """
    entries = [entry for entry in map(stamped, lines) if entry]
    for _, text in entries:
        if FAILED.match(text):
            raise LogError(f'failed run: {text}')
    first_section = next((i for i, (_, text) in enumerate(entries) if SECTION.match(text)), len(entries))
    live, printed = entries[:first_section], entries[first_section:]

    found, counts, previous = {}, {}, None
    for name, pattern in MARKERS:
        hits = [(when, pattern.match(text)) for when, text in live if pattern.match(text)]
        phase = PHASE_OF[name]
        if not hits:
            raise LogError(f'phase {phase!r}: missing marker {name!r}')
        if len(hits) > 1 and name != 'starting':
            raise LogError(f'phase {phase!r}: marker {name!r} appears {len(hits)} times')
        if previous is not None and hits[0][0] < previous:
            raise LogError(f'phase {phase!r}: timestamps go backward at marker {name!r}')
        found[name], counts[name], previous = hits[0], len(hits), hits[0][0]
    slices = int(found['cloned'][1].group(1))
    if counts['starting'] != slices + 1:
        raise LogError(f"phase 'tests': {counts['starting']} processes started for {slices} slices")
    result = {phase: found[end][0] - found[start][0] for phase, start, end in PHASES}

    sums, current = {}, None
    for _, text in printed:
        header = SECTION.match(text)
        if header:
            current = header.group(1)
            if current in sums:
                raise LogError(f'log {current}.log printed twice')
            sums[current] = []
            continue
        passed = PASSED.match(text)
        if passed and current:
            sums[current].append(float(passed.group(1)))
    expected = ['main'] + [f'shard-{i}' for i in range(slices)]
    if sorted(sums) != sorted(expected):
        raise LogError(f'process logs {sorted(sums)} are not {expected}')
    for name in expected:
        if not sums[name]:
            raise LogError(f'process {name}: no passing test run summary')
    result['main'] = sum(sums['main'])
    result.update({f'slice_{i}': sum(sums[f'shard-{i}']) for i in range(slices)})
    return result


def select_jobs(tsv, count):
    """The newest `count` successful app convergence jobs from the TSV.

    Each is {'run', 'job_id', 'total', 'setup'}. Raises ValueError when fewer
    qualify.
    """
    rows = [line.split('\t') for line in tsv.splitlines()]
    header, rows = rows[0], rows[1:]
    column = {name: header.index(name) for name in
              ('kind', 'run', 'job', 'step', 'conclusion', 'seconds', 'job_id')}
    jobs, steps = [], {}
    for row in rows:
        value = {name: row[i] for name, i in column.items()}
        if value['job'] != JOB:
            continue
        if value['kind'] == 'job':
            jobs.append(value)
        elif value['kind'] == 'step':
            steps.setdefault(value['job_id'], []).append(value)
    chosen = []
    for job in jobs:
        own = steps.get(job['job_id'], [])
        if job['conclusion'] != 'success' or not any(
                s['step'] == APP_STEP and s['conclusion'] == 'success' for s in own):
            continue
        names = [s['step'] for s in own]
        if PASS_STEP not in names:
            raise ValueError(f"run {job['run']} job {job['job_id']}: no step {PASS_STEP!r}")
        before = [s for s in own[:names.index(PASS_STEP)] if s['conclusion'] != 'skipped']
        if not job['seconds'] or any(not s['seconds'] for s in before):
            raise ValueError(f"run {job['run']} job {job['job_id']}: phase 'setup' has an unfinished step")
        chosen.append({'run': job['run'], 'job_id': job['job_id'], 'total': float(job['seconds']),
                       'setup': sum(float(s['seconds']) for s in before)})
        if len(chosen) == count:
            return chosen
    runs = len({row[column['run']] for row in rows if row[column['kind']] == 'job'})
    raise ValueError(f'found {len(chosen)} app convergence jobs in {runs} runs, need {count}')


def report(measured):
    """TSV rows: one per job, then median, minimum, maximum, samples per phase.

    A job with fewer slices leaves the higher slice columns empty, and those
    slices' summaries count fewer samples.
    """
    slices = max(sum(1 for key in m if key.startswith('slice_')) for m in measured)
    names = ['total', 'setup', 'harness', 'loading', 'build', 'clone', 'tests', 'main'] + \
        [f'slice_{i}' for i in range(slices)]
    rows = [['run', 'job_id'] + names]
    for m in measured:
        rows.append([m['run'], m['job_id']] + [f'{m[n]:.1f}' if n in m else '' for n in names])
    rows += [[], ['phase', 'median', 'min', 'max', 'samples']]
    for n in names:
        values = [m[n] for m in measured if n in m]
        rows.append([n, f'{statistics.median(values):.1f}', f'{min(values):.1f}',
                     f'{max(values):.1f}', str(len(values))])
    return '\n'.join('\t'.join(row) for row in rows)


def main(argv):
    parser = argparse.ArgumentParser(description='Convergence job phase medians on app pull requests.')
    parser.add_argument('--jobs', type=int, default=20)
    parser.add_argument('--scan', type=int, default=200)
    parser.add_argument('--created')
    args = parser.parse_args(argv)
    if args.jobs < 1 or args.scan < 1:
        parser.error('--jobs and --scan must be positive')
    command = ['bash', 'scripts/ci-run-durations.sh', 'pull request', '--all-branches',
               '--limit', str(args.scan)] + (['--created', args.created] if args.created else [])
    try:
        jobs = select_jobs(subprocess.check_output(command, text=True), args.jobs)
        measured = []
        for job in jobs:
            raw = subprocess.check_output(['gh', 'run', 'view', '--repo', REPOSITORY,
                                           '--job', job['job_id'], '--log'], text=True)
            try:
                measured.append(dict(job, **parse_log(log_text(raw))))
            except LogError as error:
                raise ValueError(f"run {job['run']} job {job['job_id']}: {error}") from None
    except (ValueError, subprocess.CalledProcessError) as error:
        print(f'convergence-phases: {error}', file=sys.stderr)
        return 1
    print(report(measured))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
