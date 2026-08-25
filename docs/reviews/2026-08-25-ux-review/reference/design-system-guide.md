# Freeside Design System

Freeside is an agent control plane by Free as in Bird: a local, durable workflow controller. Tagline: **"The harness runs the agent. You hold the reins."** Category line: **"An agent control plane."** Never decorate the category line; the tagline carries no product name by design (the wordmark or category line self-identifies where needed).

Canonical sources: plan §15 in the freeside repo (identity policy, revision 10) and devlog/2026-07-17-0050-brand-register.md (rationale and every rejected alternative). This guide is the design-facing distillation.

## Casing

Freeside is a proper noun — capitalized wherever prose can carry it, including the wordmark. Downcase only where the medium demands it: freeside.ai, github.com/freeside-ai, the daemon `freesided`. The org is Free as in Bird (also a proper noun).

## The two grounds

One identity, two canonical grounds. **Light arrives as Freeside** (vellum, bronze — engineered ease); **dark arrives as Straylight** (umber, tawny — kept gravity). Modes follow the viewer's system setting everywhere; the dichotomy assigns *meaning*, never audience or surface. The two themes are not renderings of each other: design each on its own terms. Terminals follow the developer's own terminal theme.

## Palette (tokens.css carries the working set)

| Token | Day | Dusk | Rule |
|---|---|---|---|
| ground | #EDE7D6 vellum | #16120E umber | grounds trade places with text |
| text | #2B2416 | #EAE3CF | |
| accent | #8F6B14 bronze | #C2912E tawny | one metal in two ages |
| water | #6F9EA3 pool | #5D8489 mere | one water in two ages; quiet note only |
| wax | #8A2D1C | #9A3520 | ONE color (dusk value is a legibility lift only); seal moments only — approvals, signatures |

Grammar: **ages are for the materials of the place; artifacts keep their color.** **Green is reserved for the semantic palette** (success/go) and never brands anything: success states are quiet (shape + neutral tick), failure keeps wax, attention keeps the accent. Imported greens (GitHub checks, diffs, terminal output) stay green. Semantic colors never borrow the accent.

## Type

- **Source Serif Pro** — display AND text. Wordmark, headings, and emphasis at Medium (500); body at Regular (400). Never heavier than 500 above ~40px. Italic is the tagline's voice.
- **IBM Plex Sans** — UI chrome, annotations, buttons, captions.
- **IBM Plex Mono** — the evidence register, first-class: digests, checks, ledger lines, labels. Not a caption face; it is how the product states facts.
- Plex Sans + Mono are one superfamily voice (chrome and ledger share letterforms). Commercial upgrades if ever wanted: Söhne, Berkeley Mono.
- The costume test for type: letterforms must perform no period or flavor. Rejected: Alegreya (performed the manuscript), Literata (idiosyncratic italics), Archivo (thin, then anonymous).

## The mark: the key

The Freeside key — a slab-serif **F** whose stem flows unbroken into a nib-shaped bow: the stem *is* the spindle, running through the bow to its tip, with two crescent voids beside it and a gold dot at the neck (Straylight at the axis head). Letter, key, and station in one continuous stroke. Monotone in the ground's ink; the dot is the only color moment.

Drawn spec (assets/ + assets/key/ carry the cut files — copy them, never reconstruct):
- Master (viewBox 153 54 242 404 — right-padded so the top bar sits at viewBox center; naive centering lands bar-centered): one even-odd path; dot r7.4 at (240.1, 302.85). The dot ages with the **body** it sits on, not the ground: #B99A4A on an ink (day) body, #8F6B14 on a cream (dusk) body.
- Dusk cut widens each crescent ~2 units and thins the spindle (24→20) so the pair reads equal against irradiation — solid marks don't take the ×0.92 the old strokes did.
- The master serves unmodified down to ~20px; no intermediate recut exists on purpose. Favicons live on native pixel grids (32: 2px slot + 2px dot; 16: solid — slab F kept, nib plain, dot retired).
- App tile: key at 80% of tile height, rx 22.5%, bar-centered (inner x=133/512, not the bbox); day = vellum key on an ink tile, dusk = cream key on the true dusk ground. Both bodies cream → both dots #8F6B14.
- Lockup: key beside the wordmark, optically centered; clear space on all sides = the bar's height; minima — key ≥28px tall in lockup, ≥20px alone.
- One color: the dot inverts to a pierced hole so the artifact survives as negative space (freeside-mark-*-mono.svg / freeside-key-mono.svg); below ~24px the piercing closes gracefully.

Hard rules for identity assets: **never depict the agent** (no bird, no figure, no mascot); the key is one artifact — adapt per size, never redraw or swap; the dot ages with its body, never with the ground; semantic colors never touch the mark.

**The signet box is demoted, not deleted** — it survives only as the *seal* artifact for approval and signature moments (the Seal component; redraw from the spec in repo history, its cuts retired from assets/). The key now states the letter plainly; the old "whisper" doctrine retires with the box.

## Emotional target

**Calm command** — one temperament, two weathers (engineered ease by day, kept gravity by dusk). Should feel: composed, vigilant (care for the wall, not warden energy), sovereign, earned-trust, quietly mythic, patient. Must NOT feel: accelerationist, neon-cyberpunk, cute, enterprise-gloss, fear-led, acquisitive, or **costumed** — the register (ward, signet, gauntlet, seals) is load-bearing vocabulary, never decoration; every surface stays mundane. If a design raises the viewer's pulse, it is lying about the product.
