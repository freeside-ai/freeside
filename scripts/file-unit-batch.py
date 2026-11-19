#!/usr/bin/env python3
"""File a batch of unit issues from drafts and check what the forge saved.

Units in a batch cite each other, so filing takes two passes: create every
issue to learn its number, then write the numbers into the bodies. This
script runs both passes, reads each saved body back, and runs
`trackercollect unit` on each new issue (docs/coordination.md, Work-Unit
Issues). It writes no tracker, milestone, pin, or claim.

Usage, from the repository root:

  python3 -I scripts/file-unit-batch.py DIR [--repo OWNER/NAME] [--dry-run]

--repo defaults to freeside-ai/freeside, the Forge Record slug in
AGENTS.md. The forge calls need an authenticated gh.

Input, in DIR:

- manifest.json: {"units": [{"name": ..., "title": ..., "labels": [...]}]},
  the units in creation order. A name matches [a-z0-9][a-z0-9-]*.
- <name>.md for each unit: the issue body. A draft cites a sibling as
  #{{name}}; the script replaces {{name}} with that sibling's number. Any
  other {{...}} in a draft is an input error, so a body can't carry a
  literal {{word}}.

The script checks the input before any forge call. Then:

1. Pass 1 creates each unit that has no recorded number, with a stub body
   and the manifest's title and labels. It prints `created #N name` and
   records the number in DIR/filed.json ({"repo": ..., "issues": {name:
   number}}) right after each create. A rerun reuses recorded numbers and
   creates no duplicate, so after a failure, fix the cause and run the same
   command again. A filed.json recorded for another --repo is an input
   error, so a rerun never edits an unrelated issue that shares a number.
2. Pass 2 substitutes every number into every draft, then saves each body.
   A placeholder left unresolved fails the run before any body is saved.
   Every run saves every body, so a rerun replaces edits made on the forge
   since the last run with the drafts.
3. Each saved body is read back and compared with its resolved draft,
   ignoring line endings and trailing blank lines. A real difference fails
   the run and names the issue.
4. `trackercollect unit` runs on each new issue. An `unknown_relationships`
   entry on that issue fails the run; entries on other issues and other
   UNKNOWN entries are printed and don't. Its exits 2 and 3 are completed
   reads (other open issues may carry such entries); exit 1 or a missing
   unit.json fails the run. The binary is $TRACKERCOLLECT when set, else
   scripts/trackercollect built into a temporary directory (needs go)
   before pass 1, so a failed build writes nothing.

--dry-run prints each title, labels, and resolved body, with unrecorded
numbers left as {{name}}, and makes no forge call and no filed.json write.

Exit codes: 0 when every check passed, 1 when a forge call or check failed,
2 for an input error (nothing on the forge changed).

Accepted risk: if the script dies after `gh issue create` succeeds but
before it records the number, a rerun creates that issue again. The number
is printed the moment it is known, so close the duplicate by hand.
"""

import argparse
import difflib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

DEFAULT_REPO = 'freeside-ai/freeside'
STATE_FILE = 'filed.json'
NAME = re.compile(r'[a-z0-9][a-z0-9-]*')
PLACEHOLDER = re.compile(r'\{\{([^{}]*)\}\}')
STUB = 'This body is being filled by scripts/file-unit-batch.py.\n'
ROOT = Path(__file__).resolve().parent.parent


class InputError(Exception):
    """The input directory is malformed; nothing on the forge was touched."""


class RunError(Exception):
    """A forge call or a tool failed; the run stops."""


# --- input -------------------------------------------------------------------

def load_input(directory, repo):
    """Return (units, drafts, recorded) after checking all of them.

    units: list of {name, title, labels} in creation order. drafts: name ->
    body text. recorded: name -> issue number in repo from an earlier run's
    filed.json, or empty.
    """
    try:
        manifest = json.loads((directory / 'manifest.json').read_text())
    except (OSError, ValueError) as error:
        raise InputError(f'manifest.json: {error}') from None
    units = manifest.get('units') if isinstance(manifest, dict) else None
    if not isinstance(units, list) or not units:
        raise InputError('manifest.json: needs a non-empty "units" list')
    names = []
    for index, unit in enumerate(units):
        where = f'manifest.json: unit {index + 1}'
        if not isinstance(unit, dict) or set(unit) != {'name', 'title', 'labels'}:
            raise InputError(f'{where}: needs exactly "name", "title", and "labels"')
        name, title, labels = unit['name'], unit['title'], unit['labels']
        if not isinstance(name, str) or not NAME.fullmatch(name):
            raise InputError(f'{where}: name {name!r} must match [a-z0-9][a-z0-9-]*')
        if name in names:
            raise InputError(f'{where}: name {name!r} repeats')
        if not isinstance(title, str) or not title.strip():
            raise InputError(f'{where} ({name}): title must be a non-empty string')
        if not isinstance(labels, list) or not all(isinstance(label, str) and label for label in labels):
            raise InputError(f'{where} ({name}): labels must be a list of non-empty strings')
        names.append(name)

    drafts = {}
    for name in names:
        try:
            drafts[name] = (directory / f'{name}.md').read_text()
        except (OSError, ValueError) as error:
            raise InputError(f'{name}.md: {error}') from None
        for found in PLACEHOLDER.findall(drafts[name]):
            if found not in names:
                raise InputError(
                    f'{name}.md: {{{{{found}}}}} names no unit in the manifest '
                    '(a body cannot carry a literal {{word}})')

    recorded = read_state(directory, repo)
    for name in recorded:
        if name not in names:
            raise InputError(f'{STATE_FILE}: records {name!r}, which the manifest lacks')
    return units, drafts, recorded


def read_state(directory, repo):
    path = directory / STATE_FILE
    if not path.exists():
        return {}
    try:
        state = json.loads(path.read_text())
    except (OSError, ValueError) as error:
        raise InputError(f'{STATE_FILE}: {error}') from None
    issues = state.get('issues') if isinstance(state, dict) and set(state) == {'repo', 'issues'} else None
    if not isinstance(issues, dict) or not all(
            isinstance(number, int) and not isinstance(number, bool) and number > 0
            for number in issues.values()):
        raise InputError(f'{STATE_FILE}: must hold "repo" and "issues", a map of name to issue number')
    # Numbers mean nothing in another repository; editing them there would
    # replace unrelated issues' bodies.
    if state['repo'] != repo:
        raise InputError(f"{STATE_FILE}: records issues in {state['repo']!r}, not {repo!r}")
    return issues


def write_state(directory, repo, recorded):
    path = directory / STATE_FILE
    temporary = path.with_name(STATE_FILE + '.tmp')
    temporary.write_text(json.dumps({'repo': repo, 'issues': recorded}, indent=2) + '\n')
    os.replace(temporary, path)


# --- pure checks -----------------------------------------------------------

def resolve(draft, numbers):
    """Replace each {{name}} that numbers holds; leave the rest as written."""
    return PLACEHOLDER.sub(
        lambda match: str(numbers[match.group(1)]) if match.group(1) in numbers else match.group(0),
        draft)


def resolve_all(drafts, numbers):
    """Resolve every draft, or raise naming each placeholder left over."""
    bodies = {name: resolve(draft, numbers) for name, draft in drafts.items()}
    left = [f'{name}.md: {{{{{found}}}}}' for name, body in bodies.items()
            for found in PLACEHOLDER.findall(body)]
    if left:
        raise RunError('unresolved placeholders, no body saved: ' + ', '.join(left))
    return bodies


def normalized(text):
    """Text with CRLF as LF and no trailing newlines or blank lines."""
    lines = text.replace('\r\n', '\n').split('\n')
    while lines and not lines[-1].strip():
        lines.pop()
    return '\n'.join(lines)


def body_difference(saved, draft):
    """A short diff when saved differs from draft beyond normalization."""
    want, got = normalized(draft), normalized(saved)
    if want == got:
        return ''
    diff = difflib.unified_diff(want.split('\n'), got.split('\n'), 'draft', 'saved', lineterm='', n=1)
    return '\n'.join(list(diff)[:20])


def unit_findings(report, number):
    """Split unit.json's entries into (failing, other) lines for one issue."""
    failing, other = [], []
    for entry in report.get('unknown_relationships') or []:
        line = f"UNKNOWN relationship on #{entry.get('issue')}: {entry.get('reason')}: {entry.get('line')!r}"
        (failing if entry.get('issue') == number else other).append(line)
    for entry in report.get('unknowns') or []:
        other.append(f"UNKNOWN {entry.get('code')} {entry.get('subject')}: {entry.get('detail')}")
    return failing, other


# --- forge and tools -------------------------------------------------------

def run(command, what):
    try:
        result = subprocess.run(command, capture_output=True, text=True)
    except OSError as error:
        raise RunError(f'{what}: {error}') from None
    if result.returncode != 0:
        raise RunError(f'{what}: exit {result.returncode}: {result.stderr.strip() or result.stdout.strip()}')
    return result.stdout


def create_issue(repo, unit):
    command = ['gh', 'issue', 'create', '--repo', repo, '--title', unit['title'], '--body', STUB]
    for label in unit['labels']:
        command += ['--label', label]
    output = run(command, f"creating {unit['name']}")
    numbers = re.findall(r'/issues/(\d+)', output)
    if not numbers:
        raise RunError(f"creating {unit['name']}: no issue URL in gh's output: {output.strip()!r}")
    return int(numbers[-1])


def save_body(repo, number, body, scratch):
    path = scratch / f'body-{number}.md'
    path.write_text(body)
    run(['gh', 'issue', 'edit', str(number), '--repo', repo, '--body-file', str(path)],
        f'saving the body of #{number}')


def read_body(repo, number):
    output = run(['gh', 'issue', 'view', str(number), '--repo', repo, '--json', 'body'],
                 f'reading back #{number}')
    try:
        return json.loads(output)['body']
    except (ValueError, KeyError, TypeError):
        raise RunError(f'reading back #{number}: unexpected gh output: {output.strip()[:200]!r}') from None


def trackercollect_binary(scratch):
    configured = os.environ.get('TRACKERCOLLECT')
    if configured:
        return configured
    binary = scratch / 'trackercollect'
    run(['go', '-C', str(ROOT / 'scripts/trackercollect'), 'build', '-o', str(binary), '.'],
        'building trackercollect')
    return str(binary)


def collect_unit(binary, repo, number, scratch):
    """Run `trackercollect unit` on one issue and return its unit.json."""
    out = scratch / f'unit-{number}'
    command = [binary, 'unit', '--repo', f'github.com/{repo}', '--issue', str(number), '--out', str(out)]
    try:
        result = subprocess.run(command, capture_output=True, text=True)
    except OSError as error:
        raise RunError(f'trackercollect unit #{number}: {error}') from None
    if result.returncode not in (0, 2, 3):
        raise RunError(f'trackercollect unit #{number}: exit {result.returncode}: {result.stderr.strip()}')
    try:
        return json.loads((out / 'unit.json').read_text())
    except (OSError, ValueError) as error:
        raise RunError(f'trackercollect unit #{number}: no readable unit.json: {error}') from None


# --- the run ---------------------------------------------------------------

def dry_run(units, drafts, recorded):
    for unit in units:
        name = unit['name']
        number = f'#{recorded[name]}' if name in recorded else '(not created)'
        print(f"=== {name} {number}: {unit['title']}")
        print(f"labels: {', '.join(unit['labels']) or '(none)'}")
        print(resolve(drafts[name], recorded).rstrip('\n'))
        print()


def file_batch(directory, repo, units, drafts, recorded, scratch):
    """Run both passes and the checks; return True when every check passed."""
    binary = trackercollect_binary(scratch)
    # Fail on an unwritable DIR before a create whose number could not be kept.
    write_state(directory, repo, recorded)
    for unit in units:
        if unit['name'] in recorded:
            print(f"recorded #{recorded[unit['name']]} {unit['name']}")
            continue
        number = create_issue(repo, unit)
        print(f"created #{number} {unit['name']}", flush=True)
        recorded[unit['name']] = number
        write_state(directory, repo, recorded)

    bodies = resolve_all(drafts, recorded)
    for unit in units:
        save_body(repo, recorded[unit['name']], bodies[unit['name']], scratch)

    passed = True
    rows = []
    for unit in units:
        name, number = unit['name'], recorded[unit['name']]
        difference = body_difference(read_body(repo, number), bodies[name])
        if difference:
            passed = False
            print(f'#{number} {name}: saved body differs from the draft:\n{difference}', file=sys.stderr)
        try:
            failing, other = unit_findings(collect_unit(binary, repo, number, scratch), number)
        except RunError as error:
            # Keep checking the other units so the report covers all of them.
            failing, other = [str(error)], []
        for line in failing:
            print(f'#{number} {name}: {line}', file=sys.stderr)
        for line in other:
            print(f'#{number} {name}: (not failing) {line}')
        passed = passed and not failing
        rows.append((name, number, 'differs' if difference else 'ok',
                     'failed' if failing else 'ok'))

    print()
    for name, number, body, collected in rows:
        print(f'{name}\t#{number}\thttps://github.com/{repo}/issues/{number}\t'
              f'read-back: {body}\ttrackercollect: {collected}')
    return passed


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('directory', type=Path, help='holds manifest.json and one <name>.md per unit')
    parser.add_argument('--repo', default=DEFAULT_REPO, help=f'OWNER/NAME (default {DEFAULT_REPO})')
    parser.add_argument('--dry-run', action='store_true', help='print resolved bodies; touch nothing')
    options = parser.parse_args(argv)
    if not re.fullmatch(r'[^/\s]+/[^/\s]+', options.repo):
        parser.error(f'--repo {options.repo!r}: expected OWNER/NAME')
    try:
        units, drafts, recorded = load_input(options.directory, options.repo)
    except InputError as error:
        print(f'file-unit-batch: {error}', file=sys.stderr)
        return 2
    if options.dry_run:
        dry_run(units, drafts, recorded)
        return 0
    with tempfile.TemporaryDirectory(prefix='file-unit-batch-') as scratch:
        try:
            passed = file_batch(options.directory, options.repo, units, drafts, recorded, Path(scratch))
        except (RunError, OSError) as error:
            print(f'file-unit-batch: {error}', file=sys.stderr)
            return 1
    if not passed:
        print('file-unit-batch: a check failed; see above', file=sys.stderr)
    return 0 if passed else 1


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
