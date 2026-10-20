#!/usr/bin/env python3
"""Print the four landing measures for a date range as Markdown.

The parallel-work proposal (freeasinbird/free-skills#291, §4) asks for four
measures after each wave: how often a PR fails at landing and why, how often
a rebase conflicts and in which files, how long a PR waits to land, and how
much work is discarded. This script reads them from the forge and git. It is
read-only on the forge.

Usage, from the repository root:

  python3 -I scripts/landing-measures.py --since 2026-09-14 \\
      [--until 2026-10-13] [--repo-path DIR] [--snapshot DIR] [--top N]
  python3 -I scripts/landing-measures.py --snapshot DIR --offline

--until is an inclusive date (default: now). --snapshot writes every forge
read to DIR/forge.json and the merge-tree results to DIR/merge-tree.json;
--offline renders from that snapshot and the local repository alone, with no
gh call and no fetch. The fetch layer needs an authenticated gh and the
remote `origin`; the measures need git 2.38 or later for
`merge-tree --write-tree`.

What each measure reads:

1. Landing failures. Every pull_request workflow run on each head a merged
   PR had, and the push runs on its merge commit. A head is red when the
   latest run of any workflow on it concluded failure, timed_out, or
   startup_failure; cancelled runs (the concurrency groups cancel on the
   next push) do not count. Failing runs are grouped by workflow, job, and
   first failing step, from the run's latest attempt. Also: every hand-back
   by the lander (land-approved-prs), read as a `needs-human` label event
   in the window on any PR, keyed by the reason in the hand-back comment
   the same actor posted in the hour before the label: the first
   backticked guard-shaped token (`rebase-conflict`, `checks-failed`), else
   the comment's first line.
2. Rebase conflicts. For each PR that merged in the window (the lander) and
   each other PR open, ready, and based on main at that moment (replayed
   from its closed and reopened, ready_for_review and convert_to_draft, and
   base_ref_changed events; a stacked PR rebases onto its parent, not main),
   the open PR's head as it stood then is merged with `git merge-tree`
   against main just before and just after the landing. Files that conflict only after are the conflicts
   the landing forced on the open PR's next rebase. The open PR's head at
   the landing is its latest push before it, read from its runs and its
   force-push events. Final heads are not used: under the rebase rule a PR
   that landed later already contains the earlier one, so its final head
   would never conflict. Also, as two separate counts: merged PR bodies
   whose Verification section mentions range-diff, the provisional reading
   of #2020's rebase record, and merged PRs that carry a comment by a
   non-bot user mentioning range-diff, the lander's record of a rebase it
   resolved or reworked.
3. Wait to land. Per merged PR, hours from ready-for-review (the last
   ready_for_review event, else creation) to merge, from the last push
   (the push of the merged head) to merge, and from approval (the latest
   `approved` label event whose actor holds write, maintain, or admin on
   the repository, the landing record's Approver permission, and whose
   label was still on the PR at the merge; a removal, such as the lander's
   hand-back, withdraws it) to merge.
4. Discarded work. PRs closed unmerged in the window, with the last comment
   before the close and the last hand-back reason when the lander handed
   the PR back, for a human to classify.

What it cannot see:

- Measure 1 counts every workflow, not only the `required` check #2017
  added; filtering to that check's job is a later change.
- The rebase record of #2020 has no fixed format yet; the range-diff matcher
  is provisional until #2020 defines it. The lander's comments have no fixed
  format either, so a comment that mentions range-diff for another reason
  also matches.
- A hand-back comment has no fixed format; the reason is a heuristic read of
  it, and a `needs-human` label with no comment by the same actor in the
  previous hour reads as "(no hand-back comment)".
- A label actor's role is read when the script runs, not when the label was
  added, the same reading the lander's list uses.
- GitHub records no close reason for a PR; measure 4 lists, it does not
  classify.
- `merge-tree --write-tree` writes tree and blob objects into the object
  store of --repo-path, and fetched PR heads are kept under
  refs/landing-measures/. Use a scratch clone for a large run, or remove
  them afterwards with
  `git for-each-ref --format='delete %(refname)' refs/landing-measures/ |
  git update-ref --stdin` and `git gc`.
- `merge-tree` merges trees; a rebase replays commits. A PR whose later
  commit undoes an earlier one can merge cleanly yet conflict on the
  earlier commit during a rebase, so measure 2 can undercount such PRs.
- GitHub empties a run's PR list once the PR closes. A same-repository
  run is then matched by head branch to the PR open on that branch when the
  run was created; a fork run only by head SHA to the PR's known heads (the
  merged head and force-push events), which misses heads seen only in runs.
- A push that triggered no workflow (GitHub runs no pull_request workflow
  while a PR conflicts with its base), one older than the run lookback
  (RUN_LOOKBACK_DAYS before --since), or one after --until leaves no record
  unless it was a force-push. A PR whose head at a landing is unknown is
  listed as a gap, not counted; a PR with no recorded push at all is taken
  to have held its final head. A merged PR with no push record for its
  merged head has no last-push wait.
- The Actions runs API returns at most 1,000 runs per query; the script
  reads one day at a time and fails if a day exceeds that.
"""

import argparse
import collections
import datetime as dt
import json
import math
import os
import re
import statistics
import subprocess
import sys
from pathlib import Path

FAILED = {"failure", "timed_out", "startup_failure"}
RUN_LOOKBACK_DAYS = 14
PR_FIELDS = ("number,title,author,isDraft,isCrossRepository,createdAt,closedAt,"
             "mergedAt,mergeCommit,headRefOid,headRefName,baseRefName,body")
TIMELINE_EVENTS = {"ready_for_review", "convert_to_draft", "closed",
                   "reopened", "base_ref_changed", "head_ref_force_pushed",
                   "commented", "labeled", "unlabeled"}
# The landing record in AGENTS.md: the lander takes PRs labelled APPROVAL by
# a collaborator at or above the Approver permission and hands them back
# with HAND_BACK after a comment that names the guard or reason.
APPROVAL_LABEL = "approved"
HAND_BACK_LABEL = "needs-human"
BASE_ROLES = {"read", "triage", "write", "maintain", "admin"}
APPROVER_ROLES = {"write", "maintain", "admin"}
HAND_BACK_COMMENT_WINDOW = dt.timedelta(hours=1)
GUARD_TOKEN = re.compile(r"`([a-z]+(?:-[a-z]+)+)`")
BASE_CHANGES = """query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) {
    timelineItems(first: 100, itemTypes: [BASE_REF_CHANGED_EVENT]) {
      filteredCount
      nodes { ... on BaseRefChangedEvent {
        createdAt previousRefName currentRefName } } } } } }"""
SEARCH_LIMIT = 1000
GIT_ISOLATED = dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null",
                    GIT_CONFIG_SYSTEM="/dev/null")


def progress(message):
    print(message, file=sys.stderr, flush=True)


def parse_time(text):
    return dt.datetime.fromisoformat(text.replace("Z", "+00:00"))


def iso(moment):
    return moment.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


# --- fetch layer: the only code that calls gh or fetches -------------------

def run(argv, cwd=None):
    result = subprocess.run(argv, cwd=cwd, capture_output=True, text=True)
    if result.returncode != 0:
        raise SystemExit(f"landing-measures: {' '.join(argv[:4])} failed "
                         f"(exit {result.returncode}): {result.stderr.strip()}")
    return result.stdout


def gh_json(args, cwd=None):
    return json.loads(run(["gh", *args], cwd=cwd))


def gh_pages(path):
    """Every page of a REST read, as a list of page bodies."""
    return gh_json(["api", "--paginate", "--slurp", path])


def fetch_prs(repo, since, until):
    prs = {}
    searches = (["--state", "all", "--search", f"updated:>={since.date()}"],
                ["--state", "open"])
    for search in searches:
        found = gh_json(["pr", "list", "-R", repo, *search,
                         "--limit", str(SEARCH_LIMIT), "--json", PR_FIELDS])
        if len(found) >= SEARCH_LIMIT:
            raise SystemExit("landing-measures: PR search hit its 1,000-result "
                             "limit; narrow the window")
        for pr in found:
            prs[pr["number"]] = pr
    return sorted((pr for pr in prs.values()
                   if parse_time(pr["createdAt"]) < until
                   and (pr["closedAt"] is None
                        or parse_time(pr["closedAt"]) >= since)),
                  key=lambda pr: pr["number"])


def fetch_timeline(repo, number):
    events = [event for page in gh_pages(
        f"repos/{repo}/issues/{number}/timeline?per_page=100") for event in page]
    kept = [{"event": e["event"], "created_at": e.get("created_at"),
             "commit_id": e.get("commit_id"), "body": e.get("body"),
             "user": (e.get("user") or e.get("actor") or {}).get("login"),
             "label": (e.get("label") or {}).get("name")}
            for e in events if e.get("event") in TIMELINE_EVENTS]
    if any(e["event"] == "base_ref_changed" for e in kept):
        # The REST timeline omits the branch names; GraphQL carries them.
        kept = [e for e in kept if e["event"] != "base_ref_changed"]
        kept += fetch_base_changes(repo, number)
    return kept


def fetch_base_changes(repo, number):
    owner, name = repo.split("/")
    items = gh_json(["api", "graphql", "-f", f"query={BASE_CHANGES}",
                     "-f", f"owner={owner}", "-f", f"name={name}",
                     "-F", f"number={number}"]
                    )["data"]["repository"]["pullRequest"]["timelineItems"]
    if items["filteredCount"] > len(items["nodes"]):
        raise SystemExit(f"landing-measures: #{number} changed base more "
                         f"than {len(items['nodes'])} times")
    return [{"event": "base_ref_changed", "created_at": n["createdAt"],
             "commit_id": None, "body": None, "user": None, "label": None,
             "previous": n["previousRefName"], "current": n["currentRefName"]}
            for n in items["nodes"]]


def fetch_roles(repo, logins):
    """Each login's repository role, or "" for none, read as the lander
    reads it: a custom role's name falls back to the base permission the
    endpoint reports beside it. A login GitHub no longer knows is a 404 and
    holds no role; any other failure aborts, like the other forge reads."""
    roles = {}
    for login in sorted(logins):
        argv = ["gh", "api", f"repos/{repo}/collaborators/{login}/permission"]
        result = subprocess.run(argv, capture_output=True, text=True)
        if result.returncode != 0:
            if "HTTP 404" in result.stderr:
                roles[login] = ""
                continue
            raise SystemExit(f"landing-measures: {' '.join(argv)} failed "
                             f"(exit {result.returncode}): "
                             f"{result.stderr.strip()}")
        data = json.loads(result.stdout)
        role = data.get("role_name") or ""
        roles[login] = (role if role in BASE_ROLES
                        else data.get("permission") or "")
    return roles


def fetch_runs(repo, event, branch, first_day, last_day):
    runs = []
    day = first_day
    while day <= last_day:
        query = f"event={event}&created={day}&per_page=100"
        if branch:
            query += f"&branch={branch}"
        pages = gh_pages(f"repos/{repo}/actions/runs?{query}")
        if pages and pages[0]["total_count"] > 1000:
            raise SystemExit(f"landing-measures: more than 1,000 {event} runs "
                             f"on {day}; the runs API cannot list them all")
        for page in pages:
            for r in page["workflow_runs"]:
                runs.append({
                    "id": r["id"], "name": r["name"], "event": r["event"],
                    "head_sha": r["head_sha"], "head_branch": r["head_branch"],
                    "head_repo": (r.get("head_repository") or {}).get("full_name"),
                    "status": r["status"],
                    "conclusion": r["conclusion"],
                    "run_attempt": r["run_attempt"],
                    "created_at": r["created_at"],
                    "pull_requests": [p["number"] for p in r["pull_requests"]]})
        day += dt.timedelta(days=1)
    return runs


def fetch_jobs(repo, run_id):
    jobs = [job for page in gh_pages(
        f"repos/{repo}/actions/runs/{run_id}/jobs?per_page=100")
        for job in page["jobs"]]
    return [{"name": j["name"], "conclusion": j["conclusion"],
             "steps": [{"name": s["name"], "conclusion": s["conclusion"]}
                       for s in j.get("steps") or []]} for j in jobs]


def has_commit(repo_path, sha):
    return subprocess.run(["git", "-C", repo_path, "cat-file", "-e",
                           f"{sha}^{{commit}}"], capture_output=True).returncode == 0


def fetch_commits(repo_path, shas):
    """Fetch commits by SHA and keep them under refs/landing-measures/."""
    missing = sorted(sha for sha in shas if not has_commit(repo_path, sha))
    progress(f"fetching {len(missing)} commits missing locally")
    fetch = ["git", "-C", repo_path, "fetch", "-q", "--no-tags", "origin"]
    for start in range(0, len(missing), 50):
        chunk = missing[start:start + 50]
        if subprocess.run([*fetch, *chunk], capture_output=True).returncode:
            # One unknown SHA fails the whole chunk; fetch one at a time so
            # the rest still arrive and the unknown ones surface as gaps.
            for sha in chunk:
                subprocess.run([*fetch, sha], capture_output=True)
    keep = "".join(f"update refs/landing-measures/{sha} {sha}\n"
                   for sha in missing if has_commit(repo_path, sha))
    if keep:
        subprocess.run(["git", "-C", repo_path, "update-ref", "--stdin"],
                       input=keep, text=True, check=True)


def fetch_snapshot(repo_path, since, until, jobs_cache):
    repo_view = gh_json(["repo", "view", "--json",
                         "nameWithOwner,defaultBranchRef"], cwd=repo_path)
    repo = repo_view["nameWithOwner"]
    default_branch = repo_view["defaultBranchRef"]["name"]
    run(["git", "-C", repo_path, "fetch", "-q", "--no-tags", "origin",
         default_branch])
    base_sha = run(["git", "-C", repo_path, "rev-parse",
                    f"origin/{default_branch}"]).strip()
    prs = fetch_prs(repo, since, until)
    progress(f"read {len(prs)} PRs; reading their timelines")
    timelines = {}
    for index, pr in enumerate(prs, 1):
        timelines[str(pr["number"])] = fetch_timeline(repo, pr["number"])
        if index % 25 == 0:
            progress(f"  timelines {index}/{len(prs)}")
    approvers = {e["user"] for events in timelines.values() for e in events
                 if e["event"] == "labeled" and e["label"] == APPROVAL_LABEL
                 and e["user"]}
    progress(f"reading the repository role of {len(approvers)} label actors")
    roles = fetch_roles(repo, approvers)
    first_day, last_day = run_days(since, until)
    progress(f"reading workflow runs {first_day} to {last_day}")
    runs = (fetch_runs(repo, "pull_request", None, first_day, last_day)
            + fetch_runs(repo, "push", default_branch, first_day, last_day))
    jobs = {}
    failed = [r for r in runs if r["conclusion"] in FAILED]
    progress(f"reading jobs for {len(failed)} failed runs")
    for r in failed:
        key = f"{r['id']}:{r['run_attempt']}"
        jobs[key] = jobs_cache.get(key) or fetch_jobs(repo, r["id"])
    snapshot = {"meta": {"repo": repo, "default_branch": default_branch,
                         "base_sha": base_sha, "since": iso(since),
                         "until": iso(until), "read_at": iso(now())},
                "prs": prs, "timelines": timelines, "runs": runs, "jobs": jobs,
                "roles": roles}
    fetch_commits(repo_path, needed_commits(snapshot))
    return snapshot


def run_days(since, until):
    """The first and last UTC day of runs to read: the lookback before
    since, through the last day before the exclusive bound until."""
    return ((since - dt.timedelta(days=RUN_LOOKBACK_DAYS)).date(),
            (until - dt.timedelta(microseconds=1)).date())


def needed_commits(snapshot):
    shas = set()
    for pr in snapshot["prs"]:
        shas.add(pr["headRefOid"])
        if pr["mergeCommit"]:
            shas.add(pr["mergeCommit"]["oid"])
        for event in snapshot["timelines"][str(pr["number"])]:
            if event["commit_id"]:
                shas.add(event["commit_id"])
    shas.update(r["head_sha"] for r in snapshot["runs"]
                if r["event"] == "pull_request")
    return shas


def now():
    return dt.datetime.now(dt.timezone.utc)


# --- compute layer: pure over the snapshot plus a local repository ---------

def window(snapshot):
    return (parse_time(snapshot["meta"]["since"]),
            parse_time(snapshot["meta"]["until"]))


def merged_in(prs, since, until):
    return [pr for pr in prs if pr["mergedAt"]
            and since <= parse_time(pr["mergedAt"]) < until]


def ready_at(pr, events):
    readies = [parse_time(e["created_at"]) for e in events
               if e["event"] == "ready_for_review"]
    return max(readies) if readies else parse_time(pr["createdAt"])


def ready_then(pr, events, moment):
    """Whether the PR was ready for review (not a draft) at a moment. The
    first toggle event tells the state at creation; with none, the current
    draft state held throughout."""
    toggles = sorted((parse_time(e["created_at"]), e["event"]) for e in events
                     if e["event"] in ("ready_for_review", "convert_to_draft"))
    ready = (toggles[0][1] == "convert_to_draft" if toggles
             else not pr["isDraft"])
    for at, event in toggles:
        if at > moment:
            break
        ready = event == "ready_for_review"
    return ready


def open_then(pr, events, moment):
    """Whether the PR was open at a moment: created, not yet finally
    closed, and not inside an earlier closed interval it was reopened from."""
    end = closed_at(pr)
    if parse_time(pr["createdAt"]) > moment or (end and end <= moment):
        return False
    is_open = True
    for at, event in sorted((parse_time(e["created_at"]), e["event"])
                            for e in events
                            if e["event"] in ("closed", "reopened")):
        if at > moment:
            break
        is_open = event == "reopened"
    return is_open


def base_then(pr, events, moment):
    """The PR's base branch at a moment. The first base change names the
    base at creation; with none, the current base held throughout."""
    changes = sorted((e for e in events if e["event"] == "base_ref_changed"),
                     key=lambda e: parse_time(e["created_at"]))
    base = changes[0]["previous"] if changes else pr["baseRefName"]
    for e in changes:
        if parse_time(e["created_at"]) > moment:
            break
        base = e["current"]
    return base


def closed_at(pr):
    return parse_time(pr["closedAt"]) if pr["closedAt"] else None


def runs_by_pr(prs, timelines, runs, repo):
    """pull_request runs per PR number. A run matches the PRs it references;
    GitHub empties that list once a PR closes, so a run with none matches a
    same-repository PR by head branch while the PR was open, and a fork run
    by head SHA to the PR's known heads."""
    by_branch = collections.defaultdict(list)
    for pr in prs:
        if not pr["isCrossRepository"]:
            by_branch[pr["headRefName"]].append(pr)
    owner = {}
    for pr in prs:
        known = {pr["headRefOid"]} | {
            e["commit_id"] for e in timelines[str(pr["number"])]
            if e["event"] == "head_ref_force_pushed" and e["commit_id"]}
        for sha in known:
            owner.setdefault(sha, pr["number"])
    numbers = {pr["number"] for pr in prs}
    grouped = collections.defaultdict(list)
    for r in runs:
        if r["event"] != "pull_request":
            continue
        refs = [n for n in r["pull_requests"] if n in numbers]
        if not refs and r.get("head_repo") == repo:
            at = parse_time(r["created_at"])
            refs = [pr["number"] for pr in by_branch.get(r.get("head_branch"), [])
                    if parse_time(pr["createdAt"]) <= at
                    and (pr["closedAt"] is None or at <= parse_time(pr["closedAt"]))]
        if not refs and r["head_sha"] in owner:
            refs = [owner[r["head_sha"]]]
        for n in refs:
            grouped[n].append(r)
    return grouped


def pushes(pr, events, pr_runs):
    """(time, sha) for every recorded head of a PR, oldest first. A head's
    time is its force-push event, else its earliest pull_request run."""
    first = {}
    for r in pr_runs:
        at = parse_time(r["created_at"])
        if r["head_sha"] not in first or at < first[r["head_sha"]]:
            first[r["head_sha"]] = at
    for e in events:
        if e["event"] == "head_ref_force_pushed" and e["commit_id"]:
            first[e["commit_id"]] = parse_time(e["created_at"])
    return sorted((at, sha) for sha, at in first.items())


def head_at(pr, recorded, moment):
    """The PR's head at a moment, or None when no record shows it."""
    before = [sha for at, sha in recorded if at <= moment]
    if before:
        return before[-1]
    if not any(at > moment for at, _ in recorded):
        return pr["headRefOid"]  # never pushed after: the final head held
    return None


def red_heads(pr_runs):
    """Heads whose latest run of some workflow failed, with those runs."""
    latest = {}
    for r in pr_runs:
        key = (r["head_sha"], r["name"])
        if key not in latest or r["created_at"] > latest[key]["created_at"]:
            latest[key] = r
    red = collections.defaultdict(list)
    for (sha, _), r in latest.items():
        if r["conclusion"] in FAILED:
            red[sha].append(r)
    return red


def failing_steps(run_record, jobs):
    key = f"{run_record['id']}:{run_record['run_attempt']}"
    if key not in jobs:
        return [(run_record["name"], "(jobs not read)", "")]
    rows = []
    for job in jobs[key]:
        if job["conclusion"] in FAILED:
            step = next((s["name"] for s in job["steps"]
                         if s["conclusion"] in FAILED), "(no failing step)")
            rows.append((run_record["name"], job["name"], step))
    return rows or [(run_record["name"], "(no failing job)", "")]


def hand_back_reason(body):
    """The guard or reason a hand-back comment names: its first backticked
    guard-shaped token that is not one of the two labels, else its first
    line."""
    for match in GUARD_TOKEN.finditer(body or ""):
        if match.group(1) not in (APPROVAL_LABEL, HAND_BACK_LABEL):
            return match.group(1)
    lines = [line.strip() for line in (body or "").splitlines() if line.strip()]
    return " ".join(lines[0].split())[:120] if lines else "(empty comment)"


def pr_hand_backs(pr, events):
    """Every HAND_BACK label event on one PR, whenever it happened, with the
    reason from the comment the same actor posted in the hour before it,
    oldest first."""
    found = []
    for e in events:
        if e["event"] != "labeled" or e.get("label") != HAND_BACK_LABEL:
            continue
        at = parse_time(e["created_at"])
        comments = [c for c in events if c["event"] == "commented"
                    and c["user"] == e["user"] and c["created_at"]
                    and at - HAND_BACK_COMMENT_WINDOW
                    <= parse_time(c["created_at"]) <= at]
        reason = (hand_back_reason(comments[-1]["body"]) if comments
                  else "(no hand-back comment)")
        found.append({"number": pr["number"], "at": at,
                      "actor": e["user"], "reason": reason})
    return sorted(found, key=lambda h: h["at"])


def hand_backs(snapshot):
    """Every hand-back in the window, oldest first."""
    since, until = window(snapshot)
    found = [h for pr in snapshot["prs"]
             for h in pr_hand_backs(pr, snapshot["timelines"][str(pr["number"])])
             if since <= h["at"] < until]
    return sorted(found, key=lambda h: (h["at"], h["number"]))


def approved_at(pr, events, roles):
    """When the APPROVAL label still on the PR at the merge was last added
    by a collaborator at or above the Approver permission, or None. A
    removal withdraws the approval whoever removes it: the lander's
    hand-back strips the label, and a merge without a fresh approval
    after that was not a label-based landing."""
    merged_at = parse_time(pr["mergedAt"])
    changes = sorted((e for e in events if e.get("label") == APPROVAL_LABEL
                      and e["event"] in ("labeled", "unlabeled")
                      and parse_time(e["created_at"]) <= merged_at),
                     key=lambda e: parse_time(e["created_at"]))
    active = None
    for e in changes:
        if e["event"] == "unlabeled":
            active = None
        elif roles.get(e["user"]) in APPROVER_ROLES:
            active = parse_time(e["created_at"])
    return active


def landing_failures(snapshot):
    since, until = window(snapshot)
    prs, timelines, jobs = snapshot["prs"], snapshot["timelines"], snapshot["jobs"]
    by_pr = runs_by_pr(prs, timelines, snapshot["runs"],
                       snapshot["meta"]["repo"])
    push_runs = collections.defaultdict(list)
    for r in snapshot["runs"]:
        if r["event"] == "push":
            push_runs[r["head_sha"]].append(r)
    merged = merged_in(prs, since, until)
    sent_back, merged_red, reran, red_after, no_runs = [], [], [], [], []
    table = collections.Counter()
    table_prs = collections.defaultdict(set)
    for pr in merged:
        n, final = pr["number"], pr["headRefOid"]
        pr_runs = by_pr.get(n, [])
        if not pr_runs:
            no_runs.append(n)
        red = red_heads(pr_runs)
        if any(sha != final for sha in red):
            sent_back.append(n)
        if final in red:
            merged_red.append(n)
        if any(r["head_sha"] == final and r["run_attempt"] > 1
               and r["conclusion"] == "success" for r in pr_runs):
            reran.append(n)
        landed_red = red_heads(push_runs.get(pr["mergeCommit"]["oid"], []))
        if landed_red:
            red_after.append(n)
        for runs in [*red.values(), *landed_red.values()]:
            for r in runs:
                for row in failing_steps(r, jobs):
                    table[row] += 1
                    table_prs[row].add(n)
    handed = hand_backs(snapshot)
    reasons = collections.Counter(h["reason"] for h in handed)
    reason_prs = collections.defaultdict(set)
    for h in handed:
        reason_prs[h["reason"]].add(h["number"])
    return {"merged": len(merged), "sent_back": sent_back,
            "merged_red": merged_red, "reran": reran,
            "red_after_landing": red_after, "no_runs": no_runs,
            "steps": [(row, count, sorted(table_prs[row]))
                      for row, count in table.most_common()],
            "hand_backs": handed,
            "hand_back_reasons": [(reason, count, sorted(reason_prs[reason]))
                                  for reason, count in reasons.most_common()]}


def merge_tree(repo_path, ours, theirs, cache):
    """Conflicted paths of a prospective merge, or an error.

    Exit 0 is clean; exit 1 with a tree OID on the first line is a conflict
    and the following lines are the paths; anything else is an error, which
    a bogus argument also produces with exit 1 but no OID."""
    key = " ".join(sorted((ours, theirs)))
    if key in cache:
        return cache[key]
    result = subprocess.run(
        ["git", "-C", repo_path, "merge-tree", "--write-tree", "--name-only",
         "--no-messages", ours, theirs],
        capture_output=True, text=True, env=GIT_ISOLATED)
    lines = result.stdout.splitlines()
    tree = lines[0] if lines else ""
    is_oid = len(tree) in (40, 64) and all(c in "0123456789abcdef" for c in tree)
    if result.returncode == 0 and is_oid:
        outcome = {"conflicts": []}
    elif result.returncode == 1 and is_oid:
        outcome = {"conflicts": sorted({line for line in lines[1:] if line})}
    else:
        outcome = {"error": f"exit {result.returncode}: "
                            f"{result.stderr.strip()[:200]}"}
    cache[key] = outcome
    return outcome


def landing_pairs(snapshot):
    """(lander, open PR, the open PR's head at the landing or None) for each
    PR merged in the window and each other PR open, ready, and based on the
    default branch at that time."""
    since, until = window(snapshot)
    prs, timelines = snapshot["prs"], snapshot["timelines"]
    default_branch = snapshot["meta"]["default_branch"]
    by_pr = runs_by_pr(prs, timelines, snapshot["runs"],
                       snapshot["meta"]["repo"])
    recorded = {pr["number"]: pushes(pr, timelines[str(pr["number"])],
                                     by_pr.get(pr["number"], []))
                for pr in prs}
    pairs = []
    for lander in merged_in(prs, since, until):
        landed = parse_time(lander["mergedAt"])
        for other in prs:
            if other is lander:
                continue
            events = timelines[str(other["number"])]
            if (open_then(other, events, landed)
                    and ready_then(other, events, landed)
                    and base_then(other, events, landed) == default_branch):
                pairs.append((lander, other,
                              head_at(other, recorded[other["number"]], landed)))
    return pairs


def rebase_conflicts(repo_path, pairs, cache, report=progress):
    """Per pair, the files that conflict against main after the landing but
    not before it: the conflicts the landing forced on the open PR."""
    checked, conflicting, gaps, errors = 0, [], [], []
    for index, (lander, other, head) in enumerate(pairs, 1):
        if index % 200 == 0:
            report(f"  merge-tree {index}/{len(pairs)}")
        label = (lander["number"], other["number"])
        if head is None:
            gaps.append((*label, "head at the landing not recorded"))
            continue
        after_sha = lander["mergeCommit"]["oid"]
        before = merge_tree(repo_path, f"{after_sha}^1", head, cache)
        after = merge_tree(repo_path, after_sha, head, cache)
        failed = before.get("error") or after.get("error")
        if failed:
            errors.append((*label, failed))
            continue
        checked += 1
        forced = sorted(set(after["conflicts"]) - set(before["conflicts"]))
        if forced:
            conflicting.append((*label, forced))
    files = collections.defaultdict(lambda: [0, set()])
    for lander, other, paths in conflicting:
        for path in paths:
            files[path][0] += 1
            files[path][1].update((lander, other))
    table = sorted(((path, count, sorted(prs))
                    for path, (count, prs) in files.items()),
                   key=lambda row: (-row[1], row[0]))
    return {"pairs": len(pairs), "checked": checked, "conflicting": conflicting,
            "files": table, "gaps": gaps, "errors": errors}


def verification_section(body):
    lines, inside = [], False
    for line in (body or "").splitlines():
        if line.startswith("## "):
            inside = line[3:].strip().lower().startswith("verification")
        elif inside:
            lines.append(line)
    return "\n".join(lines)


def lander_rebase_comment(pr, events):
    """Whether a non-bot comment before the merge mentions range-diff: the
    lander comments the range-diff of a rebase it resolved or reworked."""
    merged_at = parse_time(pr["mergedAt"])
    return any(e["event"] == "commented" and e["created_at"]
               and parse_time(e["created_at"]) <= merged_at
               and not (e["user"] or "").endswith("[bot]")
               and "range-diff" in (e["body"] or "").lower()
               for e in events)


def rebase_records(snapshot):
    since, until = window(snapshot)
    merged = merged_in(snapshot["prs"], since, until)
    return {"merged": len(merged),
            "with_range_diff": [pr["number"] for pr in merged
                                if "range-diff" in
                                verification_section(pr["body"]).lower()],
            "with_lander_comment": [
                pr["number"] for pr in merged
                if lander_rebase_comment(
                    pr, snapshot["timelines"][str(pr["number"])])]}


def percentile(values, fraction):
    """Nearest-rank percentile."""
    ordered = sorted(values)
    return ordered[max(0, math.ceil(fraction * len(ordered)) - 1)]


def wait_to_land(snapshot):
    since, until = window(snapshot)
    prs, timelines = snapshot["prs"], snapshot["timelines"]
    by_pr = runs_by_pr(prs, timelines, snapshot["runs"],
                       snapshot["meta"]["repo"])
    roles = snapshot["roles"]
    from_ready, from_push, from_approved = {}, {}, {}
    unknown_push, unapproved = [], []
    for pr in merged_in(prs, since, until):
        n, merged_at = pr["number"], parse_time(pr["mergedAt"])
        events = timelines[str(n)]
        from_ready[n] = (merged_at - ready_at(pr, events)).total_seconds() / 3600
        approved = approved_at(pr, events, roles)
        if approved is None:
            unapproved.append(n)
        else:
            from_approved[n] = (merged_at - approved).total_seconds() / 3600
        times = dict((sha, at) for at, sha in pushes(pr, events, by_pr.get(n, [])))
        # No commit date stands in: a commit can be pushed long after it
        # was made, so an unobserved push stays unknown.
        pushed = times.get(pr["headRefOid"])
        if pushed is None:
            unknown_push.append(n)
        else:
            from_push[n] = (merged_at - pushed).total_seconds() / 3600
    return {"from_ready": summarize(from_ready),
            "from_push": summarize(from_push),
            "from_approved": summarize(from_approved),
            "unknown_push": unknown_push, "unapproved": unapproved}


def summarize(hours_by_pr):
    if not hours_by_pr:
        return {"count": 0}
    values = list(hours_by_pr.values())
    longest = sorted(hours_by_pr.items(), key=lambda kv: -kv[1])[:5]
    return {"count": len(values), "median": statistics.median(values),
            "p90": percentile(values, 0.9), "longest": longest}


def discarded(snapshot):
    since, until = window(snapshot)
    rows = []
    for pr in snapshot["prs"]:
        closed = closed_at(pr)
        if pr["mergedAt"] or closed is None or not since <= closed < until:
            continue
        events = snapshot["timelines"][str(pr["number"])]
        comments = [e for e in events if e["event"] == "commented"
                    and e["created_at"] and parse_time(e["created_at"]) <= closed]
        last = " ".join((comments[-1]["body"] or "").split())[:160] if comments else ""
        # The PR's own history up to the close, not the window: a hand-back
        # before --since is still its last, and a label after the close is
        # not a close reason.
        handed = [h for h in pr_hand_backs(pr, events) if h["at"] <= closed]
        rows.append({"number": pr["number"], "title": pr["title"],
                     "author": (pr["author"] or {}).get("login", ""),
                     "closed_at": pr["closedAt"], "last_comment": last,
                     "last_hand_back": handed[-1]["reason"] if handed else ""})
    return rows


# --- rendering --------------------------------------------------------------

def cell(text):
    return str(text).replace("|", "\\|").replace("\n", " ")


def pr_list(numbers, limit=30):
    shown = ", ".join(f"#{n}" for n in numbers[:limit])
    return shown + (f", and {len(numbers) - limit} more" if len(numbers) > limit else "")


def hours(value):
    return f"{value:.1f} h"


def render(snapshot, failures, conflicts, records, waits, discards, top):
    meta = snapshot["meta"]
    out = [f"# Landing Measures: {meta['since'][:10]} to {meta['until'][:16]}Z",
           "",
           f"Repository `{meta['repo']}`, `{meta['default_branch']}` at "
           f"`{meta['base_sha']}`, forge read at {meta['read_at']}. "
           f"{len(snapshot['prs'])} PRs were open during the window.",
           "", "## 1. Landing failures", "",
           f"Of {failures['merged']} PRs merged: {len(failures['sent_back'])} "
           f"had a red head before the head that merged (sent back); "
           f"{len(failures['merged_red'])} merged with a red head; "
           f"{len(failures['reran'])} needed a re-run on the merged head; "
           f"{len(failures['red_after_landing'])} merge commits went red on "
           f"`{meta['default_branch']}`. Every workflow counts, not only "
           f"the required check. The lander handed back "
           f"{len(failures['hand_backs'])} times "
           f"(`{HAND_BACK_LABEL}` label events in the window) on "
           f"{len({h['number'] for h in failures['hand_backs']})} PRs.", ""]
    for label, key in (("Sent back", "sent_back"), ("Merged red", "merged_red"),
                       ("Re-run to green", "reran"),
                       ("Red after landing", "red_after_landing")):
        if failures[key]:
            out.append(f"- {label}: {pr_list(failures[key])}")
    if failures["hand_backs"]:
        handed = failures["hand_backs"]
        out.append("- Handed back: " + ", ".join(
            f"#{h['number']} ({cell(h['reason'])})" for h in handed[:30])
            + (f", and {len(handed) - 30} more" if len(handed) > 30 else ""))
    out += ["", "| Failing runs | Workflow | Job | First failing step | PRs |",
            "| ---: | --- | --- | --- | --- |"]
    for (workflow, job, step), count, numbers in failures["steps"][:top]:
        out.append(f"| {count} | {cell(workflow)} | {cell(job)} | {cell(step)} "
                   f"| {pr_list(numbers, 8)} |")
    if failures["hand_back_reasons"]:
        out += ["", "| Hand-backs | Reason | PRs |", "| ---: | --- | --- |"]
        for reason, count, numbers in failures["hand_back_reasons"][:top]:
            out.append(f"| {count} | {cell(reason)} | {pr_list(numbers, 8)} |")
    out += ["", "## 2. Rebase conflicts", "",
            f"{len(conflicts['conflicting'])} of {conflicts['checked']} "
            f"landing pairs checked forced a conflict on the open PR "
            f"({conflicts['pairs']} pairs found; {len(conflicts['gaps'])} "
            f"without a recorded head, {len(conflicts['errors'])} merge-tree "
            f"errors). A pair is a PR that merged and a PR open, ready, and "
            f"based on `{meta['default_branch']}` at that moment.", "",
            f"Rebase records (provisional until #2020): "
            f"{len(records['with_range_diff'])} of {records['merged']} merged "
            f"PR bodies mention `range-diff` in Verification"
            + (f" ({pr_list(records['with_range_diff'])})"
               if records["with_range_diff"] else "")
            + f"; {len(records['with_lander_comment'])} of {records['merged']} "
            f"carry a lander comment mentioning `range-diff`"
            + (f" ({pr_list(records['with_lander_comment'])})."
               if records["with_lander_comment"] else "."), "",
            "| Path | Conflicting pairs | PRs |", "| --- | ---: | --- |"]
    for path, count, numbers in conflicts["files"][:top]:
        out.append(f"| `{cell(path)}` | {count} | {pr_list(numbers, 12)} |")
    if len(conflicts["files"]) > top:
        out.append(f"\n{len(conflicts['files']) - top} more paths not shown.")
    out += ["", "## 3. Wait to land", ""]
    for label, key in (("Ready to merge", "from_ready"),
                       ("Last push to merge", "from_push"),
                       ("Approved to merge", "from_approved")):
        s = waits[key]
        if not s["count"]:
            out.append(f"- {label}: 0 PRs.")
            continue
        longest = ", ".join(f"#{n} ({hours(h)})" for n, h in s["longest"])
        out.append(f"- {label}: {s['count']} PRs, median {hours(s['median'])}, "
                   f"p90 {hours(s['p90'])}; longest {longest}.")
    out.append(f"- Merged without a collaborator's `{APPROVAL_LABEL}` label: "
               f"{len(waits['unapproved'])} PRs"
               + (f" ({pr_list(waits['unapproved'])})."
                  if waits["unapproved"] else "."))
    out += ["", "## 4. Discarded work", "",
            f"{len(discards)} PRs closed unmerged. GitHub records no close "
            f"reason; classify each by hand. Last hand-back is the lander's "
            f"reason for the last `{HAND_BACK_LABEL}` label before the close, "
            "whenever it was added.",
            ""]
    if discards:
        out += ["| PR | Title | Author | Closed | Last comment before close "
                "| Last hand-back |",
                "| --- | --- | --- | --- | --- | --- |"]
        for row in discards:
            out.append(f"| #{row['number']} | {cell(row['title'])} | "
                       f"{cell(row['author'])} | {row['closed_at'][:10]} | "
                       f"{cell(row['last_comment'])} | "
                       f"{cell(row['last_hand_back'])} |")
    out += ["", "## What this cannot see", "",
            "- Measure 1 counts every workflow, not only the required check "
            "#2017 added.",
            "- The range-diff matcher is provisional until #2020 fixes the "
            "rebase record's format; the lander's comments have no fixed "
            "format either, so any non-bot comment mentioning range-diff "
            "counts.",
            "- A hand-back comment has no fixed format: its reason is the "
            "first backticked guard-shaped token, else its first line, and "
            f"a `{HAND_BACK_LABEL}` label with no comment by the same actor "
            "in the previous hour reads as no comment.",
            "- A label actor's role is read when the script runs, not when "
            "the label was added.",
            "- GitHub records no close reason, so measure 4 is unclassified.",
            "- A push that ran no workflow, or one more than "
            f"{RUN_LOOKBACK_DAYS} days before the window, leaves no head "
            "record.",
            "- merge-tree merges trees, not the commits a rebase replays; "
            "a PR that undoes its own earlier change can hide a conflict.",
            "- Fork PRs' runs are matched by head SHA only, so a fork "
            "head seen only in runs is missed."]
    if failures["no_runs"]:
        out.append(f"- Merged PRs with no pull_request runs read: "
                   f"{pr_list(failures['no_runs'])}.")
    if waits["unknown_push"]:
        out.append(f"- Merged PRs with no push time for the merged head: "
                   f"{pr_list(waits['unknown_push'])}.")
    for lander, other, reason in conflicts["gaps"][:10]:
        out.append(f"- Pair #{lander} then #{other} not checked: {reason}.")
    if len(conflicts["gaps"]) > 10:
        out.append(f"- {len(conflicts['gaps']) - 10} more unchecked pairs.")
    for lander, other, reason in conflicts["errors"]:
        out.append(f"- Pair #{lander} then #{other}: merge-tree {cell(reason)}.")
    return "\n".join(out) + "\n"


# --- command line -----------------------------------------------------------

def day(text):
    return dt.datetime.strptime(text, "%Y-%m-%d").replace(tzinfo=dt.timezone.utc)


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--since", type=day)
    parser.add_argument("--until", type=day,
                        help="inclusive end date (default: now)")
    parser.add_argument("--repo-path", default=".")
    parser.add_argument("--snapshot", type=Path)
    parser.add_argument("--offline", action="store_true")
    parser.add_argument("--top", type=int, default=20)
    args = parser.parse_args(argv)
    forge_file = args.snapshot / "forge.json" if args.snapshot else None
    tree_file = args.snapshot / "merge-tree.json" if args.snapshot else None
    cache = (json.loads(tree_file.read_text())
             if tree_file and tree_file.is_file() else {})
    if args.offline:
        if not forge_file or not forge_file.is_file():
            parser.error("--offline needs --snapshot DIR holding forge.json")
        snapshot = json.loads(forge_file.read_text())
        if "roles" not in snapshot:
            # Written before the lander reads: no roles, no label events.
            parser.error(f"{forge_file} predates the approval and hand-back "
                         "reads; re-read it without --offline")
        since, until = window(snapshot)
        if (args.since and args.since != since) or (args.until and
                                                    args.until + dt.timedelta(days=1) != until):
            parser.error("--since/--until differ from the snapshot's window")
    else:
        if not args.since:
            parser.error("--since is required unless --offline")
        until = args.until + dt.timedelta(days=1) if args.until else now()
        if until <= args.since:
            parser.error("--until is before --since")
        old = (json.loads(forge_file.read_text())
               if forge_file and forge_file.is_file() else {})
        snapshot = fetch_snapshot(args.repo_path, args.since, until,
                                  old.get("jobs", {}))
        if forge_file:
            args.snapshot.mkdir(parents=True, exist_ok=True)
            forge_file.write_text(json.dumps(snapshot, indent=1))
    pairs = landing_pairs(snapshot)
    progress(f"checking {len(pairs)} landing pairs with merge-tree")
    try:
        conflicts = rebase_conflicts(args.repo_path, pairs, cache)
    finally:
        if tree_file:
            args.snapshot.mkdir(parents=True, exist_ok=True)
            tree_file.write_text(json.dumps(cache, indent=0, sort_keys=True))
    sys.stdout.write(render(snapshot, landing_failures(snapshot), conflicts,
                            rebase_records(snapshot),
                            wait_to_land(snapshot),
                            discarded(snapshot), args.top))


if __name__ == "__main__":
    main(sys.argv[1:])
