#!/usr/bin/env bash
# Token-report computations over synthetic task records, ledger, allowance
# reports, and Claude Code and Codex transcripts; fake freesided and gh on
# PATH; no network.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
PYTHONDONTWRITEBYTECODE=1 python3 - "$root/scripts/token-report.py" <<'PY'
import atexit
import collections
import contextlib
import importlib.util
import io
import json
import os
import shutil
import stat
import sys
import tempfile
from pathlib import Path

spec = importlib.util.spec_from_file_location("report", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def expect(label, got, want):
    if got != want:
        raise AssertionError(f"{label}: got {got!r}, want {want!r}")


def t(hour, day=1):
    return f"2026-11-{day:02d}T{hour:02d}:00:00Z"


def tokens(**metrics):
    return [{"kind": "reported_usage", "metric": k, "quantity": v,
             "unit": "usd_micros" if k == "total_cost" else "tokens"}
            for k, v in metrics.items()]


def agent(harness="claude_code", model="claude-opus-5-5"):
    return {"harness_client": harness, "route_model_id": model,
            "selection_model_id": None}


def admission(inv, stage, a=None):
    return {"invocation_id": inv, "stage_id": stage, "agent": a}


def usage(inv, coverage="complete", toks=(), wall=60):
    return {"invocation_id": inv, "coverage": coverage, "tokens": list(toks),
            "wall_seconds": wall}


def run(run_id, admissions=(), reviews=(), usages=(), statuses=None, pr=None, errors=()):
    return {"run_id": run_id, "admissions": list(admissions),
            "review_admissions": list(reviews), "usage": list(usages),
            "invocations": [{"invocation_id": k, "run_id": run_id, "status": v}
                            for k, v in (statuses or {}).items()],
            "publication": ({"outcome": {"repo": "o/r", "pr_number": pr}} if pr else None),
            "errors": [{"section": s, "kind": "integrity", "message": "x"} for s in errors]}


def record(task_id, runs, ready=None, version=m.RECORD_VERSIONS[-1]):
    milestones = [{"run_id": r, "kind": "publication_ready", "recorded_at": at}
                  for r, at in (ready or {}).items()]
    return {"version": version, "task_id": task_id,
            "timeline": {"sections": [{"runs": [{"milestones": milestones}]}]},
            "runs": runs}


# Task A: a failed implementer attempt, then a retry that publishes PR 7 and
# is reviewed by Codex; Task B never publishes.
TASK_A = record("task-a", [
    run("run-spec", [admission("i-spec", "specify-run-spec", agent())],
        usages=[usage("i-spec", toks=tokens(input_tokens=100, output_tokens=10,
                                            total_cost=999))],
        statuses={"i-spec": "completed"}),
    run("run-1", [admission("i-w1", "implement-run-1", agent())],
        usages=[usage("i-w1", "partial", tokens(input_tokens=50), wall=30)],
        statuses={"i-w1": "failed"}),
    run("run-2", [admission("i-w2", "implement-run-2", agent()),
                  admission("i-rv", "implement-run-2", None),
                  admission("i-rm", "remediate-1-run-2", agent("codex_cli", "gpt-5"))],
        reviews=[{"invocation_id": "i-rv", "role": "reviewer",
                  "agent": agent("codex_cli", "gpt-5")}],
        usages=[usage("i-w2", toks=tokens(input_tokens=200, output_tokens=20,
                                          cache_read_input_tokens=1000)),
                usage("i-rv", toks=tokens(input_tokens=300, cached_input_tokens=120,
                                          output_tokens=30)),
                usage("i-rm", "missing", wall=45),
                usage("i-odd", "missing", wall=None)],
        statuses={"i-w2": "completed", "i-rv": "completed"}, pr=7),
], ready={"run-2": t(12)})
TASK_B = record("task-b", [
    run("run-b", [admission("i-b", "operator-feedback-i-b", agent()),
                  admission("i-x", "triage-run-b", agent())],
        usages=[usage("i-b", toks=tokens(output_tokens=5)),
                usage("i-x", toks=tokens(output_tokens=1))],
        errors=["usage"]),
])

a = m.read_task(TASK_A)
expect("ready PR", a["pr"], ("o/r", 7))
expect("specification row, total_cost dropped", {k: v for k, v in a["rows"].items()
       if k[1] == "specification"},
       {("specifier", "specification", "claude_code", "claude-opus-5-5", "completed",
         "input_tokens"): 100,
        ("specifier", "specification", "claude_code", "claude-opus-5-5", "completed",
         "output_tokens"): 10})
expect("failed attempt counts toward the task's PR",
       a["rows"][("implementer", "writer", "claude_code", "claude-opus-5-5", "failed",
                  "input_tokens")], 50)
expect("review joins review_admissions and normalizes Codex input",
       {k[-1]: v for k, v in a["rows"].items() if k[1] == "review"},
       {"input_tokens": 180, "cache_read_input_tokens": 120, "output_tokens": 30})
expect("review role and provider", {k[:5] for k in a["rows"] if k[1] == "review"},
       {("reviewer", "review", "codex_cli", "gpt-5", "completed")})
expect("coverage", a["coverage"], collections.Counter(complete=3, partial=1, missing=2))
expect("wall seconds of partial and missing", (a["gap_wall"], a["gap_wall_unknown"]), (75, 1))
b = m.read_task(TASK_B)
expect("no ready PR", b["pr"], None)
expect("stage prefixes", sorted({k[1] for k in b["rows"]}),
       ["operator_feedback", "other:triage"])
expect("usage error is a gap", b["errors"], [("run-b", ["usage"])])
expect("older root build reads", m.read_task(record("task-c", [], version="freeside-task-record-v21"))["pr"], None)
try:
    m.read_task(record("task-c", [], version="freeside-task-record-v25"))
    raise AssertionError("v25 record was accepted")
except m.ReportError as err:
    expect("version named", "freeside-task-record-v25" in str(err), True)

# Judgment calls: one attributed to run-2, one unobserved, one unattributed.
ledger = {"version": m.LEDGER_VERSIONS[0], "calls": [
    {"site": "publication_author", "root_lineage": "run-2", "called_at": t(11),
     "identity": {"admission": {"role": "publication_author",
                                "enrollment_id": "anthropic-a/claude_code"},
                  "requested_model_id": "claude-haiku-5-5",
                  "observed": {"model_id": "claude-haiku-5-5", "output_tokens": 40}}},
    {"site": "task_namer", "root_lineage": "run-b", "called_at": t(9),
     "identity": {"admission": {}, "observed": None}},
    {"site": "task_namer", "root_lineage": "run-gone", "called_at": t(8),
     "identity": {"admission": {}, "observed": {"output_tokens": 6}}},
    {"site": "task_namer", "root_lineage": "run-2", "called_at": t(10)},
]}
judgment = m.add_judgment([a, b], ledger)
expect("judgment row", a["rows"][("publication_author", "judgment", "claude_code",
                                  "claude-haiku-5-5", "-", "output_tokens")], 40)
expect("judgment gaps", (judgment["missing"], judgment["no_identity"],
                         dict(judgment["unattributed"]), judgment["earliest"]),
       (1, 1, {"task_namer": 6}, t(8)))
try:
    m.add_judgment([], {"version": "freeside.inference-budget/v3"})
    raise AssertionError("v3 ledger was accepted")
except m.ReportError:
    pass

# Allowance: two readings of one window bracket PR 7, published twice; a
# third reading comes after the window reset.
def report(observed, used, resets=t(0, 8)):
    return {"pool": "identity:anthropic-a", "outcome": "collected", "observation": {
        "windows": [{"window": "seven_day", "enrollment_id": "anthropic-a/claude_code",
                     "status": "fresh", "used_percent": used, "resets_at": resets,
                     "observed_at": observed}]}}
allowance = m.readings([report(t(10), 41), report(t(14), 47),
                        report(t(1, 9), 3, t(0, 15))])
changes = m.reading_changes(allowance, [(t(12), "o/r", 7), (t(13), "o/r", 7)])
WINDOW = ("identity:anthropic-a", "seven_day", "anthropic-a/claude_code")
expect("reading change", changes, [(WINDOW, t(10), t(14), 6, 1),
                                   (WINDOW, t(14), t(1, 9), None, 0)])

# Transcripts. PR 21 (issue 20) opened at 12:00 on 2026-11-01, merged 18:00.
tmp = Path(tempfile.mkdtemp())
atexit.register(shutil.rmtree, tmp, ignore_errors=True)
claude_dir, codex_dir = tmp / "claude" / "proj", tmp / "codex"
(claude_dir / "s-handle" / "subagents").mkdir(parents=True)
codex_dir.mkdir()


def write_jsonl(path, entries):
    path.write_text("".join(json.dumps(e) + "\n" for e in entries))


def assistant(msg_id, hour, branch, out, minute=0):
    return {"type": "assistant", "cwd": "/work/freeside", "gitBranch": branch,
            "timestamp": f"2026-11-01T{hour:02d}:{minute:02d}:00Z",
            "message": {"id": msg_id, "model": "claude-opus-5-5", "usage": {
                "input_tokens": 1, "output_tokens": out,
                "cache_creation_input_tokens": 0, "cache_read_input_tokens": 100}}}


def user(text, **extra):
    return {"type": "user", "cwd": "/work/freeside", "isMeta": False,
            "message": {"role": "user", "content": text}, **extra}


write_jsonl(claude_dir / "s-plan.jsonl", [
    user("<command-name>/plan-work-unit</command-name>\n"
         "<command-args>20 after #19 lands</command-args>"),
    assistant("m1", 9, "main", 7)])
write_jsonl(claude_dir / "s-handle.jsonl", [
    user("x", isMeta=True), user("Handle #20, implementation plan in comments."),
    # One message split across three lines carries the same usage each time.
    assistant("m2", 10, "main", 5), assistant("m2", 10, "main", 5, minute=1),
    assistant("m2", 10, "main", 5, minute=2),
    assistant("m3", 13, "feat/x", 3), assistant("m4", 19, "main", 2)])
write_jsonl(claude_dir / "s-handle" / "subagents" / "agent-1.jsonl",
            [user("brief"), assistant("m5", 11, "main", 11)])
(claude_dir / "s-handle" / "subagents" / "agent-1.meta.json").write_text(
    json.dumps({"agentType": "general-purpose", "description": "Fresh-context Review"}))
# A workflow subagent in a review phase, and a plain agent it started.
workflow = claude_dir / "s-handle" / "subagents" / "workflows" / "wf_1"
workflow.mkdir(parents=True)
write_jsonl(workflow / "agent-2.jsonl", [assistant("m8", 11, "main", 13)])
(workflow / "agent-2.meta.json").write_text(json.dumps(
    {"agentType": "workflow-subagent", "description": "check", "workflowPhase": "Review"}))
write_jsonl(workflow / "agent-3.jsonl", [assistant("m9", 11, "main", 17)])
(workflow / "agent-3.meta.json").write_text(json.dumps(
    {"agentType": "general-purpose", "description": "read files", "parentAgentId": "2"}))
write_jsonl(claude_dir / "s-other.jsonl", [user("Handle #99"), assistant("m6", 10, "main", 50)])
write_jsonl(claude_dir / "s-elsewhere.jsonl", [
    {**user("Plan #20"), "cwd": "/work/other"},
    {**assistant("m7", 9, "main", 80), "cwd": "/work/other"}])


def token_count(hour, inp, cached, out, limits=None):
    return {"type": "event_msg", "timestamp": f"2026-11-01T{hour:02d}:00:00Z",
            "payload": {"type": "token_count", "rate_limits": limits, "info": {
                "total_token_usage": {"input_tokens": inp, "cached_input_tokens": cached,
                                      "cache_write_input_tokens": 0, "output_tokens": out,
                                      "reasoning_output_tokens": 1,
                                      "total_tokens": inp + out}}}}


LIMITS = {"primary": {"used_percent": 12.0, "window_minutes": 300, "resets_at": 1},
          "secondary": {"used_percent": 30.0, "window_minutes": 10080, "resets_at": 2}}
write_jsonl(codex_dir / "rollout-a.jsonl", [
    {"type": "session_meta", "payload": {"cwd": "/work/freeside",
                                         "git": {"branch": "feat/x"}}},
    {"type": "turn_context", "payload": {"model": "gpt-5"}},
    {"type": "response_item", "payload": {"type": "message", "role": "user", "content": [
        {"type": "input_text", "text": "<environment_context>review</environment_context>"}]}},
    {"type": "response_item", "payload": {"type": "message", "role": "user", "content": [
        {"type": "input_text", "text": "Fix the failing test"}]}},
    token_count(10, 100, 40, 10), token_count(11, 300, 200, 20),
    token_count(14, 500, 300, 30, LIMITS), token_count(20, 900, 500, 90)])
claude = m.claude_sessions([tmp / "claude"], lambda p: True)
codex = [m.read_codex(p) for p in sorted(codex_dir.glob("rollout-*.jsonl"))]
expect("split message counts once", sum(
    e["usage"]["output_tokens"] for s in claude if s["id"] == "s-handle"
    for e, _ in s["lines"]), 5 + 3 + 2 + 11 + 13 + 17)
cohort = [{"number": 21, "head": "feat/x",
           "created": m.parse_time("2026-11-01T12:00:00Z"),
           "merged": m.parse_time("2026-11-01T18:00:00Z"), "size": 300, "issues": [20]}]
keep = [s for s in claude if any("/work/freeside" in c for c in s["cwds"])]
hand = m.attribute(keep, codex, cohort)
by_stage = collections.Counter()
for (stage, harness, _model, metric), v in hand["rows"][21].items():
    if metric == "output_tokens":
        by_stage[(stage, harness)] += v
expect("hand-run stages", dict(by_stage), {
    ("planning", "claude_code"): 7, ("implementation", "claude_code"): 5,
    ("review", "claude_code"): 11 + 13 + 17, ("review_fix", "claude_code"): 3,
    # Codex: last cumulative before 12:00 (20), then up to the merge (30).
    ("implementation", "codex_cli"): 20, ("review_fix", "codex_cli"): 10})
expect("Codex input excludes cached", hand["rows"][21][
    ("implementation", "codex_cli", "gpt-5", "input_tokens")], 100)
expect("after merge excluded", hand["after_merge"]["output_tokens"], 2 + 60)
expect("unmatched sessions", hand["unmatched"], 1)
expect("Codex limits kept", hand["limits"]["rollout-a"], (21, LIMITS))

# A session naming two cohort PRs splits by branch; the rest is shared.
two = cohort + [{"number": 23, "head": "feat/y", "created": cohort[0]["created"],
                 "merged": None, "size": 10, "issues": [22]}]
write_jsonl(claude_dir / "s-batch.jsonl", [user("Plan #20 and Handle #22"),
                                           assistant("b1", 13, "feat/y", 4),
                                           assistant("b2", 13, "main", 9)])
batch = [s for s in m.claude_sessions([tmp / "claude"], lambda p: True) if s["id"] == "s-batch"]
split = m.attribute(batch, [], two)
expect("batch line on feat/y", split["rows"][23][
    ("review_fix", "claude_code", "claude-opus-5-5", "output_tokens")], 4)
expect("batch shared", split["shared"]["output_tokens"], 9)

# End to end through fake freesided and gh on PATH.
bin_dir, data = tmp / "bin", tmp / "data"
bin_dir.mkdir()
data.mkdir()
(data / "index.json").write_text(json.dumps({"version": m.INDEX_VERSIONS[0], "tasks": [
    {"task_id": "task-a", "created_at": t(8)}, {"task_id": "task-b", "created_at": t(8)},
    {"task_id": "task-old", "created_at": "2026-10-01T00:00:00Z"}]}))
(data / "task-a.json").write_text(json.dumps(TASK_A))
(data / "task-b.json").write_text(json.dumps(TASK_B))
(data / "ledger.json").write_text(json.dumps(ledger))
(data / "allow-1.json").write_text(json.dumps(report(t(10), 41)))
(data / "allow-2.json").write_text(json.dumps(report(t(14), 47)))
prs = {"7": {"number": 7, "headRefName": "freeside/x", "createdAt": t(12),
             "mergedAt": None, "additions": 250, "deletions": 50,
             "closingIssuesReferences": [{"number": 6}]},
       "21": {"number": 21, "headRefName": "feat/x", "createdAt": t(12),
              "mergedAt": t(18), "additions": 280, "deletions": 20,
              "closingIssuesReferences": [{"number": 20}]},
       # A cohort PR whose sessions these transcripts don't reach.
       "24": {"number": 24, "headRefName": "feat/elsewhere", "createdAt": t(12),
              "mergedAt": t(18), "additions": 30, "deletions": 10,
              "closingIssuesReferences": []}}
(data / "prs.json").write_text(json.dumps(prs))
plan = [[{"author_association": "MEMBER", "body": m.PLAN_MARKER + "\nPlan"}]]
(data / "comments-6.json").write_text(json.dumps(plan))
(data / "comments-20.json").write_text(json.dumps(
    [[{"author_association": "NONE", "body": m.PLAN_MARKER}]]))


def executable(path, body):
    path.write_text("#!/usr/bin/env python3\nimport json, sys\nfrom pathlib import Path\n"
                    f"data = Path({str(data)!r})\nargs = sys.argv[1:]\n" + body)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


executable(bin_dir / "freesided", """
if args[:1] == ["inspect"] and "task" not in args:
    print((data / "index.json").read_text())
else:
    print((data / (args[args.index("-task") + 1] + ".json")).read_text())
""")
executable(bin_dir / "gh", """
if args[:2] == ["pr", "view"]:
    print(json.dumps(json.loads((data / "prs.json").read_text())[args[2]]))
else:
    issue = args[-1].split("/")[-2]
    print((data / f"comments-{issue}.json").read_text())
""")
os.environ["PATH"] = f"{bin_dir}{os.pathsep}{os.environ['PATH']}"


def run_main(*cohort):
    out = io.StringIO()
    with contextlib.redirect_stdout(out):
        code = m.main(["--db", "x.db", "--since", "2026-11-01", "--repo", "o/r",
                       "--inference-ledger", str(data / "ledger.json"),
                       "--allowance", str(data / "allow-1.json"),
                       "--allowance", str(data / "allow-2.json"),
                       *[a for n in cohort for a in ("--hand-run-pr", n)],
                       "--claude-transcripts", str(tmp / "claude"),
                       "--codex-transcripts", str(codex_dir), "--cwd-contains", "/work/freeside"])
    expect("exit", code, 0)
    return out.getvalue()


def comparison(text):
    section = text[text.index("## Stage Comparison"):text.index("## Queue-Cap")]
    return [line for line in section.splitlines() if line.startswith("| ")]


text = run_main("21", "24")
expect("unmeasured PR leaves the medians alone", comparison(text), comparison(run_main("21")))
for want in ("### o/r#7\n\nSize 300 lines; current plan comment; 1 task(s), 3 run(s), 6 invocation(s)",
             "| `task-b` | 1 | 2 | 6 |", "| Invocations complete / partial / missing | 5 / 1 / 2 |",
             "| Wall seconds of partial and missing invocations | 75 (plus 1 with no wall time) |",
             "Task `task-b` run `run-b`: unread usage",
             "| identity:anthropic-a | collected | seven_day | anthropic-a/claude_code | fresh | 41 | null "
             "| null |",
             "| identity:anthropic-a | seven_day | anthropic-a/claude_code | " + t(10) + " | "
             + t(14) + " | +6 | 1 |",
             "### #21\n\nSize 300 lines; no current plan comment;",
             "| nothing / failed or lost attempts | 0 | 50 | 0 | 0 |",
             "| nothing / specification on a planned issue | 0 | 110 |",
             "| #21 | primary | 12.0 | 300 | 1 |",
             "### #24\n\nSize 40 lines; no current plan comment; no attributed tokens, so unmeasured",
             "| Hand-run pull requests with no attributed tokens (left out of medians) | #24 |",
             "and 1 measured hand-run pull requests"):
    if want not in text:
        raise AssertionError(f"report lacks {want!r}:\n{text}")
# The allowance section's numbers are the readings, never token totals.
allowance_text = text[text.index("## Allowance Readings"):]
for token_total in ("1,410", "2,010"):
    expect(f"no token total {token_total} in allowance", token_total in allowance_text, False)

(data / "task-a.json").write_text(json.dumps({**TASK_A, "version": "freeside-task-record-v19"}))
err = io.StringIO()
with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err):
    code = m.main(["--db", "x.db", "--repo", "o/r"])
expect("old record exit", code, 1)
expect("old record message", "freeside-task-record-v19" in err.getvalue(), True)
print("token-report: all checks passed")
PY
