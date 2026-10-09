# Claude Code brief — Interfaces, 8 Oct 2026

You are implementing a presentation-only pass over the Freeside Mac and iOS clients from the attached design handoff folder (`design_handoff_freeside_8oct/`). First, copy the folder into the repo at `docs/design/handoff-2026-10-08-interfaces/` and commit it, so every PR can cite the frames by path. All paths below are relative to that folder unless they start with `app/` or `docs/`.

## Read first, in this order

1. `WORK_ITEMS.md` — six items, one PR each, in order. Each names the files it touches and what done means.
2. `README.md` — the two ladders and the six rule changes.
3. `Interfaces - 8 Oct 2026.dc.html` — the frames, at real size (Mac 1,180pt, iPhone 393pt). Open in a browser. `_gen8/lib8.js` is the exact-px source for every atom; `_gen8/group*.js` for every frame.
4. `app/SURFACES.md` and the Swift sources the item names. SURFACES.md is the truth for content; the frames are the truth for presentation. Where a frame drops something SURFACES.md lists (the task row's phase sentence), the item says so and the PR updates the SURFACES.md line.
5. The 6 Oct handoff at `docs/design/handoff-2026-10-06-refined-interfaces/` for everything this round does not change (card module orders, the quote, the disclosure shape, the claim marker, Title Case labels).

## Rules of engagement

- Serial PRs, in the order listed. Each re-records the shared screenshot digests and updates its `SURFACES.md` lines in the same PR.
- Item 1 first: every later surface is measured on the new ladders.
- Extend the `DesignLanguage.swift` vocabulary; never bypass it. Contrast floors (`DesignContrastTests`) win over any hex in the frames — record the departure in a devlog note as the 6–8 Oct notes did.
- A ◆ on a frame is an owner decision not taken. Implement the drawn default and name the row in the PR. Do not settle: the finding-card first-viewport budget, #1863, `Approve` on an observation-only dispute, inspector width vs 1,620pt, #1809.
- System chrome (window title, toolbar, inspector toggle, popups, context menus, pickers) stays native.
- iPhone evidence is manual, as before.

## Acceptance for every PR

The native screenshot matches the frame at the stated size; nothing under 11.5pt on iPhone or 11pt on Mac; one filled control per card or none; no `Notice` draws a text action; no `KeyMark` outside the status item; VoiceOver order unchanged from what `SURFACES.md` states.

## Finish

Write a devlog note per item in the house style (what departed from the frame and why), and update `SURFACES.md`'s At a Glance paragraph to name the 8 Oct ladders.
