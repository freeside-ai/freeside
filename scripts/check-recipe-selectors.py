#!/usr/bin/env python3
"""Check that the verification recipe's path rules match CI's selectors.

Freeside's recipe (.freeside/verify.json) skips a command when no changed
path matches its "paths" (daemon/README.md, A command can name the paths
that make it run). Each command's paths copy the select_component call in
.github/workflows/pull-request.yml that runs the same check, so a change CI
would check is a change verification checks. This script fails when the two
disagree.

Usage: check-recipe-selectors.py [--recipe <file>] [--workflow <file>]
Defaults are the repository's own files, read from the working directory.

Rules:
- A command maps to a selector by its `scripts/check.sh <component> [step...]`
  tokens. daemon, app, api, docs, and convergence map to the selector of the
  same name. scripts maps by step: vocabulary to the vocabulary selector;
  tier-a-paths, credential-literals, and recipe-selectors, which CI runs on
  every pull request, to no selector; any other step to the scripts
  selector. A scripts command with no steps, or with steps that map to more
  than one of these, is refused.
- A command mapped to a selector carries "paths" equal, as a set, to that
  selector's entries.
- A command mapped to no selector, or that names no check.sh component at
  all, carries no "paths": it always runs.
- "run_all_on" holds .github/workflows/, which every selector matches
  (changed_under), and scripts/check.sh.
- Every key is spelled exactly as the daemon's recipe format names it. The
  daemon matches keys ignoring case, so a "Paths" this script didn't read
  would still skip its command.

Exit codes: 0 when they agree, 1 on a disagreement, 2 on a usage error or a
file it can't read.
"""

import argparse
import json
import re
import shlex
import sys

COMPONENT_SELECTORS = {
    "daemon": "daemon",
    "app": "app",
    "api": "api",
    "docs": "docs",
    "convergence": "convergence",
}
ALWAYS_RUN_SCRIPTS_STEPS = {"tier-a-paths", "credential-literals", "recipe-selectors"}
REQUIRED_RUN_ALL_ON = (".github/workflows/", "scripts/check.sh")
RECIPE_KEYS = {"commands", "capture", "run_all_on"}
COMMAND_KEYS = {"argv", "room", "history", "paths"}


def selectors(workflow_text):
    """Return {name: [entries]} from the select_component calls."""
    # Join backslash continuations so each call is one line.
    text = re.sub(r"\\\n", " ", workflow_text)
    found = {}
    for line in text.splitlines():
        # Split only the calls: other shell lines need not be shlex-safe.
        if not line.lstrip().startswith("select_component "):
            continue
        words = shlex.split(line, comments=True)
        if len(words) >= 2:
            if words[1] in found:
                raise ValueError(f"selector {words[1]} is defined twice")
            found[words[1]] = words[2:]
    return found


def selector_for(argv):
    """Return (selector name or None, refusal or None) for one command."""
    if "scripts/check.sh" not in argv:
        return None, None
    rest = argv[argv.index("scripts/check.sh") + 1 :]
    if not rest:
        return None, "names scripts/check.sh with no component"
    component, steps = rest[0], rest[1:]
    if component in COMPONENT_SELECTORS:
        return COMPONENT_SELECTORS[component], None
    if component != "scripts":
        return None, f"names unknown check.sh component {component!r}"
    if not steps:
        return None, "runs every scripts step, which CI runs on different triggers"
    kinds = set()
    for step in steps:
        if step in ALWAYS_RUN_SCRIPTS_STEPS:
            kinds.add(None)
        elif step == "vocabulary":
            kinds.add("vocabulary")
        else:
            kinds.add("scripts")
    if len(kinds) > 1:
        return None, "mixes scripts steps CI runs on different triggers"
    return kinds.pop(), None


def command_argv(command):
    return command if isinstance(command, list) else command.get("argv", [])


def disagreements(recipe, found):
    problems = [f"recipe carries key {key!r}" for key in sorted(set(recipe) - RECIPE_KEYS)]
    for index, command in enumerate(recipe.get("commands", [])):
        argv = command_argv(command)
        label = f"command {index} ({' '.join(argv)})"
        if isinstance(command, dict):
            problems.extend(f"{label} carries key {key!r}" for key in sorted(set(command) - COMMAND_KEYS))
        paths = None if isinstance(command, list) else command.get("paths")
        name, refusal = selector_for(argv)
        if refusal:
            problems.append(f"{label} {refusal}")
            continue
        if name is None:
            if paths is not None:
                problems.append(f"{label} maps to no CI selector, so it must carry no paths")
            continue
        if name not in found:
            problems.append(f"{label} maps to selector {name!r}, which pull-request.yml does not define")
            continue
        if paths is None or set(paths) != set(found[name]):
            problems.append(
                f"{label} paths {sorted(paths or [])} differ from selector {name!r} {sorted(found[name])}"
            )
    run_all_on = recipe.get("run_all_on") or []
    for entry in REQUIRED_RUN_ALL_ON:
        if entry not in run_all_on:
            problems.append(f"run_all_on lacks {entry}")
    return problems


def main(argv):
    parser = argparse.ArgumentParser(description="Check the recipe's path rules against CI's selectors.")
    parser.add_argument("--recipe", default=".freeside/verify.json")
    parser.add_argument("--workflow", default=".github/workflows/pull-request.yml")
    args = parser.parse_args(argv)
    try:
        with open(args.recipe, encoding="utf-8") as f:
            recipe = json.load(f)
        with open(args.workflow, encoding="utf-8") as f:
            found = selectors(f.read())
    except (OSError, ValueError) as err:
        print(f"check-recipe-selectors: {err}", file=sys.stderr)
        return 2
    if not isinstance(recipe, dict) or not found:
        print("check-recipe-selectors: no recipe object or no select_component calls", file=sys.stderr)
        return 2
    problems = disagreements(recipe, found)
    for problem in problems:
        print(f"check-recipe-selectors: {problem}", file=sys.stderr)
    if problems:
        return 1
    print(f"recipe paths agree with {len(found)} CI selectors")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
