#!/usr/bin/env bash
# Landing-measures computations over fixture JSON and a synthetic git
# repository; no network.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
PYTHONDONTWRITEBYTECODE=1 python3 - "$root/scripts/landing-measures.py" <<'PY'
import contextlib
import importlib.util
import io
import json
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


def snapshot(prs, runs=(), timelines=None, jobs=None, roles=None):
    timelines = timelines or {}
    return {"meta": {"repo": "o/r", "default_branch": "main",
                     "base_sha": "0" * 40, "since": "2026-09-01T00:00:00Z",
                     "until": "2026-09-02T00:00:00Z",
                     "read_at": "2026-09-02T00:00:00Z"},
            "prs": prs, "runs": list(runs), "jobs": jobs or {},
            "timelines": {str(p["number"]): timelines.get(p["number"], [])
                          for p in prs},
            "roles": roles or {"owner": "admin", "dev": "write"}}


def comment(hour, body, user="dev", minute=0):
    return {"event": "commented", "created_at": f"2026-09-01T{hour:02d}:{minute:02d}:00Z",
            "commit_id": None, "body": body, "user": user, "label": None}


def labeled(hour, label, user="owner", minute=0, event="labeled"):
    return {"event": event, "created_at": f"2026-09-01T{hour:02d}:{minute:02d}:00Z",
            "commit_id": None, "body": None, "user": user, "label": label}


def unlabeled(hour, label, user="owner", minute=0):
    return labeled(hour, label, user, minute, event="unlabeled")


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
    expect("no hand-backs", (failures["hand_backs"], failures["hand_back_reasons"]),
           ([], []))

    # Measure 1's hand-backs. #60 was handed back twice: once naming a guard
    # in backticks, once with prose only, by a comment 40 minutes before the
    # label. #61's label came with no comment within the hour; its earlier
    # comment is a review reply. #62 was handed back before the window.
    # Someone else's comment just before the label is not the hand-back.
    handed = [pr(60, t(0), "a", closed=t(23)), pr(61, t(0), "a"),
              pr(62, t(0), "a")]
    handed_timelines = {
        60: [comment(3, "Swapping `approved` for `needs-human`: landing "
                        "stopped at `rebase-conflict` in `x.txt`; choose "
                        "which rename wins.", user="owner"),
             comment(3, "I can fix that", user="dev", minute=10),
             labeled(3, "needs-human", minute=20),
             labeled(5, "approved"),
             comment(7, "Stopped at step 2.\nThe landed #99 removed the "
                        "helper this PR calls.", user="owner", minute=20),
             labeled(8, "needs-human")],
        61: [comment(1, "replying to review", user="owner"),
             labeled(9, "needs-human")],
        62: [comment(1, "`checks-failed`", user="owner"),
             {**labeled(1, "needs-human", minute=5),
              "created_at": "2026-08-31T23:00:00Z"}]}
    hand_back_failures = m.landing_failures(snapshot(handed, timelines=handed_timelines))
    expect("hand-backs",
           [(h["number"], h["reason"]) for h in hand_back_failures["hand_backs"]],
           [(60, "rebase-conflict"), (60, "Stopped at step 2."),
            (61, "(no hand-back comment)")])
    expect("hand-back reasons", hand_back_failures["hand_back_reasons"],
           [("rebase-conflict", 1, [60]), ("Stopped at step 2.", 1, [60]),
            ("(no hand-back comment)", 1, [61])])

    # Measure 2's rebase records and measure 4. #42 carries the lander's
    # range-diff comment; #43's only range-diff mention is Codex's.
    records = m.rebase_records(snapshot([
        pr(40, t(0), "a", merged=t(1), merge="m",
           body="## Why\nrange-diff\n## Verification\n- Checked: git range-diff A...B"),
        pr(41, t(0), "a", merged=t(1), merge="m",
           body="## Why\nrange-diff elsewhere\n## Verification\n- Passed: tests"),
        pr(42, t(0), "a", merged=t(2), merge="m"),
        pr(43, t(0), "a", merged=t(2), merge="m")],
        timelines={42: [comment(1, "Rebased onto #41. Renamed the call.\n\n"
                                   "```\n$ git range-diff A B C\n1:  x = 1:  y\n```",
                                user="owner")],
                   43: [comment(1, "the range-diff looks fine",
                                user="chatgpt-codex-connector[bot]"),
                        comment(3, "range-diff after the merge", user="owner")]}))
    expect("rebase records", records,
           {"merged": 4, "with_range_diff": [40], "with_lander_comment": [42]})
    # #53's only hand-back before its close predates the window; the label
    # added after the close is not its close reason.
    closed = [pr(50, t(0), "a", closed=t(4), title="Probe | CI"),
              pr(51, t(0), "a", merged=t(4), merge="m"),
              pr(52, t(0), "a", closed=t(10)),
              pr(53, t(0), "a", closed=t(4))]
    rows = m.discarded(snapshot(closed, timelines={
        50: [comment(2, "superseded\nby #52"), comment(6, "after close")],
        52: [comment(3, "`checks-failed`", user="owner"), labeled(3, "needs-human", minute=1),
             comment(5, "`lease-failed`: someone pushed", user="owner"),
             labeled(5, "needs-human", minute=1)],
        53: [{**comment(1, "`stale-guard`", user="owner"),
              "created_at": "2026-08-31T22:30:00Z"},
             {**labeled(1, "needs-human"), "created_at": "2026-08-31T23:00:00Z"},
             comment(6, "`late-guard`", user="owner"),
             labeled(6, "needs-human", minute=1)]}))
    expect("discarded", [(r["number"], r["last_comment"], r["last_hand_back"])
                         for r in rows],
           [(50, "superseded by #52", ""),
            (52, "`lease-failed`: someone pushed", "lease-failed"),
            (53, "`stale-guard`", "stale-guard")])

    # --offline refuses a snapshot written before the approval reads instead
    # of rendering every merged PR as unapproved.
    with tempfile.TemporaryDirectory() as legacy:
        legacy_snapshot = snapshot(closed)
        del legacy_snapshot["roles"]
        with open(os.path.join(legacy, "forge.json"), "w") as f:
            json.dump(legacy_snapshot, f)
        with contextlib.redirect_stderr(io.StringIO()) as err:
            try:
                m.main(["--offline", "--snapshot", legacy])
            except SystemExit as refusal:
                if refusal.code != 2 or "predates" not in err.getvalue():
                    raise AssertionError(f"legacy snapshot: {refusal.code} "
                                         f"{err.getvalue()!r}")
            else:
                raise AssertionError("--offline rendered a legacy snapshot")

    # Measure 3's approved-to-merge wait: #70's latest collaborator approval
    # is at 02:00 (the 03:00 label is a reader's, not an approver's); #71 was
    # never labelled; #72's approval came from an unknown login; #73's was
    # withdrawn (a hand-back removes the label) and never renewed; #74's was
    # withdrawn and renewed at 02:00.
    approvals = [pr(70, t(0), "a", merged=t(4), merge="m"),
                 pr(71, t(0), "a", merged=t(4), merge="m"),
                 pr(72, t(0), "a", merged=t(4), merge="m"),
                 pr(73, t(0), "a", merged=t(4), merge="m"),
                 pr(74, t(0), "a", merged=t(4), merge="m")]
    approval_waits = m.wait_to_land(snapshot(approvals, timelines={
        70: [labeled(1, "approved"), labeled(2, "approved"),
             labeled(3, "approved", user="reader")],
        72: [labeled(1, "approved", user="ghost")],
        73: [labeled(1, "approved"), unlabeled(2, "approved", user="dev")],
        74: [labeled(1, "approved"), unlabeled(1, "approved", minute=30),
             labeled(2, "approved")]},
        roles={"owner": "admin", "reader": "read", "ghost": ""}))
    expect("approved wait", approval_waits["from_approved"],
           {"count": 2, "median": 2.0, "p90": 2.0,
            "longest": [(70, 2.0), (74, 2.0)]})
    expect("unapproved", approval_waits["unapproved"], [71, 72, 73])

    # fetch_roles reads the permission endpoint as the lander does: a custom
    # role falls back to the base permission, a 404 is no role, and any
    # other failure aborts instead of reporting every approval unqualified.
    responses = {
        "dev": (0, '{"permission": "write", "role_name": "write"}', ""),
        "custom": (0, '{"permission": "write", "role_name": "releaser"}', ""),
        "ghost": (1, '{"message": "ghost is not a user", "status": "404"}',
                  "gh: ghost is not a user (HTTP 404)")}

    def fake_gh(argv, **kwargs):
        code, out, err = responses[argv[2].split("/")[-2]]
        return subprocess.CompletedProcess(argv, code, out, err)
    real_run = m.subprocess.run
    m.subprocess.run = fake_gh
    try:
        expect("roles", m.fetch_roles("o/r", {"dev", "custom", "ghost"}),
               {"dev": "write", "custom": "write", "ghost": ""})
        responses["dev"] = (1, "", "gh: API rate limit exceeded (HTTP 403)")
        try:
            m.fetch_roles("o/r", {"dev"})
        except SystemExit as failure:
            if "HTTP 403" not in str(failure):
                raise AssertionError(f"roles failure lost its cause: {failure}")
        else:
            raise AssertionError("fetch_roles swallowed a non-404 failure")
    finally:
        m.subprocess.run = real_run

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
    for text in ("| lease-failed |", "1 of 4 carry a lander comment",
                 "- Approved to merge: 0 PRs.",
                 "label: 5 PRs (#20, #21, #22, #23, #24)."):
        if text not in page:
            raise AssertionError(f"render lacks {text!r}")
    page = m.render(snapshot(handed), hand_back_failures, conflicts, records,
                    approval_waits, [], top=20)
    for text in ("handed back 3 times", "on 2 PRs.",
                 "- Handed back: #60 (rebase-conflict), #60 (Stopped at step 2.), "
                 "#61 ((no hand-back comment))",
                 "| 1 | rebase-conflict | #60 |",
                 "- Approved to merge: 2 PRs, median 2.0 h, p90 2.0 h; "
                 "longest #70 (2.0 h), #74 (2.0 h).",
                 "label: 3 PRs (#71, #72, #73)."):
        if text not in page:
            raise AssertionError(f"render lacks {text!r}")
print("landing-measures: ok")
PY
