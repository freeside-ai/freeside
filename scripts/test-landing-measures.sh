#!/usr/bin/env bash
# Landing-measures computations over fixture JSON and a synthetic git
# repository; no network.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
PYTHONDONTWRITEBYTECODE=1 python3 - "$root/scripts/landing-measures.py" <<'PY'
import importlib.util
import os
import subprocess
import sys
import tempfile

spec = importlib.util.spec_from_file_location("measures", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

ENV = dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null",
           GIT_AUTHOR_NAME="t", GIT_AUTHOR_EMAIL="t@example.invalid",
           GIT_COMMITTER_NAME="t", GIT_COMMITTER_EMAIL="t@example.invalid",
           GIT_AUTHOR_DATE="2026-09-01T00:00:00Z",
           GIT_COMMITTER_DATE="2026-09-01T00:00:00Z")


def t(hour):
    return f"2026-09-01T{hour:02d}:00:00Z"


def pr(number, created, head, merged=None, merge=None, closed=None,
       title="", body="", draft=False):
    return {"number": number, "title": title or f"PR {number}",
            "author": {"login": "dev"}, "isDraft": draft,
            "isCrossRepository": False, "createdAt": created,
            "closedAt": closed or merged, "mergedAt": merged,
            "mergeCommit": {"oid": merge} if merge else None,
            "headRefOid": head, "headRefName": f"b{number}",
            "baseRefName": "main", "body": body}


def run(run_id, number, head, created, conclusion, name="ci", attempt=1,
        event="pull_request", linked=True, branch=None, head_repo="o/r"):
    """linked=False models a closed PR's run, whose PR list GitHub empties."""
    return {"id": run_id, "name": name, "event": event, "head_sha": head,
            "head_branch": branch or f"b{number}", "head_repo": head_repo,
            "status": "completed", "conclusion": conclusion,
            "run_attempt": attempt, "created_at": created,
            "pull_requests": [number] if number and linked else []}


def snapshot(prs, runs=(), timelines=None, jobs=None):
    timelines = timelines or {}
    return {"meta": {"repo": "o/r", "default_branch": "main",
                     "base_sha": "0" * 40, "since": "2026-09-01T00:00:00Z",
                     "until": "2026-09-02T00:00:00Z",
                     "read_at": "2026-09-02T00:00:00Z"},
            "prs": prs, "runs": list(runs), "jobs": jobs or {},
            "timelines": {str(p["number"]): timelines.get(p["number"], [])
                          for p in prs}}


def expect(label, got, want):
    if got != want:
        raise AssertionError(f"{label}: got {got!r}, want {want!r}")


with tempfile.TemporaryDirectory() as repo:
    def git(*args):
        return subprocess.run(["git", "-C", repo, *args], env=ENV, check=True,
                              capture_output=True, text=True).stdout.strip()

    def commit(branch, start, files, message):
        if start:
            git("checkout", "-q", "-B", branch, start)
        for name, text in files.items():
            with open(os.path.join(repo, name), "w") as handle:
                handle.write(text)
        git("add", "-A")
        git("commit", "-q", "-m", message)
        return git("rev-parse", "HEAD")

    git("init", "-q", "-b", "main")
    base = commit("main", None, {"x.txt": "1\n2\n3\n", "y.txt": "y\n",
                                   "z.txt": "z\n"}, "base")
    before = commit("main", base, {"z.txt": "P\n"}, "main edits z")
    a = commit("a", before, {"x.txt": "A\n2\n3\n"}, "A edits x")
    git("checkout", "-q", "main")
    git("merge", "-q", "--no-ff", "-m", "Merge A", a)
    landed = git("rev-parse", "HEAD")
    b1 = commit("b", base, {"x.txt": "B\n2\n3\n"}, "B edits x")
    b2 = commit("b", landed, {"w.txt": "w\n"}, "B rebased onto A")
    c = commit("c", base, {"y.txt": "C\n"}, "C edits y")
    e = commit("e", base, {"x.txt": "E\n2\n3\n", "z.txt": "E\n"}, "E edits x, z")
    g = commit("g", landed, {"g.txt": "g\n"}, "G")

    # Measure 2: A lands at 05:00. B was at b1 then (conflicts in x.txt) and
    # was rebased to b2 afterwards, so its final head is clean. E already
    # conflicted with main in z.txt; only x.txt is A's doing. D was not
    # ready until after the landing. F's head is unknown to git (an error);
    # G's only recorded push is after the landing (a gap). H was ready from
    # 02:00 to 06:00 and again from 07:00; I went back to draft at 03:00;
    # J is a draft with no events. K was stacked on B until 06:00; L was
    # closed from 03:00 to 06:00; M was closed and reopened and retargeted
    # to main before the landing. B's run on b1 lists no PR, so only the
    # branch match records b1 as its head at the landing.
    prs = [pr(1, t(0), a, merged=t(5), merge=landed),
           pr(2, t(1), b2), pr(3, t(1), c), pr(4, t(1), c),
           pr(5, t(1), e), pr(6, t(1), "f" * 40), pr(7, t(1), g),
           pr(8, t(1), c), pr(9, t(1), c, draft=True),
           pr(10, t(1), c, draft=True), pr(11, t(1), c), pr(12, t(1), c),
           pr(13, t(1), c)]
    runs = [run(1, 2, b1, t(2), "success", linked=False), run(2, 2, b2, t(6), "success"),
            run(3, 3, c, t(1), "success"), run(4, 5, e, t(1), "success"),
            run(5, 7, g, t(6), "success")]
    toggle = lambda event, hour: {"event": event, "created_at": t(hour),
                                  "commit_id": None, "body": None, "user": "dev"}
    timelines = {4: [toggle("ready_for_review", 6)],
                 8: [toggle("ready_for_review", 7), toggle("ready_for_review", 2),
                     toggle("convert_to_draft", 6)],
                 9: [toggle("convert_to_draft", 3)],
                 11: [dict(toggle("base_ref_changed", 6), previous="b2",
                           current="main")],
                 12: [toggle("closed", 3), toggle("reopened", 6)],
                 13: [dict(toggle("base_ref_changed", 4), previous="b2",
                           current="main"),
                      toggle("reopened", 3), toggle("closed", 2)]}
    snap = snapshot(prs, runs, timelines)

    expect("ready_at uses the event", m.ready_at(prs[3], timelines[4]),
           m.parse_time(t(6)))
    expect("ready_at falls back to creation", m.ready_at(prs[1], []),
           m.parse_time(t(1)))
    expect("ready between toggles",
           [m.ready_then(prs[7], timelines[8], m.parse_time(t(h)))
            for h in (1, 2, 5, 6, 7)], [False, True, True, False, True])
    # A run on A's branch after A closed, and a fork run on B's branch
    # name, match no PR.
    stray = [run(9, 1, "9" * 40, t(6), "success", linked=False),
             run(10, 2, "9" * 40, t(2), "success", linked=False,
                 head_repo="fork/r")]
    grouped = m.runs_by_pr(prs, snap["timelines"], runs + stray, "o/r")
    expect("runs matched by branch",
           {n: [r["id"] for r in rs] for n, rs in sorted(grouped.items())},
           {2: [1, 2], 3: [3], 5: [4], 7: [5]})
    pairs = m.landing_pairs(snap)
    expect("pairs", [(x["number"], y["number"], head) for x, y, head in pairs],
           [(1, 2, b1), (1, 3, c), (1, 5, e), (1, 6, "f" * 40), (1, 7, None),
            (1, 8, c), (1, 13, c)])

    cache = {}
    conflicts = m.rebase_conflicts(repo, pairs, cache, report=lambda _: None)
    expect("pairs found", conflicts["pairs"], 7)
    expect("pairs checked", conflicts["checked"], 5)
    expect("conflicting pairs", conflicts["conflicting"],
           [(1, 2, ["x.txt"]), (1, 5, ["x.txt"])])
    expect("per-file table", conflicts["files"], [("x.txt", 2, [1, 2, 5])])
    expect("gaps", conflicts["gaps"], [(1, 7, "head at the landing not recorded")])
    expect("errors", [pair[:2] for pair in conflicts["errors"]], [(1, 6)])
    expect("final head is clean", m.merge_tree(repo, landed, b2, {}),
           {"conflicts": []})
    if "error" not in m.merge_tree(repo, landed, "f" * 40, {}):
        raise AssertionError("a bogus head was not reported as an error")
    expect("cache reused", m.merge_tree(
        repo, landed, b1, {" ".join(sorted((landed, b1))): {"conflicts": ["cached"]}}),
        {"conflicts": ["cached"]})

    # Measure 3: ready-to-merge waits of 1, 2, 3, 4, and 10 hours.
    waits = [pr(20 + i, t(0), sha, merged=t(h), merge=landed)
             for i, (h, sha) in enumerate(((1, b1), (2, b2), (3, c), (4, e),
                                           (10, g)))]
    wait_runs = [run(30 + i, 20 + i, p["headRefOid"], t(0), "success")
                 for i, p in enumerate(waits[:4])]
    # #24's head was force-pushed at 08:00; its commit date is 00:00.
    wait_timelines = {24: [{"event": "head_ref_force_pushed", "created_at": t(8),
                            "commit_id": g, "body": None, "user": "dev"}]}
    result = m.wait_to_land(snapshot(waits, wait_runs, wait_timelines))
    expect("ready count", result["from_ready"]["count"], 5)
    expect("ready median", result["from_ready"]["median"], 3.0)
    expect("ready p90", result["from_ready"]["p90"], 10.0)
    expect("push waits use the force-push",
           dict(result["from_push"]["longest"])[24], 2.0)
    expect("push p90", result["from_push"]["p90"], 4.0)
    expect("no unknown push", result["unknown_push"], [])
    # #25's head is a local commit, but no push of it was observed.
    result = m.wait_to_land(snapshot([pr(25, t(0), c, merged=t(1),
                                         merge=landed)]))
    expect("unknown push listed", result["unknown_push"], [25])

    # Runs are read through the last day before the exclusive bound.
    expect("run days", m.run_days(m.parse_time(t(0)),
                                  m.parse_time("2026-09-02T00:00:00Z")),
           (m.dt.date(2026, 8, 18), m.dt.date(2026, 9, 1)))

    # Measure 1. #10 was sent back and merged green; #11 needed a re-run and
    # its cancelled run does not count; #12 merged red and went red on main;
    # #13's failure was superseded by a later success on the same head.
    failing_job = [{"name": "test", "conclusion": "failure", "steps": [
        {"name": "checkout", "conclusion": "success"},
        {"name": "go test", "conclusion": "failure"}]}]
    quiet_job = [{"name": "build", "conclusion": "failure", "steps": [
        {"name": "checkout", "conclusion": "success"}]}]
    landing = [pr(10, t(0), "h2", merged=t(5), merge="m10"),
               pr(11, t(0), "k1", merged=t(5), merge="m11"),
               pr(12, t(0), "n1", merged=t(5), merge="m12"),
               pr(13, t(0), "p1", merged=t(5), merge="m13")]
    landing_runs = [run(100, 10, "h1", t(1), "failure"),
                    run(101, 10, "h1", t(1), "cancelled", name="lint"),
                    run(102, 10, "h2", t(2), "success"),
                    run(103, 11, "k1", t(1), "cancelled", name="lint"),
                    run(104, 11, "k1", t(1), "success", attempt=2),
                    run(105, 12, "n1", t(1), "timed_out"),
                    run(106, None, "m12", t(6), "failure", event="push"),
                    run(107, 13, "p1", t(1), "failure"),
                    run(108, 13, "p1", t(2), "success")]
    failures = m.landing_failures(snapshot(
        landing, landing_runs,
        jobs={"100:1": failing_job, "106:1": quiet_job, "107:1": failing_job}))
    expect("merged", failures["merged"], 4)
    expect("sent back", failures["sent_back"], [10])
    expect("merged red", failures["merged_red"], [12])
    expect("re-run", failures["reran"], [11])
    expect("red after landing", failures["red_after_landing"], [12])
    expect("failing steps", sorted(failures["steps"]),
           [(("ci", "(jobs not read)", ""), 1, [12]),
            (("ci", "build", "(no failing step)"), 1, [12]),
            (("ci", "test", "go test"), 1, [10])])

    # Measure 2's rebase records and measure 4.
    records = m.rebase_records(snapshot([
        pr(40, t(0), "a", merged=t(1), merge="m",
           body="## Why\nrange-diff\n## Verification\n- Checked: git range-diff A...B"),
        pr(41, t(0), "a", merged=t(1), merge="m",
           body="## Why\nrange-diff elsewhere\n## Verification\n- Passed: tests")]))
    expect("rebase records", records, {"merged": 2, "with_range_diff": [40]})
    closed = [pr(50, t(0), "a", closed=t(4), title="Probe | CI"),
              pr(51, t(0), "a", merged=t(4), merge="m")]
    comment = lambda hour, body: {"event": "commented", "created_at": t(hour),
                                  "commit_id": None, "body": body, "user": "dev"}
    rows = m.discarded(snapshot(closed, timelines={
        50: [comment(2, "superseded\nby #52"), comment(6, "after close")]}))
    expect("discarded", [(r["number"], r["last_comment"]) for r in rows],
           [(50, "superseded by #52")])

    page = m.render(snap, failures, conflicts, records,
                    m.wait_to_land(snapshot(waits, wait_runs, wait_timelines)),
                    rows, top=20)
    for heading in ("## 1. Landing failures", "## 2. Rebase conflicts",
                    "## 3. Wait to land", "## 4. Discarded work",
                    "## What this cannot see"):
        if heading not in page:
            raise AssertionError(f"render lacks {heading}")
    if "| `x.txt` | 2 | #1, #2, #5 |" not in page or "Probe \\| CI" not in page:
        raise AssertionError("render lost the conflict table or a table escape")
print("landing-measures: ok")
PY
