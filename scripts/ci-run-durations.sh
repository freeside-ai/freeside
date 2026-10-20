#!/usr/bin/env bash
# Print job and step seconds, then successful-job medians, for recent CI runs.
# Usage: ci-run-durations.sh <workflow-name> [--branch main] [--limit N]
# Cohorts: ci-run-durations.sh --cohort samples.json --out DIRECTORY
# Requires authenticated gh and jq. Run from the repository to measure.
# Re-runs report the latest attempt; save each report before the next re-run.
set -euo pipefail

# Explicit run/attempt cohorts preserve the raw API responses. A rerun needs
# a separately recorded trigger timestamp; GitHub's original created_at is
# never substituted for it. The default job/step report below stays intact.
if [[ ${1:-} == --cohort ]]; then
  [[ $# == 4 && $3 == --out ]] || { echo 'usage: ci-run-durations.sh --cohort samples.json --out DIRECTORY' >&2; exit 2; }
  python3 - "$2" "$4" <<'PY'
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import statistics
import subprocess
import sys

manifest, destination = Path(sys.argv[1]), Path(sys.argv[2])
destination.mkdir(parents=True, exist_ok=True)

def require(ok, message):
    if not ok:
        raise ValueError(message)

def stamp(value):
    require(isinstance(value, str) and value.endswith('Z'), 'missing UTC timestamp')
    return datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp()

def api(path, name, pages=False):
    command = ['gh', 'api', 'repos/freeside-ai/freeside/' + path]
    if pages:
        command += ['--paginate', '--slurp']
    raw = subprocess.check_output(command, text=True)
    (destination / name).write_text(raw)
    return json.loads(raw)

def job_name(job):
    return job['name'].removeprefix('daemon / ')

def duration(job):
    require(job.get('status') == 'completed', 'unfinished job or step')
    seconds = stamp(job.get('completed_at')) - stamp(job.get('started_at'))
    require(seconds >= 0, 'reversed job or step timestamps')
    return seconds

def measure(sample, run, jobs, original):
    result = {'cohort': sample['cohort'], 'run': sample['run'], 'attempt': sample['attempt'],
              'head': run.get('head_sha'), 'bases': [p['base']['sha'] for p in run.get('pull_requests', [])],
              'event': run.get('event'), 'status': run.get('status'),
              'conclusion': run.get('conclusion'), 'complete': False,
              'excluded_reason': sample.get('excluded_reason'), 'jobs': jobs}
    try:
        require(run['id'] == sample['run'] and run['run_attempt'] == sample['attempt'],
                'run/attempt identity mismatch')
        require(original['id'] == sample['run'], 'original run identity mismatch')
        trigger = run['created_at'] if sample['attempt'] == 1 else sample.get('triggered_at')
        if sample['attempt'] > 1:
            require(sample.get('trigger_source'), 'rerun requires a verified trigger source')
        trigger = stamp(trigger)
        # The attempt endpoint creates a new record after the rerun request.
        # Only the canonical run retains the original creation timestamp.
        require(trigger >= stamp(original['created_at']), 'trigger precedes run creation')
        result['triggered_at'] = datetime.fromtimestamp(trigger, timezone.utc).isoformat()
        result['trigger_source'] = 'run.created_at' if sample['attempt'] == 1 else sample['trigger_source']
        expected = {'linux (build, test, vet, lint)', 'macos (build, test)'}
        count = sample.get('integration_shards', 0)
        require(type(count) is int and count in (0, 2, 3, 4), 'invalid integration shard count')
        if count:
            expected.add('linux (build, vet, lint)')
            for platform in ('linux', 'macos'):
                expected.add(f'{platform} (test, rest 1/1)')
                expected.update(f'{platform} (test, integration {i}/{count})' for i in range(1, count + 1))
        work = [j for j in jobs if re.match(r'^(linux|macos) \(', job_name(j))]
        require(len(work) == len(expected) and {job_name(j) for j in work} == expected,
                'missing, duplicate, or unexpected daemon jobs')
        require(all(j.get('run_attempt') == sample['attempt'] and j.get('run_id') == sample['run'] for j in jobs),
                'job belongs to another run or attempt')
        end = {}
        for platform, name in [('linux', 'linux (build, test, vet, lint)'), ('macos', 'macos (build, test)')]:
            gate = next(j for j in work if job_name(j) == name)
            end[platform] = stamp(gate.get('completed_at'))
            result[platform + '_seconds'] = end[platform] - trigger
        first = min(stamp(j.get('started_at')) for j in jobs)
        require(first >= trigger, 'job starts before trigger')
        result['initial_queue_seconds'] = first - trigger
        result['daemon_seconds'] = max(end.values()) - trigger
        result['daemon_runner_seconds'] = sum(duration(j) for j in work)
        result['total_runner_seconds'] = (sum(duration(j) for j in jobs if j.get('conclusion') != 'skipped')
                                          if all(j.get('status') == 'completed' for j in jobs) else None)
        required = [j for j in jobs if j['name'] == 'required']
        result['required_seconds'] = (stamp(required[0]['completed_at']) - trigger
                                      if len(required) == 1 and required[0].get('completed_at') else None)
        select = [j for j in jobs if j['name'] == 'select components']
        if run.get('event') == 'pull_request':
            require(len(select) == 1 and len(required) == 1 and result['required_seconds'] is not None,
                    'missing PR selection or required gate')
            require(required[0].get('conclusion') == 'success', 'required gate did not succeed')
        ready = stamp(select[0]['completed_at']) if len(select) == 1 else trigger
        result['internal_queue_seconds'] = {}
        result['steps'] = []
        for job in work:
            name = job_name(job)
            parent_end = ready
            if count and name in ('linux (build, test, vet, lint)', 'macos (build, test)'):
                platform = name.split()[0]
                parent_end = max(stamp(j['completed_at']) for j in work
                                 if job_name(j).startswith(platform + ' (') and j is not job)
            queue = stamp(job['started_at']) - parent_end
            require(queue >= 0, 'child starts before prerequisite completion')
            result['internal_queue_seconds'][name] = queue
            for step in job.get('steps', []):
                if step.get('conclusion') == 'skipped':
                    continue
                result['steps'].append({'job': name, 'step': step['name'],
                                        'conclusion': step.get('conclusion'), 'seconds': duration(step)})
        require(all(j.get('conclusion') == 'success' for j in work), 'daemon child or gate did not succeed')
        if select:
            require(len(select) == 1 and select[0].get('conclusion') == 'success', 'selection failed')
        require(all(end[p] >= stamp(j['completed_at']) for p in end for j in work
                    if job_name(j).startswith(p + ' (')), 'gate finished before child')
        require(run.get('status') == 'completed' and run.get('conclusion') == 'success'
                and result['total_runner_seconds'] is not None,
                'attempt is unfinished or unsuccessful; total runner cost is not a successful sample')
        result['complete'] = not bool(result['excluded_reason'])
    except (KeyError, TypeError, ValueError, StopIteration) as error:
        result['error'] = str(error)
    return result

try:
    samples = json.loads(manifest.read_text())
    require(isinstance(samples, list) and samples, 'cohort must be a nonempty array')
    identities = set()
    reports = []
    for sample in samples:
        require(sample.get('cohort') in ('baseline', 'candidate'), 'unknown cohort')
        run_id, attempt = sample['run'], sample['attempt']
        require(type(run_id) is int and run_id > 0 and type(attempt) is int and attempt > 0,
                'invalid run/attempt identity')
        require((run_id, attempt) not in identities, 'duplicate run/attempt')
        identities.add((run_id, attempt))
        prefix = f'{run_id}-{attempt}'
        run = api(f'actions/runs/{run_id}/attempts/{attempt}', prefix + '-run.json')
        original = (api(f'actions/runs/{run_id}', prefix + '-original-run.json')
                    if attempt > 1 else run)
        pages = api(f'actions/runs/{run_id}/attempts/{attempt}/jobs?per_page=100',
                    prefix + '-jobs.json', pages=True)
        jobs = [j for page in pages for j in page['jobs']]
        reports.append(measure(sample, run, jobs, original))
    summaries = {}
    for cohort in ('baseline', 'candidate'):
        valid = [r for r in reports if r['cohort'] == cohort and r['complete']]
        if valid:
            summaries[cohort] = {key: statistics.median(r[key] for r in valid) for key in
                                ('daemon_seconds', 'linux_seconds', 'macos_seconds',
                                 'daemon_runner_seconds', 'total_runner_seconds')}
            summaries[cohort]['successful_samples'] = len(valid)
    output = {'samples': reports, 'medians': summaries}
    # Timing is only one part of acceptance. This report never claims coverage,
    # matched production/toolchain/images, cache comparability, or shard limits.
    (destination / 'report.json').write_text(json.dumps(output, indent=2) + '\n')
    print('cohort\trun/attempt\tcomplete\tdaemon_seconds\tlinux_seconds\tmacos_seconds\trequired_seconds\terror')
    for r in reports:
        print('\t'.join(str(r.get(k, '')) for k in ('cohort',)) + '\t' + f"{r['run']}/{r['attempt']}\t" +
              '\t'.join(str(r.get(k, '')) for k in ('complete', 'daemon_seconds', 'linux_seconds',
                        'macos_seconds', 'required_seconds', 'error')))
    print(json.dumps({'medians': summaries}, sort_keys=True))
except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError) as error:
    print(f'ci-run-durations: {error}', file=sys.stderr)
    sys.exit(1)
PY
  exit $?
fi

usage() {
  echo "usage: $0 <workflow-name> [--branch main] [--limit N]" >&2
  exit 2
}

[[ $# -gt 0 ]] || usage
workflow=$1
shift
branch=main
limit=6
while [[ $# -gt 0 ]]; do
  case $1 in
    --branch) [[ $# -ge 2 && -n $2 ]] || usage; branch=$2; shift 2 ;;
    --limit) [[ $# -ge 2 ]] || usage; limit=$2; shift 2 ;;
    *) usage ;;
  esac
done
[[ $limit =~ ^[1-9][0-9]*$ ]] || usage

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
gh run list --workflow "$workflow" --branch "$branch" --limit "$limit" \
  --json databaseId,headSha,createdAt >"$scratch/runs.json"
jq -r '.[].databaseId' "$scratch/runs.json" >"$scratch/ids"
while IFS= read -r run_id; do
  gh api --paginate "repos/{owner}/{repo}/actions/runs/$run_id/jobs?per_page=100" \
    | jq -c '.jobs[]' >>"$scratch/jobs.jsonl"
done <"$scratch/ids"
touch "$scratch/jobs.jsonl"

jq -rs --slurpfile runs "$scratch/runs.json" '
  def seconds:
    if .started_at != null and .completed_at != null and .status == "completed"
    then (.completed_at | fromdateiso8601) - (.started_at | fromdateiso8601)
    else null end;
  def median:
    sort | length as $n |
    if $n % 2 == 1 then .[$n / 2 | floor]
    else (.[($n / 2) - 1] + .[$n / 2]) / 2 end;
  . as $jobs |
  (["kind", "run", "attempt", "sha", "job", "step", "conclusion", "seconds", "samples"] | @tsv),
  ($jobs[] | . as $job |
    ($runs[0][] | select(.databaseId == $job.run_id) | .headSha) as $sha |
    (["job", .run_id, .run_attempt, $sha, .name, "", .conclusion, seconds, ""] | @tsv),
    (.steps[] | ["step", $job.run_id, $job.run_attempt, $sha, $job.name,
      .name, .conclusion, seconds, ""] | @tsv)),
  ($jobs | map(select(.conclusion == "success" and seconds != null)) |
    group_by(.name)[] |
    ["median", "", "", "", .[0].name, "", "success",
      (map(seconds) | median), length] | @tsv)
' "$scratch/jobs.jsonl"
