# Freeside — Interfaces, 8 Oct 2026 · client-only handoff

Date: 2026-10-08 · Supersedes the presentation rules of `docs/design/handoff-2026-10-06-refined-interfaces/` where the two disagree; everything that handoff settled and this one does not touch stays as built.
Scope: **presentation only, both clients.** No daemon, API, contract or command change.

## Contents

| File | What it is |
|---|---|
| `WORK_ITEMS.md` | **The brief.** Six serial PRs, files touched, done-when per item |
| `CLAUDE_CODE_PROMPT.md` | Paste-ready prompt for Claude Code |
| `Interfaces - 8 Oct 2026.dc.html` | **The frames.** Band 1 Mac at the native ladder, band 2 iPhone, band 3 the remaining Mac surfaces. Caption above each frame: platform · surface · the owner issues it answers. ◆ = still an owner call |
| `Sync - 8 Oct 2026.dc.html` | What `main` drew on 8 Oct vs the 6 Oct frames, and the departures recorded in the devlog |
| `Card Box Options - 8 Oct 2026.dc.html`, `Tray Glyphs - 8 Oct 2026.dc.html` | The two comparisons the owner decided from (card keeps its border; `archivebox` / `checklist.checked` / `lock.slash`) |
| `tokens.css` | Day/dusk tokens as synced from `DesignLanguage.swift` on 8 Oct (diff cuts `#377142` / `#A93E2D`, dusk hover = ground-3, quote / item-border / notice cuts) |
| `_gen8/` | The generator that drew the frames. `lib8.js` holds both ladders (`MAC`, `PHONE`) and every atom with exact px; `groupA/B/C.js` compose the frames. The second reading of any frame |
| `design-direction.md` | P1–P8 |
| `assets/key/` | The key mark — status item only after item 5 |
| `support.js` | Runtime for the `.dc.html` files; open them directly in a browser |

## The two ladders (item 1)

| | Mac | iPhone |
|---|---|---|
| ask (serif medium) | 20 | 24 |
| statement (serif) | 15 | 16 |
| label (sans) | 13 | 15 |
| body (sans) | 13 | 13.5 |
| mono value | 12 | 13.5 |
| trailing summary (mono dim) | 12 | 12.5 |
| keyword (mono medium, 0.08em, caps) | 11 | 11.5 |
| chip (mono medium, 0.04em) | 11 | 11.5 |
| button height / label | 28 / 13 | 44 / 15 |
| disclosure label | 13 | 15 |
| gaps section / module / control | 18 / 9 / 8 | 20 / 10 / 10 |
| card padding | 22 22 20 | 18 16 16 |
| sidebar row serif / mono | 14 / 12 | 15.5 / 12.5 |
| card max width | 640 | 361 |

Floors: 11.5pt on iPhone; 11pt on Mac (mono caption).

## Rules this round changes

- **R10** two ladders replace the 4b scale.
- **R14** a notice never carries a text action; the act is a button (control group in a card; own line under the sentence on a banner). Revoked empties the detail pane around a filled Pair Again.
- **R31** task rows take the inbox row grammar; current phase and round ride the context line; the four-phase sentence and hold text leave the row (◆ 6.1). Section switcher carries both counts; scope controls none.
- **R13** system glyph, not the key: `archivebox`, `checklist.checked`, `lock.slash`.
- **R6** sheet submit fills only when valid; disabled recipe otherwise, same in both appearances.
- **R18** detail pane top-leading at one x, max 640; summary and timelines in the same bordered card as a decision.

## Still the owner's (do not settle)

Finding-card first-viewport budget · #1863 per-finding route · `Approve` on an observation-only dispute · 1,620pt vs a narrower inspector for two columns · `Operator · iPhone` (#1809).
