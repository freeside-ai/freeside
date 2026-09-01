#!/usr/bin/env bash
# test-link-plan-section-refs.sh — regression suite for
# link-plan-section-refs.sh.
#
# Runs the linker over synthetic Markdown fixtures in a temp directory
# (no network, no git) and checks the whole parser contract in one
# place: the citation grammar (singular, plural, list, range, wrapped
# across a line break, repeated, possessive) and its number boundaries;
# the contexts a citation must survive untouched (fenced code with
# matching delimiters, headings, inline code spans, and the label of an
# existing link or image, which a nested link would corrupt);
# idempotency, including a list whose first number is already linked;
# and the broken-reference exits, which cover both a citation with no
# heading and a link whose destination no longer matches its heading's
# anchor.
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
LINKER=$SCRIPT_DIR/link-plan-section-refs.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
CASE=''
FIXTURE=''
OUT=''
RC=0

begin_case() {
  CASE=$1
  echo "case: $CASE"
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

ok() { pass=$((pass + 1)); }

# The numbered headings every fixture shares. Their anchors are
# 1-purpose, 5-architecture, 513-review, 6-roadmap, and 7-risks.
headings() {
  cat <<'MD'
# Plan

## 1. Purpose

## 5. Architecture

### 5.13 Review

## 6. Roadmap

## 7. Risks

MD
}

# fixture <name>: heading preamble plus the Markdown read from stdin.
fixture() {
  FIXTURE=$TMP/$1.md
  headings > "$FIXTURE"
  cat >> "$FIXTURE"
  cp "$FIXTURE" "$FIXTURE.orig"
}

run_linker() {
  set +e
  OUT=$(bash "$LINKER" "$@" 2>&1)
  RC=$?
  set -e
}

assert_rc() {
  if [ "$RC" -eq "$1" ]; then ok; else report_failure "expected rc=$1, got rc=$RC"; fi
}

assert_out_contains() {
  if printf '%s' "$OUT" | grep -Fq -- "$1"; then ok; else report_failure "output lacks: $1"; fi
}

assert_contains() {
  if grep -Fq -- "$1" "$FIXTURE"; then ok; else report_failure "file lacks: $1"; fi
}

assert_lacks() {
  if grep -Fq -- "$1" "$FIXTURE"; then report_failure "file still has: $1"; else ok; fi
}

assert_unchanged() {
  if cmp -s "$FIXTURE" "$FIXTURE.orig"; then ok; else report_failure "file was rewritten"; fi
}

# ------------------------------------------------- citation grammar
begin_case "1 every citation shape links its number only"
fixture grammar <<'MD'
See Section 5 and Section 5.13 and Section 5 again.
Also Sections 5 and 6, Sections 5, 6, and 7, and Sections 5-6.
Shorthand §5 and §5.13, plus Section 5's own wording.

- Section 6 inside an indented list item.

A citation may wrap: see Section
5.13 for the detail.
MD
run_linker "$FIXTURE"
assert_rc 0
assert_contains 'See Section [5](#5-architecture) and Section [5.13](#513-review) and Section [5](#5-architecture) again.'
assert_contains 'Sections [5](#5-architecture) and [6](#6-roadmap)'
assert_contains 'Sections [5](#5-architecture), [6](#6-roadmap), and [7](#7-risks)'
assert_contains 'Sections [5](#5-architecture)-[6](#6-roadmap)'
assert_contains '[§5](#5-architecture) and [§5.13](#513-review)'
assert_contains "Section [5](#5-architecture)'s own wording"
assert_contains '- Section [6](#6-roadmap) inside an indented list item.'
assert_contains '[5.13](#513-review) for the detail.'

begin_case "2 a number that is not a section number is left alone"
fixture boundaries <<'MD'
Milestone Section 1B.1 and Section 5.13.2 and Sectional 5 stay as text.
MD
run_linker "$FIXTURE"
assert_rc 0
assert_out_contains '0 new link(s)'
assert_unchanged

# ------------------------------------------------------- code fences
begin_case "3 a fenced block is skipped and counted"
fixture fence <<'MD'
```
Section 5 in code
```

Section 6 in prose after the fence.
MD
run_linker "$FIXTURE"
assert_rc 0
assert_out_contains 'skipped 1 citation(s) in code blocks'
assert_contains 'Section 5 in code'
assert_contains 'Section [6](#6-roadmap) in prose after the fence.'

begin_case "4 a longer fence is not closed by a shorter one"
fixture longfence <<'MD'
````text
```
Section 5 inside the quoted fence
```
````

Section 6 after the real close.
MD
run_linker "$FIXTURE"
assert_rc 0
assert_contains 'Section 5 inside the quoted fence'
assert_contains 'Section [6](#6-roadmap) after the real close.'

begin_case "5 a tilde fence is not closed by backticks"
fixture mixedfence <<'MD'
~~~
Section 5 in a tilde fence
```
Section 6 still inside it
~~~

Section 7 in prose.
MD
run_linker "$FIXTURE"
assert_rc 0
assert_contains 'Section 5 in a tilde fence'
assert_contains 'Section 6 still inside it'
assert_contains 'Section [7](#7-risks) in prose.'

begin_case "6 an indented fence still opens and closes"
fixture indentfence <<'MD'
- An example:

   ```
   Section 5 in an indented block
   ```

- Section 6 after it.
MD
run_linker "$FIXTURE"
assert_rc 0
assert_contains 'Section 5 in an indented block'
assert_contains '- Section [6](#6-roadmap) after it.'

# ------------------------------------------- headings and inline spans
begin_case "7 a citation in a heading is skipped and counted"
fixture heading <<'MD'
#### Notes on Section 5

Section 6 in prose.
MD
run_linker "$FIXTURE"
assert_rc 0
assert_out_contains '1 in headings'
assert_contains '#### Notes on Section 5'
assert_contains 'Section [6](#6-roadmap) in prose.'

begin_case "8 a citation in an inline code span is left alone"
fixture codespan <<'MD'
Write `Section 5` literally, then cite Section 6.
MD
run_linker "$FIXTURE"
assert_rc 0
tick=$(printf '\140')
assert_contains "Write ${tick}Section 5${tick} literally, then cite Section [6](#6-roadmap)."

begin_case "9 a link or image label never gains a nested link"
fixture nested <<'MD'
See [Section 5](other-doc.md) and ![Section 6](diagram.png) and
[§7](https://example.invalid/plan) elsewhere, plus Section 5 here.
MD
run_linker "$FIXTURE"
assert_rc 0
assert_contains '[Section 5](other-doc.md)'
assert_contains '![Section 6](diagram.png)'
assert_contains '[§7](https://example.invalid/plan)'
assert_lacks '[Section [5]'
assert_contains 'plus Section [5](#5-architecture) here.'

# ---------------------------------------------------- idempotency
begin_case "10 a second run changes nothing"
fixture idempotent <<'MD'
See Section 5, Sections 5 and 6, and §5.13.
MD
run_linker "$FIXTURE"
assert_rc 0
cp "$FIXTURE" "$FIXTURE.linked"
run_linker "$FIXTURE"
assert_rc 0
assert_out_contains '0 new link(s)'
if cmp -s "$FIXTURE" "$FIXTURE.linked"; then ok; else report_failure "second run rewrote the file"; fi
run_linker --check "$FIXTURE"
assert_rc 0

# ---------------------------------------------- broken references
begin_case "11 a citation with no heading is reported, nothing written"
fixture dangling <<'MD'
See Section 99 and Sections 5 and 98 and §97.
MD
run_linker "$FIXTURE"
assert_rc 2
assert_out_contains 'Section 99 has no numbered heading'
assert_out_contains 'Section 98 has no numbered heading'
assert_out_contains 'Section 97 has no numbered heading'
assert_out_contains '3 broken section reference(s); nothing written'
assert_out_contains 'dangling.md:13:'
assert_unchanged

begin_case "12 a link whose destination is stale is reported"
fixture stale <<'MD'
See Section [5](#5-old-title) and [§6](#6-roadmap).
MD
run_linker "$FIXTURE"
assert_rc 2
assert_out_contains 'Section 5 links to #5-old-title, not #5-architecture'
assert_out_contains '1 broken section reference(s); nothing written'
assert_unchanged

begin_case "13 a link to a section that no longer exists is reported"
fixture removed <<'MD'
See Section [4](#4-gone).
MD
run_linker "$FIXTURE"
assert_rc 2
assert_out_contains 'Section 4 has no numbered heading'
assert_unchanged

begin_case "16 a partly linked citation list is completed"
fixture partial <<'MD'
Sections [5](#5-architecture) and 6, and Sections 5, [6](#6-roadmap), and 7.
A range: Sections [1](#1-purpose)-6 and Section [5](#5-architecture) alone.
MD
run_linker "$FIXTURE"
assert_rc 0
assert_contains 'Sections [5](#5-architecture) and [6](#6-roadmap), and Sections [5](#5-architecture), [6](#6-roadmap), and [7](#7-risks).'
assert_contains 'A range: Sections [1](#1-purpose)-[6](#6-roadmap) and Section [5](#5-architecture) alone.'
cp "$FIXTURE" "$FIXTURE.linked"
run_linker "$FIXTURE"
assert_rc 0
assert_out_contains '0 new link(s)'
if cmp -s "$FIXTURE" "$FIXTURE.linked"; then ok; else report_failure "second run rewrote the file"; fi

begin_case "17 a line opening with an issue reference is prose"
fixture issueref <<'MD'
The unit lands as
#265 tracks Section 5, and Section 6 follows.
MD
run_linker "$FIXTURE"
assert_rc 0
assert_out_contains '0 in headings'
assert_contains '#265 tracks Section [5](#5-architecture), and Section [6](#6-roadmap) follows.'

# --------------------------------------------------- check and usage
begin_case "14 --check reports an unlinked file without writing"
fixture needslinks <<'MD'
See Section 5.
MD
run_linker --check "$FIXTURE"
assert_rc 1
assert_out_contains 'is not fully linked'
assert_unchanged

begin_case "15 usage errors exit 2"
fixture usage <<'MD'
See Section 5.
MD
run_linker "$FIXTURE" "$FIXTURE"
assert_rc 2
assert_out_contains 'usage:'
run_linker "$TMP/does-not-exist.md"
assert_rc 2
assert_out_contains 'cannot read'
run_linker --bogus "$FIXTURE"
assert_rc 2
assert_out_contains 'usage:'

# ---------------------------------------------------------- summary
echo
echo "passed $pass assertion(s), failed $fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
