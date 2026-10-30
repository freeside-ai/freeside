# Work items — Both grounds, 30 Oct 2026

Three PRs, serial. Each re-records the dusk screenshot digests it touches and writes a devlog note.

## 1 · Palette: Dead Channel dusk, Overcast day

Files: `app/Sources/FreesideCore/DesignLanguage.swift` (`FreesidePalette` only), `app/Tests/…/DesignContrastTests*`.

Replace `FreesidePalette`'s colour lines with `FreesidePalette-day.swift.md` (it carries both appearances). Identity values — bronze, tawny, wax, water — do not move; `accentText.day` deepens to `0x7A5A0C`. Derived cuts keep their derivations (`quoteWash = ground3.dusk`, `quoteRule = accentBorder.dusk`, `hover = ground3.dusk`, `segmentSelected = ground3.dusk`, `milestoneConnector = ground3.dusk`), so they follow automatically; `itemBorder.dusk` becomes its own value (`0x3E434A`) instead of `milestonePrior.dusk`; `neutralWash.dusk = accentWashSoft.dusk`.

Done when: `DesignContrastTests` pass with no floor relaxed; day and dusk screenshots re-recorded for Inbox, decision detail, task timeline, New Task sheet, diff reader, menu-bar panel on Mac and Inbox + decision on iPhone; the devlog note records any value the tests forced off the handoff hex.

## 2 · Identity assets on the new ground

Files: the day and dusk app icons, keys, favicons and the GitHub avatar under `app/Apps/**` and wherever the daemon or docs ship them; the handoff's `assets/` are the sources.

Only the ground rectangle changes (dusk #16120E → #131416; the day icon's ink ground #2B2416 → #212120). Mark geometry, stroke widths (`mark-frame-w 3.2`, `mark-inlay-w 2.4`) and the tawny fill are unchanged.

Done when: both icons render on their new grounds in the Dock, menu bar and About.

## 3 · Docs and tokens

Files: `docs/design/handoff-2026-10-30-dark-ground/` (this folder, committed), `tokens.css` wherever the repo mirrors it, `app/SURFACES.md` At a Glance.

Done when: SURFACES.md names Overcast and Dead Channel as the two grounds and links both decision pages; `tokens.css` matches `FreesidePalette` in both blocks.
