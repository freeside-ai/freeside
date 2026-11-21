#!/usr/bin/env python3
"""Report tokens per ready pull request against a hand-run baseline (#3416).

Freeside's specifier, reviewer, and remediator replace spend the hand-run
flow already pays for: `Plan #N` sessions, the fresh-context review before
every push, and review-fix rounds. This report shows where Freeside spends
more than that flow for the same work, and prints the inputs for sizing an
overnight queue cap (#3417). It is read-only: it never runs
`freesided auth refresh-usage`, never writes the forge, and prints no
prompt or message text.

Usage, from the repository root:

  python3 -I scripts/token-report.py --db <root db> [--freesided BIN]
      [--since DATE] [--until DATE] [--inference-ledger FILE]
      [--allowance FILE]... [--repo OWNER/NAME] [--hand-run-pr N]...
      [--claude-transcripts DIR]... [--codex-transcripts DIR]...
      [--cwd-contains TEXT]...

--since and --until (inclusive) filter tasks on their `created_at`; --since
also skips transcript files last modified before it. --cwd-contains keeps only
transcript sessions whose working directory contains one of the given
strings, so another project's `Plan #N` or branch never matches. The report
prints Markdown to stdout.

What each section reads:

- Freeside tasks: `freesided inspect -db` (freeside-inspect-index-v1) and
  `freesided inspect task -db -task` (freeside-task-record-v20); any other
  version stops the run. Each run's `usage` entry joins on invocation id to
  `review_admissions` (stage review, its role) or else `admissions` (stage
  from the stage id prefix: specify- specification, implement- writer,
  remediate- remediation, operator-feedback- operator_feedback, anything
  else other:<prefix>). Provider is the agent's `harness_client`, model its
  `route_model_id`, else `selection_model_id`; a null agent prints unknown.
  Only `tokens`-unit totals count; `total_cost` is not a token count. A
  Codex review records `input_tokens` including `cached_input_tokens`, so
  those rows are normalized to the Claude metric names (input minus cached,
  cached as cache_read_input_tokens) to keep stage totals comparable.
- Ready pull requests: a task's ready pull request is the
  `publication.outcome` of any of its runs; every run of the task counts
  toward it, failed attempts included. Its ready time is the run's
  `publication_ready` milestone in the record's timeline.
- Judgment calls: `inference-budget.json` (freeside.inference-budget/v4).
  A call's `root_lineage` is a run id; its `identity.observed.output_tokens`
  counts, and a null `observed` counts as missing.
- Allowance: saved `freesided auth refresh-usage` output, one JSON report
  per line or per file. Readings print as the provider reported them; a
  change between two readings of the same pool, window, and enrollment
  prints with the count of ready pull requests in that period.
- Hand-run cohort: `gh pr view` for each --hand-run-pr and each Freeside
  pull request, and each closing issue's comments for a current
  implementation plan. Claude Code sessions (`<session>.jsonl` and
  any `agent-*.jsonl` under `<session>/subagents/`, workflow subagents
  included, with its `.meta.json`) count each
  assistant `message.id` once. Codex rollouts count the last cumulative
  `total_token_usage` before each cut (input minus cached as input, cached
  as cache read, `cache_write_input_tokens` as cache creation, output).

Hand-run attribution and stages:

- A session matches a cohort pull request when its first prompt names one
  of its closing issues (`Plan #N`, `Handle #N`, or the plan-work-unit
  command with argument N) or when any line ran on its head branch. A
  session whose prompt names exactly one cohort pull request counts every
  line toward it unless the line ran on another cohort branch; otherwise
  each line counts by its branch, and the rest prints as shared. A Codex
  rollout records one branch, so one matching more than one pull request
  prints as shared.
- A `Plan #N` or plan-work-unit session is planning. A subagent whose
  `.meta.json` description, agent type, or workflow phase contains "review",
  or that a review subagent started (`parentAgentId`), or a Codex
  session whose first user message does, is review. Other work before the
  pull request opened is implementation, after it review-fix. Work after the
  merge is excluded and its total printed.

What it cannot see:

- Hand-run wall time, and judgment-call input tokens (the ledger keeps
  output tokens only). Per-site retention prunes old ledger calls; the
  report prints the earliest retained one.
- A partial Claude invocation has no output tokens; a missing one has none
  at all. Both are counted, never read as zero tokens.
- Allowance readings cover every use of the account, hand-run work
  included. Claude Code transcripts carry no readings.
- Landing and batch sessions touch several pull requests; their other work
  prints as shared, so the hand-run baseline reads low by that much.
- The cohort is the operator's choice; the report prints each pull
  request's size so the match is visible.
"""

import argparse
import collections
import datetime as dt
import json
import math
import re
import statistics
import subprocess
import sys
from pathlib import Path

INDEX_VERSION = "freeside-inspect-index-v1"
RECORD_VERSION = "freeside-task-record-v20"
LEDGER_VERSION = "freeside.inference-budget/v4"
PLAN_MARKER = "<!-- freeside-implementation-plan:v1 -->"
TRUSTED = {"OWNER", "MEMBER", "COLLABORATOR"}
METRICS = ("input_tokens", "output_tokens", "cache_creation_input_tokens",
           "cache_read_input_tokens")
STAGE_PREFIXES = (("specify-", "specification", "specifier"),
                  ("implement-", "writer", "implementer"),
                  ("remediate-", "remediation", "remediator"),
                  ("operator-feedback-", "operator_feedback", "-"))
# Hand-run stage paired with the Freeside stage that replaces it.
PAIRS = (("planning", "specification"), ("implementation", "writer"),
         ("review", "review"), ("review_fix", "remediation"))
# Invocation statuses that mean the attempt's work was lost.
LOST = {"failed", "canceled", "gone"}
GAP_SECTIONS = {"admissions", "review_admissions", "usage", "observation",
                "publication"}
PROMPT_ISSUE = re.compile(r"\b(Plan|Handle) #(\d+)")
PLAN_COMMAND = re.compile(r"<command-name>/?plan-work-unit</command-name>\s*"
                          r"<command-args>\s*#?(\d+)\b[^<]*</command-args>")


EPOCH = dt.datetime(1970, 1, 1, tzinfo=dt.timezone.utc)


class ReportError(Exception):
    pass


def parse_time(text):
    return dt.datetime.fromisoformat(text.replace("Z", "+00:00"))


def percentile(values, fraction):
    """Nearest-rank percentile."""
    ordered = sorted(values)
    return ordered[max(0, math.ceil(fraction * len(ordered)) - 1)]


def check_version(doc, want, what):
    got = doc.get("version") if isinstance(doc, dict) else None
    if got != want:
        raise ReportError(f"{what} has version {got!r}; this script reads "
                          f"only {want}")


# --- Freeside records ------------------------------------------------------

def stage_of(stage_id):
    for prefix, stage, role in STAGE_PREFIXES:
        if stage_id.startswith(prefix):
            return stage, role
    head = stage_id.split("-", 1)[0] if stage_id else "unknown"
    return f"other:{head}", "-"


def agent_of(agent):
    if not agent:
        return "unknown", "unknown"
    model = agent.get("route_model_id") or agent.get("selection_model_id")
    return agent.get("harness_client") or "unknown", model or "unknown"


def token_metrics(tokens):
    """The invocation's token totals by metric, Codex input normalized."""
    totals = collections.Counter()
    for total in tokens:
        if total.get("unit") == "tokens":
            totals[total["metric"]] += total["quantity"]
    if "cached_input_tokens" in totals:
        cached = totals.pop("cached_input_tokens")
        totals["input_tokens"] -= cached
        totals["cache_read_input_tokens"] += cached
    return totals


def publication_times(record):
    """Run id to its publication_ready time, from anywhere in the timeline."""
    found = {}

    def walk(node):
        if isinstance(node, dict):
            if node.get("kind") == "publication_ready" and node.get("run_id"):
                found.setdefault(node["run_id"], node.get("recorded_at"))
            for value in node.values():
                walk(value)
        elif isinstance(node, list):
            for value in node:
                walk(value)
    walk(record.get("timeline"))
    return found


def read_task(record):
    """One task's token rows, coverage, gaps, and ready pull request."""
    check_version(record, RECORD_VERSION, f"task record {record.get('task_id')}")
    task_id = record["task_id"]
    ready_times = publication_times(record)
    task = {"task_id": task_id, "rows": collections.Counter(),
            "coverage": collections.Counter(), "gap_wall": 0,
            "gap_wall_unknown": 0, "runs": [], "invocations": 0,
            "errors": [], "publications": []}
    for run in record.get("runs") or []:
        run_id = run["run_id"]
        task["runs"].append(run_id)
        failed = sorted({e["section"] for e in run.get("errors") or []}
                        & GAP_SECTIONS)
        if failed:
            task["errors"].append((run_id, failed))
        writers = {a["invocation_id"]: a for a in run.get("admissions") or []}
        reviews = {a["invocation_id"]: a
                   for a in run.get("review_admissions") or []}
        statuses = {o["invocation_id"]: o["status"]
                    for o in run.get("invocations") or []}
        for usage in run.get("usage") or []:
            inv = usage["invocation_id"]
            task["invocations"] += 1
            if inv in reviews:
                stage, role = "review", reviews[inv]["role"]
                provider, model = agent_of(reviews[inv].get("agent"))
            elif inv in writers:
                stage, role = stage_of(writers[inv]["stage_id"])
                provider, model = agent_of(writers[inv].get("agent"))
            else:
                stage, role, provider, model = "other:unadmitted", "-", "unknown", "unknown"
            coverage = usage["coverage"]
            task["coverage"][coverage] += 1
            if coverage != "complete":
                if usage.get("wall_seconds") is None:
                    task["gap_wall_unknown"] += 1
                else:
                    task["gap_wall"] += usage["wall_seconds"]
            status = statuses.get(inv, "unknown")
            for metric, quantity in token_metrics(usage.get("tokens") or []).items():
                task["rows"][(role, stage, provider, model, status, metric)] += quantity
        publication = run.get("publication")
        if publication:
            outcome = publication["outcome"]
            task["publications"].append(
                (ready_times.get(run_id) or "", outcome["repo"],
                 outcome["pr_number"], run_id))
    latest = max(task["publications"], default=None,
                 key=lambda p: (parse_time(p[0]) if p[0] else EPOCH, p[1:]))
    task["pr"] = latest[1:3] if latest else None
    return task


def add_judgment(tasks, ledger):
    """Fold judgment calls into their run's task; return what stays apart."""
    check_version(ledger, LEDGER_VERSION, "inference ledger")
    by_run = {run: task for task in tasks for run in task["runs"]}
    out = {"unattributed": collections.Counter(), "missing": 0,
           "no_identity": 0, "earliest": None, "calls": 0}
    for call in ledger.get("calls") or []:
        out["calls"] += 1
        called = call.get("called_at")
        if called and (out["earliest"] is None or called < out["earliest"]):
            out["earliest"] = called
        identity = call.get("identity")
        if identity is None:
            # Written by a v2 ledger, before calls recorded what they ran.
            out["no_identity"] += 1
            continue
        observed = identity.get("observed")
        task = by_run.get(call.get("root_lineage"))
        if observed is None:
            out["missing"] += 1
            if task:
                task.setdefault("judgment_missing", 0)
                task["judgment_missing"] += 1
            continue
        tokens = observed.get("output_tokens", 0)
        if task is None:
            out["unattributed"][call["site"]] += tokens
            continue
        admission = identity.get("admission") or {}
        enrollment = admission.get("enrollment_id") or ""
        provider = enrollment.split("/", 1)[1] if "/" in enrollment else "unknown"
        model = observed.get("model_id") or identity.get("requested_model_id") or "unknown"
        role = admission.get("role") or call["site"]
        task["rows"][(role, "judgment", provider, model, "-", "output_tokens")] += tokens
    return out


def ready_prs(tasks):
    """Ready pull request (repo, number) to the tasks that produced it."""
    prs = collections.defaultdict(list)
    for task in tasks:
        if task["pr"]:
            prs[task["pr"]].append(task)
    return prs


# --- allowance -------------------------------------------------------------

def readings(reports):
    out = []
    for report in reports:
        observation = report.get("observation") or {}
        for window in observation.get("windows") or []:
            out.append({"pool": report["pool"], "outcome": report["outcome"],
                        "window": window["window"],
                        "enrollment": window.get("enrollment_id", ""),
                        "status": window["status"],
                        "used_percent": window.get("used_percent"),
                        "quantity": window.get("quantity"),
                        "unit": window.get("unit"),
                        "resets_at": window.get("resets_at"),
                        "observed_at": window["observed_at"]})
    return out


def reading_changes(rows, ready):
    """Change in used share between successive readings of one window.

    ready is (time, repo, number) per publication; a pull request published
    again in the period counts once. A pair whose window reset between the
    readings prints no change, since the share fell back to zero in between.
    """
    groups = collections.defaultdict(list)
    for row in rows:
        groups[(row["pool"], row["window"], row["enrollment"])].append(row)
    changes = []
    for key, group in sorted(groups.items()):
        group = sorted({r["observed_at"]: r for r in group}.values(),
                       key=lambda r: parse_time(r["observed_at"]))
        for before, after in zip(group, group[1:]):
            if before["used_percent"] is None or after["used_percent"] is None:
                continue
            start, end = parse_time(before["observed_at"]), parse_time(after["observed_at"])
            landed = len({(repo, number) for t, repo, number in ready
                          if start < parse_time(t) <= end})
            reset = before["resets_at"] is not None and (
                after["resets_at"] != before["resets_at"]
                or parse_time(before["resets_at"]) <= end)
            delta = None if reset else after["used_percent"] - before["used_percent"]
            changes.append((key, before["observed_at"], after["observed_at"], delta, landed))
    return changes


# --- transcripts -----------------------------------------------------------

def prompt_issues(text):
    """(verb, issue) pairs a first prompt names."""
    found = {(verb.lower(), int(n)) for verb, n in PROMPT_ISSUE.findall(text)}
    found |= {("plan", int(n)) for n in PLAN_COMMAND.findall(text)}
    return found


def user_text(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        texts = [b.get("text", "") for b in content
                 if isinstance(b, dict) and b.get("type") in ("text", "input_text")]
        return "\n".join(texts) if texts else None
    return None


def read_claude(path):
    """One Claude Code transcript file as a session, without message text."""
    entries, messages, first, cwds = [], {}, None, set()
    with open(path, encoding="utf-8", errors="replace") as handle:
        for line in handle:
            kind = ('"assistant"' in line and "assistant") or \
                   (first is None and '"user"' in line and "user")
            if not kind:
                continue
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                continue
            if entry.get("cwd"):
                cwds.add(entry["cwd"])
            message = entry.get("message") or {}
            if entry.get("type") == "user" and first is None:
                if entry.get("isMeta") or entry.get("toolUseResult") is not None:
                    continue
                text = user_text(message.get("content"))
                if text is not None:
                    first = text
            elif entry.get("type") == "assistant" and message.get("usage"):
                key = message.get("id") or entry.get("uuid")
                messages[key] = (entry.get("timestamp"), entry.get("gitBranch"),
                                 message.get("model") or "unknown",
                                 {m: message["usage"].get(m) or 0 for m in METRICS})
    for timestamp, branch, model, usage in messages.values():
        if timestamp:
            entries.append({"time": parse_time(timestamp), "branch": branch,
                            "model": model, "usage": usage})
    return {"entries": entries, "prompt": prompt_issues(first or ""), "cwds": cwds}


def claude_sessions(dirs, keep):
    """Sessions grouped by parent: each has its own lines and subagents."""
    sessions = {}
    for directory in dirs:
        for path in sorted(Path(directory).rglob("*.jsonl")):
            if not keep(path):
                continue
            if "subagents" in path.parent.parts:
                # <session>/subagents/[workflows/<wf>/]agent-<id>.jsonl
                parts = path.parent.parts
                parent = parts[len(parts) - 1 - parts[::-1].index("subagents") - 1]
                data = read_claude(path)
                meta_path = path.with_suffix(".meta.json")
                meta = json.loads(meta_path.read_text()) if meta_path.is_file() else {}
                data["label"] = " ".join(str(meta.get(k, "")) for k in
                                         ("description", "agentType", "workflowPhase"))
                data["parent"] = meta.get("parentAgentId")
                group = sessions.setdefault(parent, {"main": None, "subs": {}})
                group["subs"][path.stem.removeprefix("agent-")] = data
            else:
                sessions.setdefault(path.stem, {"main": None, "subs": {}})["main"] = read_claude(path)
    out = []
    for session_id, group in sorted(sessions.items()):
        main = group["main"] or {"entries": [], "prompt": set(), "cwds": set()}
        prompt = main["prompt"]
        subs = group["subs"]

        def is_review(agent_id, seen=()):
            sub = subs.get(agent_id)
            if sub is None or agent_id in seen:
                return False
            return "review" in sub["label"].lower() or is_review(sub["parent"],
                                                                 (*seen, agent_id))
        parts = [(e, False) for e in main["entries"]]
        parts += [(e, is_review(agent_id)) for agent_id, sub in subs.items()
                  for e in sub["entries"]]
        cwds = main["cwds"].union(*(s["cwds"] for s in subs.values()))
        out.append({"id": session_id, "harness": "claude_code", "prompt": prompt,
                    "cwds": cwds, "lines": parts})
    return out


def codex_usage(total):
    cached = total.get("cached_input_tokens", 0)
    return {"input_tokens": total.get("input_tokens", 0) - cached,
            "output_tokens": total.get("output_tokens", 0),
            "cache_creation_input_tokens": total.get("cache_write_input_tokens", 0),
            "cache_read_input_tokens": cached}


def read_codex(path):
    meta, model, first, totals, limits = {}, "unknown", None, [], None
    with open(path, encoding="utf-8", errors="replace") as handle:
        for line in handle:
            if not any(k in line for k in ('"session_meta"', '"turn_context"',
                                           '"token_count"', '"message"')):
                continue
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                continue
            kind, payload = entry.get("type"), entry.get("payload") or {}
            if kind == "session_meta":
                meta = payload
            elif kind == "turn_context":
                model = payload.get("model") or model
            elif kind == "event_msg" and payload.get("type") == "token_count":
                info = payload.get("info") or {}
                if info.get("total_token_usage") and entry.get("timestamp"):
                    totals.append((parse_time(entry["timestamp"]),
                                   codex_usage(info["total_token_usage"])))
                if payload.get("rate_limits"):
                    limits = payload["rate_limits"]
            elif (first is None and kind == "response_item"
                  and payload.get("type") == "message" and payload.get("role") == "user"):
                text = user_text(payload.get("content")) or ""
                # Skip the harness's injected context and instructions.
                if text and not text.lstrip().startswith(("<", "# AGENTS.md")):
                    first = text
    return {"id": path.stem, "harness": "codex_cli", "model": model,
            "branch": (meta.get("git") or {}).get("branch"),
            "cwds": {meta["cwd"]} if meta.get("cwd") else set(),
            "prompt": prompt_issues(first or ""),
            "review": "review" in (first or "").lower(),
            "totals": totals, "limits": limits}


def codex_at(totals, moment):
    """The last cumulative total at or before moment."""
    best = {m: 0 for m in METRICS}
    for time, usage in totals:
        if moment is None or time <= moment:
            best = usage
    return best


# --- hand-run attribution --------------------------------------------------

def cohort_maps(cohort):
    by_issue, by_branch = {}, {}
    for pr in cohort:
        by_branch[pr["head"]] = pr["number"]
        for issue in pr["issues"]:
            by_issue[issue] = pr["number"]
    return by_issue, by_branch


def hand_stage(pr, planning, review, moment):
    if planning:
        return "planning"
    if review:
        return "review"
    return "implementation" if moment < pr["created"] else "review_fix"


def attribute(claude, codex, cohort):
    """Per cohort pull request: Counter of (stage, harness, model, metric)."""
    by_issue, by_branch = cohort_maps(cohort)
    prs = {pr["number"]: pr for pr in cohort}
    rows = {pr["number"]: collections.Counter() for pr in cohort}
    out = {"rows": rows, "shared": collections.Counter(),
           "after_merge": collections.Counter(), "matched": collections.Counter(),
           "unmatched": 0, "limits": {}}
    for session in claude:
        prompt_prs = {by_issue[n] for _, n in session["prompt"] if n in by_issue}
        planned = {by_issue[n] for verb, n in session["prompt"]
                   if verb == "plan" and n in by_issue}
        branch_prs = {by_branch[e["branch"]] for e, _ in session["lines"]
                      if e["branch"] in by_branch}
        if not prompt_prs | branch_prs:
            out["unmatched"] += 1
            continue
        out["matched"][session["harness"]] += 1
        sole = next(iter(prompt_prs)) if len(prompt_prs) == 1 else None
        for entry, review in session["lines"]:
            number = by_branch.get(entry["branch"], sole)
            if number is None:
                for metric, value in entry["usage"].items():
                    out["shared"][metric] += value
                continue
            pr = prs[number]
            if pr["merged"] and entry["time"] > pr["merged"]:
                for metric, value in entry["usage"].items():
                    out["after_merge"][metric] += value
                continue
            stage = hand_stage(pr, number in planned, review, entry["time"])
            for metric, value in entry["usage"].items():
                rows[number][(stage, "claude_code", entry["model"], metric)] += value
    for session in codex:
        matched = {by_issue[n] for _, n in session["prompt"] if n in by_issue}
        if session["branch"] in by_branch:
            matched.add(by_branch[session["branch"]])
        if not matched:
            out["unmatched"] += 1
            continue
        out["matched"]["codex_cli"] += 1
        if len(matched) > 1:
            for metric, value in codex_at(session["totals"], None).items():
                out["shared"][metric] += value
            continue
        pr = prs[matched.pop()]
        if session["limits"]:
            out["limits"][session["id"]] = (pr["number"], session["limits"])
        merged = pr["merged"]
        final = codex_at(session["totals"], None)
        upto_merge = codex_at(session["totals"], merged)
        for metric in METRICS:
            out["after_merge"][metric] += final[metric] - upto_merge[metric]
        planning = any(v == "plan" and by_issue.get(n) == pr["number"]
                       for v, n in session["prompt"])
        if planning or session["review"]:
            parts = {hand_stage(pr, planning, session["review"], None): upto_merge}
        else:
            before = codex_at(session["totals"], pr["created"])
            parts = {"implementation": before,
                     "review_fix": {m: upto_merge[m] - before[m] for m in METRICS}}
        for stage, usage in parts.items():
            for metric, value in usage.items():
                rows[pr["number"]][(stage, "codex_cli", session["model"], metric)] += value
    return out


# --- forge -----------------------------------------------------------------

def run_command(argv):
    result = subprocess.run(argv, capture_output=True, text=True)
    if result.returncode != 0:
        raise ReportError(f"{' '.join(argv[:4])} failed (exit "
                          f"{result.returncode}): {result.stderr.strip()}")
    return result.stdout


def pr_facts(repo, number):
    data = json.loads(run_command(
        ["gh", "pr", "view", str(number), "--repo", repo, "--json",
         "number,headRefName,createdAt,mergedAt,additions,deletions,"
         "closingIssuesReferences"]))
    return {"number": data["number"], "head": data["headRefName"],
            "created": parse_time(data["createdAt"]),
            "merged": parse_time(data["mergedAt"]) if data.get("mergedAt") else None,
            "size": data["additions"] + data["deletions"],
            "issues": [i["number"] for i in data.get("closingIssuesReferences") or []]}


def issue_planned(repo, issue):
    pages = json.loads(run_command(
        ["gh", "api", "--paginate", "--slurp",
         f"repos/{repo}/issues/{issue}/comments"]))
    return any(c.get("author_association") in TRUSTED
               and (c.get("body") or "").startswith(PLAN_MARKER)
               for page in pages for c in page)


# --- rendering -------------------------------------------------------------

def total(counter):
    return sum(counter.values())


def fmt(value):
    return f"{round(value):,}"


def table(headers, body):
    lines = ["| " + " | ".join(headers) + " |",
             "| " + " | ".join("---" for _ in headers) + " |"]
    lines += ["| " + " | ".join(str(c) for c in row) + " |" for row in body]
    return "\n".join(lines) + "\n"


def breakdown(rows):
    return [(*key, fmt(value)) for key, value in sorted(rows.items()) if value]


def median(values):
    return statistics.median(values) if values else 0


def pr_rows(group):
    rows = collections.Counter()
    for task in group:
        rows.update(task["rows"])
    return rows


def freeside_split(rows, planned):
    """Per-PR totals: (all, output) by comparison row, Freeside side.

    Lost invocations and stages the hand-run flow has no counterpart for go
    to their own rows; a specification on an already-planned issue counts
    both in the paired specification row and on its own as extra spend.
    """
    out = collections.defaultdict(lambda: [0, 0])
    for (_role, stage, _provider, _model, status, metric), value in rows.items():
        name = "failed or lost attempts" if status in LOST else stage
        if name == "specification" and planned:
            out["specification on a planned issue"][0] += value
            out["specification on a planned issue"][1] += value if metric == "output_tokens" else 0
        out[name][0] += value
        out[name][1] += value if metric == "output_tokens" else 0
    return out


def hand_split(rows):
    out = collections.defaultdict(lambda: [0, 0])
    for (stage, _harness, _model, metric), value in rows.items():
        out[stage][0] += value
        out[stage][1] += value if metric == "output_tokens" else 0
    return out


def render_gaps(tasks, prs, judgment, hand):
    coverage = collections.Counter()
    for task in tasks:
        coverage.update(task["coverage"])
    errors = [(t["task_id"], run, sections) for t in tasks for run, sections in t["errors"]]
    multi = [t["task_id"] for t in tasks if len({p[1:3] for p in t["publications"]}) > 1]
    gaps = [
        ("Tasks read", len(tasks)),
        ("Ready pull requests", len(prs)),
        ("Tasks with no ready pull request", sum(1 for t in tasks if not t["pr"])),
        ("Tasks that published more than one pull request (counted toward the latest)",
         len(multi)),
        ("Invocations complete / partial / missing",
         f"{coverage['complete']} / {coverage['partial']} / {coverage['missing']}"),
        ("Wall seconds of partial and missing invocations",
         f"{fmt(sum(t['gap_wall'] for t in tasks))} (plus "
         f"{sum(t['gap_wall_unknown'] for t in tasks)} with no wall time)"),
        ("Runs with unread sections", len(errors)),
    ]
    if judgment:
        gaps += [("Judgment calls retained", judgment["calls"]),
                 ("Earliest retained judgment call", judgment["earliest"] or "none"),
                 ("Judgment calls with no observed tokens", judgment["missing"]),
                 ("Judgment calls from before identities were recorded",
                  judgment["no_identity"]),
                 ("Unattributed judgment output tokens",
                  fmt(total(judgment["unattributed"])))]
    gaps += [("Hand-run sessions matched (by harness)",
              ", ".join(f"{k} {v}" for k, v in sorted(hand["matched"].items())) or "0"),
             ("Sessions read but matching no cohort pull request", hand["unmatched"]),
             ("Hand-run tokens shared by several cohort pull requests",
              fmt(total(hand["shared"]))),
             ("Hand-run tokens after merge (excluded)", fmt(total(hand["after_merge"])))]
    out = ["## Inputs and Gaps\n\n", table(("Input", "Value"), gaps)]
    for task_id, run, sections in errors:
        out.append(f"\n- Task `{task_id}` run `{run}`: unread {', '.join(sections)}; "
                   "its tokens are unknown, not zero.")
    return "".join(out) + "\n"


def render_comparison(free, hand_rows):
    free_split = [freeside_split(rows, planned) for rows, planned in free]
    hand_split_rows = [hand_split(rows) for rows in hand_rows]

    def med(splits, name, index):
        return fmt(median([s[name][index] if name in s else 0 for s in splits]))
    body = [(f"{h} / {f}", med(hand_split_rows, h, 0), med(free_split, f, 0),
             med(hand_split_rows, h, 1), med(free_split, f, 1)) for h, f in PAIRS]
    paired = {f for _, f in PAIRS}
    extra = sorted({name for s in free_split for name in s} - paired)
    body += [(f"nothing / {name}", "0", med(free_split, name, 0), "0",
              med(free_split, name, 1)) for name in extra]
    return ("## Stage Comparison\n\nPer-PR median tokens over all metrics, then output "
            "tokens alone. Lost invocations count only in the failed or lost row. "
            f"Sample: {len(free)} Freeside and {len(hand_rows)} hand-run pull requests; "
            "the report does not extrapolate.\n\n"
            + table(("Hand-run / Freeside", "Hand-run", "Freeside", "Hand-run output",
                     "Freeside output"), body))


def render_cap(free_rows):
    out = ["## Queue-Cap Inputs (#3417)\n\nTokens per ready pull request by provider "
           "and model, all metrics; a pull request that didn't use a model counts as "
           "0.\n\n"]
    if not free_rows:
        return out[0] + "No ready pull requests.\n"
    body = []
    for provider, model in sorted({(k[2], k[3]) for r in free_rows for k in r}):
        values = [sum(v for k, v in r.items() if (k[2], k[3]) == (provider, model))
                  for r in free_rows]
        body.append((provider, model, fmt(median(values)), fmt(percentile(values, 0.9))))
    values = [total(r) for r in free_rows]
    body.append(("all", "all", fmt(median(values)), fmt(percentile(values, 0.9))))
    return out[0] + table(("Provider", "Model", "Median", "90th percentile"), body)


def render_allowance(allowance, changes, limits):
    out = ["## Allowance Readings\n\nAs the provider reported them; none is derived "
           "from tokens, and every reading includes any other use of the account.\n\n"]
    if allowance:
        out.append(table(
            ("Pool", "Outcome", "Window", "Enrollment", "Status", "Used %", "Quantity",
             "Unit", "Resets at", "Observed at"),
            [(r["pool"], r["outcome"], r["window"], r["enrollment"], r["status"],
              *("null" if r[k] is None else r[k]
                for k in ("used_percent", "quantity", "unit", "resets_at")),
              r["observed_at"]) for r in allowance]))
    else:
        out.append("No saved readings were given.\n")
    if changes:
        out.append("\nChange in used share between two readings of one window, with the "
                   "ready pull requests published in that period (any other use of the "
                   "account included):\n\n")
        out.append(table(("Pool", "Window", "Enrollment", "From", "To", "Change (points)",
                          "Ready PRs"),
                         [(*key, start, end,
                           "window reset" if delta is None else f"{delta:+d}", landed)
                          for key, start, end, delta, landed in changes]))
    if limits:
        out.append("\nLast Codex rate-limit reading per attributed hand-run session:\n\n")
        body = [(f"#{number}", name, window.get("used_percent"),
                 window.get("window_minutes"), window.get("resets_at"))
                for _, (number, reading) in sorted(limits.items())
                for name in ("primary", "secondary")
                for window in [reading.get(name) or {}] if window]
        out.append(table(("Pull request", "Window", "Used %", "Window minutes",
                          "Resets at"), body))
    out.append("\nClaude Code transcripts carry no allowance readings.\n")
    return "".join(out)


def render(tasks, judgment, allowance, changes, hand, cohort, facts, planned, repo):
    """facts maps (repo, number) to forge facts; planned (repo, issue) to bool;
    repo is the cohort's repository."""
    prs = ready_prs(tasks)
    out = ["# Tokens per Ready Pull Request\n\n", render_gaps(tasks, prs, judgment, hand)]
    out.append("\n## Freeside Ready Pull Requests\n")
    free = []
    for key, group in sorted(prs.items()):
        rows = pr_rows(group)
        pr = facts[key]
        is_planned = any(planned.get((key[0], i)) for i in pr["issues"])
        free.append((rows, is_planned))
        out.append(f"\n### {key[0]}#{key[1]}\n\nSize {pr['size']} lines; "
                   f"{'current plan comment' if is_planned else 'no current plan comment'}; {len(group)} task(s), "
                   f"{sum(len(t['runs']) for t in group)} run(s), "
                   f"{sum(t['invocations'] for t in group)} invocation(s); "
                   f"{fmt(total(rows))} tokens.\n\n")
        out.append(table(("Role", "Stage", "Provider", "Model", "Status", "Metric",
                          "Tokens"), breakdown(rows)))
    out.append("\n## Tasks With No Ready Pull Request\n\n")
    orphans = [(f"`{t['task_id']}`", len(t["runs"]), t["invocations"], fmt(total(t["rows"])))
               for t in tasks if not t["pr"]]
    out.append(table(("Task", "Runs", "Invocations", "Tokens"), orphans)
               if orphans else "None.\n")
    if judgment:
        out.append("\n## Judgment Calls\n\nOutput tokens only. A call attributed to a "
                   "run is in that task's rows under stage `judgment`.\n\n")
        body = [(site, fmt(v)) for site, v in sorted(judgment["unattributed"].items())]
        out.append(table(("Unattributed site", "Output tokens"), body)
                   if body else "No unattributed calls.\n")
    out.append("\n## Hand-Run Pull Requests\n")
    for pr in cohort:
        rows = hand["rows"][pr["number"]]
        is_planned = any(planned.get((repo, i)) for i in pr["issues"])
        out.append(f"\n### #{pr['number']}\n\nSize {pr['size']} lines; "
                   f"{'current plan comment' if is_planned else 'no current plan comment'}; "
                   f"{fmt(total(rows))} tokens.\n\n")
        out.append(table(("Stage", "Provider", "Model", "Metric", "Tokens"),
                         breakdown(rows)))
    out.append("\n" + render_comparison(free, [hand["rows"][pr["number"]] for pr in cohort]))
    out.append("\n" + render_cap([rows for rows, _ in free]))
    out.append("\n" + render_allowance(allowance, changes, hand["limits"]))
    return "".join(out)


# --- I/O and command line --------------------------------------------------

def inspect(freesided, *args):
    return json.loads(run_command([freesided, "inspect", *args]))


def read_json_reports(path):
    text = Path(path).read_text()
    try:
        return [json.loads(text)]
    except json.JSONDecodeError:
        return [json.loads(line) for line in text.splitlines() if line.strip()]


def day(text):
    return dt.datetime.strptime(text, "%Y-%m-%d").replace(tzinfo=dt.timezone.utc)


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--db", required=True)
    parser.add_argument("--freesided", default="freesided")
    parser.add_argument("--since", type=day)
    parser.add_argument("--until", type=day)
    parser.add_argument("--inference-ledger")
    parser.add_argument("--allowance", action="append", default=[])
    parser.add_argument("--repo", default="freeside-ai/freeside")
    parser.add_argument("--hand-run-pr", type=int, action="append", default=[])
    parser.add_argument("--claude-transcripts", action="append", default=[])
    parser.add_argument("--codex-transcripts", action="append", default=[])
    parser.add_argument("--cwd-contains", action="append", default=[])
    args = parser.parse_args(argv)
    until = args.until + dt.timedelta(days=1) if args.until else None

    def in_window(moment):
        return ((args.since is None or moment >= args.since)
                and (until is None or moment < until))
    try:
        index = inspect(args.freesided, "-db", args.db)
        check_version(index, INDEX_VERSION, "inspect index")
        tasks = [read_task(inspect(args.freesided, "task", "-db", args.db,
                                   "-task", entry["task_id"]))
                 for entry in index.get("tasks") or []
                 if in_window(parse_time(entry["created_at"]))]
        judgment = (add_judgment(tasks, json.loads(Path(args.inference_ledger).read_text()))
                    if args.inference_ledger else None)
        allowance = readings(r for path in args.allowance for r in read_json_reports(path))
        ready = [p[:3] for t in tasks for p in t["publications"] if p[0]]
        changes = reading_changes(allowance, ready)
        facts = {key: pr_facts(*key) for key in ready_prs(tasks)}
        cohort = [pr_facts(args.repo, n) for n in args.hand_run_pr]
        issues = {(key[0], i) for key, pr in facts.items() for i in pr["issues"]}
        issues |= {(args.repo, i) for pr in cohort for i in pr["issues"]}
        planned = {(repo, i): issue_planned(repo, i) for repo, i in sorted(issues)}

        def keep(path):
            moment = dt.datetime.fromtimestamp(path.stat().st_mtime, dt.timezone.utc)
            return args.since is None or moment >= args.since

        def cwd_ok(session):
            return not args.cwd_contains or any(
                text in cwd for cwd in session["cwds"] for text in args.cwd_contains)
        claude = [s for s in claude_sessions(args.claude_transcripts, keep) if cwd_ok(s)]
        codex = [s for d in args.codex_transcripts
                 for s in (read_codex(p) for p in sorted(Path(d).rglob("rollout-*.jsonl"))
                           if keep(p)) if cwd_ok(s)]
        hand = attribute(claude, codex, cohort)
    except ReportError as err:
        print(f"token-report: {err}", file=sys.stderr)
        return 1
    sys.stdout.write(render(tasks, judgment, allowance, changes, hand, cohort,
                            facts, planned, args.repo))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
