// Dark ground page. Evaluate after lib.js.
// ---- assemble --------------------------------------------------------------------------------------
const deadBand = band('Dead Channel · full fidelity', 'tuned token set · Mac detail column at 1,300pt · iPhone at 390pt',
  frame('Mac · Inbox and final-review card', mac(dead, 'Inbox', sidebar(dead, MAC, 0, inboxRows(dead, MAC), [2, 2]), finalCard(dead, MAC, 640), 780)),
  frame('Mac · Task timeline, newest first', mac(dead, 'Tasks', sidebar(dead, MAC, 1, taskRows(dead, MAC), [2, 2]), timelineCard(dead, MAC, 640), 900)),
  frame('Mac · New Task sheet', mac(dead, 'Inbox', sidebar(dead, MAC, 0, inboxRows(dead, MAC), [2, 2]), finalCard(dead, MAC, 640) + sheet(dead, MAC), 780)),
  frame('Mac · Diff reader', mac(dead, 'Inbox', sidebar(dead, MAC, 0, inboxRows(dead, MAC), [2, 2]), diffReader(dead, MAC, 760), 780)),
  frame('Mac · Menu-bar panel', menuBar(dead, MAC)),
  frame('iPhone · Inbox', phoneInbox(dead, IOS)),
  frame('iPhone · Final-review card', phoneCard(dead, IOS))
);
const compareBand = band('Soot and Lacquer · for comparison', 'candidate values as given · inbox and card only',
  frame('Soot · Mac Inbox and card', mac(soot, 'Inbox', sidebar(soot, MAC, 0, inboxRows(soot, MAC), [2, 2]), finalCard(soot, MAC, 640), 780)),
  frame('Lacquer · Mac Inbox and card', mac(lacq, 'Inbox', sidebar(lacq, MAC, 0, inboxRows(lacq, MAC), [2, 2]), finalCard(lacq, MAC, 640), 780)),
  frame('Dead Channel as given · same frame, untuned', mac(dead0, 'Inbox', sidebar(dead0, MAC, 0, inboxRows(dead0, MAC), [2, 2]), finalCard(dead0, MAC, 640), 780)),
  frame('Straylight today · the ground being replaced', mac(stray, 'Inbox', sidebar(stray, MAC, 0, inboxRows(stray, MAC), [2, 2]), finalCard(stray, MAC, 640), 780))
);
const ladder = t => `ground→ground2 ${f2(CR(t.g, t.g2))}, ground2→ground3 ${f2(CR(t.g2, t.g3))}, ground→sidebar ${f2(CR(t.g, t.sb))}`;
const critBand = `<div style="display: flex; flex-direction: column; gap: 22px;">${h2('Critique', 'against “exciting and powerful, not stodgy”, and against the invariants')}<div style="display: flex; gap: 48px; align-items: flex-start; flex-wrap: wrap; row-gap: 48px;">
${crit('Dead Channel', dead0, 'Recommended', [
  ['Goal', 'The only candidate that changes what the accent <em>is</em>. On umber, tawny was a lighter brown; on neutral graphite it becomes metal against stone — the signet mark, the Needs-decision chip and the one filled button carry the whole room. That is the villa: a cold house with one lit object. Nothing else had to get louder.'],
  ['Accent', `Holds. accentText ${f2(CR(dead0.acc, dead0.g2))}:1 on ground2 (was ${f2(CR(stray.acc, stray.g2))} on Straylight), and the filled button’s ground2 label is ${f2(CR(dead0.g2, dead0.acc))}:1. Still gold-ochre: the hue did not move, only its surroundings.`],
  ['Wax · water', `Both gain. Wax ${f2(CR(dead0.wax, dead0.g2))}:1, and because the ground is no longer red-brown, the Unverified notice is unmistakably a different colour from the card. Water ${f2(CR(dead0.water, dead0.g2))}:1 and reads as teal rather than a cooler dim ink, so links finally look like links.`],
  ['Elevation', `Best ladder of the three: ${ladder(dead0)}. Weak spots as given: itemBorder (#43474D) at 1.8:1 on ground2 is the card’s only edge and nearly disappears; milestonePrior rings at 1.8:1 next to a 1.7:1 connector; waxWash reads as mud. All tuned below.`],
  ['Risk', 'The given greys lean blue (<code style="font-family: ${MONO}; font-size: 12px;">ruleStrong</code> #7B828A, <code style="font-family: ${MONO}; font-size: 12px;">inkFaint</code> #7E8187: blue 15 above red) while ink and dim ink are warm. Two temperatures on one surface reads as two palettes and drifts toward the rejected Nocturne. The tuned set pulls those three greys to within 6 of neutral; the ink stays warm, as it should — it is the vellum, dimmed.']
])}
${crit('Soot', soot, 'Safe, not the brief', [
  ['Goal', 'Straylight with the brown cut by two thirds. It is calmer and cleaner than today, and it is still the same room: a warm ground under a warm accent, so tawny remains a lighter version of the floor. “Less stodgy” rather than “exciting”. If Dead Channel had been rejected, Soot is the fallback that risks nothing.'],
  ['Accent', `${f2(CR(soot.acc, soot.g2))}:1 on ground2 — plenty — but the chip and filled button sit at ~2% chroma distance from ground3, so the accent reads by lightness alone. The memorable move is softened, not lost.`],
  ['Wax · water', `Wax ${f2(CR(soot.wax, soot.g2))}:1; the Unverified wash (#261512) is still a sibling of ground2 (#191715), which is the Straylight problem in miniature. Water ${f2(CR(soot.water, soot.g2))}:1, and teal on warm grey reads clean.`],
  ['Elevation', `${ladder(soot)} — identical steps to today, so the card still relies on itemBorder (1.9:1) to exist. Sidebar = ground2, same as today.`],
  ['Verdict', 'Passes every invariant because it changes almost nothing. It does not answer the owner’s complaint.']
])}
${crit('Lacquer', lacq, 'Dramatic, structurally fragile', [
  ['Goal', `Near-black makes the accent loudest of all (${f2(CR(lacq.acc, lacq.g2))}:1) and ink the brightest (${f2(CR(lacq.ink, lacq.g2))}:1). It is the most “powerful” first impression — and the one that most resembles a terminal theme, which the invariants rule out. On an iPhone OLED, #0A0908 is effectively off, and a warm-black field with gold text and a hairline grid is the cyberpunk costume by other means.`],
  ['Accent', 'Holds on contrast, fails on register: #DBA63A on near-black is the brightest, most saturated-looking tawny of the four, and the step to “yellow” is short. The filled button starts to glow without any glow being drawn.'],
  ['Wax · water', `Both fine numerically (wax ${f2(CR(lacq.wax, lacq.g2))}, water ${f2(CR(lacq.water, lacq.g2))}). But every wash is within 1.1:1 of its ground, so a notice is a border with nothing inside it, and a diff’s add/remove lines are coloured text on nothing.`],
  ['Elevation', `${ladder(lacq)}. The ladder is gone by design: cards exist only as hairlines, and the given rule (#262321) is 1.2:1, so they barely exist. Sidebar at 1.02:1 from ground is invisible. To work, rule would have to come up to ~3:1 everywhere, which turns the villa into a wireframe.`],
  ['Verdict', 'The most exciting swatch and the least Freeside surface. Long reading at 16:1 also halates on a Mac at night.']
])}
</div></div>`;

const recBand = `<div style="display: flex; flex-direction: column; gap: 22px;">${h2('Recommendation · Dead Channel, tuned', 'mid-tones and washes first, then the Increased Contrast cuts · day values unchanged')}
<div style="display: flex; gap: 48px; align-items: flex-start; flex-wrap: wrap;">
${prose(`<p style="margin: 0 0 10px;">Dead Channel. It is the only ground that changes the accent’s meaning instead of its volume, and it does so without touching the accent, the type, the controls or the mark. The tuning below is small and mostly about the middle of the ladder, which the candidate table had not addressed.</p>
<p style="margin: 0 0 10px;"><strong style="font-weight: 500; color: #2B2416;">What changed.</strong> ground3 lifted one step so the selected row and the quote block read on their own; itemBorder separated from milestonePrior and lifted so the card has an edge; secondaryBorder and milestonePrior lifted for the same reason; the three structural greys (ruleStrong, inkFaint, secondaryBorder) pulled toward neutral so the chrome and the warm ink stop reading as two palettes; every wash lifted a step because on a neutral ground a tinted wash needs less lightness to read as colour, and the given values read as dirt. Accent, wax, water, ink, dim ink and the diff colours are as given.</p>
<p style="margin: 0;"><strong style="font-weight: 500; color: #2B2416;">What fails and the fix.</strong> rule, secondaryBorder and itemBorder sit below 3:1 on every ground — as they do in Straylight, Soot and Lacquer, and by design: they are tints of the ground, and separation is carried by the ground steps. That is exactly why they promote to ruleStrong (4.47:1 on ground2) under Increased Contrast; the promotion is the fix and it is already in DesignLanguage.swift. ruleStrong, accentBorder and inkFaint clear 3:1 on ground, ground2 and ground3. Every text token clears 4.5:1 on ground and ground2; the two closest are waxText on ground3 (${f2(CR(dead.wax, dead.g3))}:1, a wax-outlined control on hover) and inkFaint on ground3 (${f2(CR(dead.faint, dead.g3))}:1, disabled, floor 3:1). Nothing is listed red in the table.</p>`, 880)}
<div style="display: flex; flex-direction: column; gap: 10px;">${cap('Increased Contrast · Dead Channel')}<div style="display: flex; gap: 24px;">${frame('Standard', mac(dead, 'Inbox', sidebar(dead, MAC, 0, inboxRows(dead, MAC), [2, 2]), finalCard(dead, MAC, 640), 720).replace('width: 1300px', 'width: 1000px'))}${frame('Increased Contrast · hairlines → ruleStrong, faint → dim, accent brighter', mac({ ...dead, rule: dead.rs, sec: dead.rs, itemB: dead.rs, faint: dead.dim, acc: deadIC.acc, accB: deadIC.accB, wax: deadIC.wax, water: deadIC.water, mp: dead.rs }, 'Inbox', sidebar({ ...dead, rule: dead.rs, sec: dead.rs, itemB: dead.rs, acc: deadIC.acc }, MAC, 0, inboxRows(dead, MAC), [2, 2]), finalCard({ ...dead, rule: dead.rs, sec: dead.rs, itemB: dead.rs, faint: dead.dim, acc: deadIC.acc, accB: deadIC.accB, wax: deadIC.wax, water: deadIC.water }, MAC, 640), 720).replace('width: 1300px', 'width: 1000px'))}</div></div>
</div>
${tokenTable(dead0, dead, deadIC)}
${prose(`Derived cuts, unchanged in role: <code style="font-family: ${MONO}; font-size: 12px;">quoteWash</code> = ground3, <code style="font-family: ${MONO}; font-size: 12px;">quoteRule</code> = <code style="font-family: ${MONO}; font-size: 12px;">accentBorder</code>, <code style="font-family: ${MONO}; font-size: 12px;">hover</code> = ground3, <code style="font-family: ${MONO}; font-size: 12px;">segmentSelected</code> = ground3, <code style="font-family: ${MONO}; font-size: 12px;">neutralWash</code> = <code style="font-family: ${MONO}; font-size: 12px;">accentWashSoft</code>, <code style="font-family: ${MONO}; font-size: 12px;">milestoneConnector</code> = ground3. Increased Contrast: rule, <code style="font-family: ${MONO}; font-size: 12px;">secondaryBorder</code>, <code style="font-family: ${MONO}; font-size: 12px;">itemBorder</code> and <code style="font-family: ${MONO}; font-size: 12px;">milestonePrior</code> → <code style="font-family: ${MONO}; font-size: 12px;">ruleStrong</code>; <code style="font-family: ${MONO}; font-size: 12px;">inkFaint</code> → <code style="font-family: ${MONO}; font-size: 12px;">inkDim</code>; <code style="font-family: ${MONO}; font-size: 12px;">accentText</code> and <code style="font-family: ${MONO}; font-size: 12px;">accentBorder</code> → #E3B449 (${f2(CR('#E3B449', dead.g2))}:1 on ground2); <code style="font-family: ${MONO}; font-size: 12px;">waxText</code> → #E2855F; <code style="font-family: ${MONO}; font-size: 12px;">waterText</code> → #9FC5CA. The tawny IC cut is brighter, not yellower: hue held at the given accent’s, lightness up one step.`, 1200)}
</div>`;

const doc = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<script src="./support.js"></script>
</head>
<body>
<x-dc>
<helmet data-dc-atomics>
  <meta name="design_doc_mode" content="canvas" />
  <link href="https://fonts.googleapis.com/css2?family=Source+Serif+Pro:ital,wght@0,400;0,500;1,400&amp;family=IBM+Plex+Sans:wght@400;500;600&amp;family=IBM+Plex+Mono:wght@400;500;600&amp;display=swap" rel="stylesheet" />
  <style>
    body { margin: 0; background: #DAD3BF; }
    a { color: #8F6B14; } a:hover { color: #B99A4A; }
  </style>
</helmet>
<section id="turn1" style="box-sizing: border-box; padding: 48px 48px 120px; display: flex; flex-direction: column; gap: 96px; font-family: ${SANS}; color: #2B2416; min-width: 4200px;">
  <div style="display: flex; flex-direction: column; gap: 8px;">${h1('Dark ground — 30 Oct 2026')}${prose('Three grounds for Straylight, rendered on the real surfaces. Dead Channel at full fidelity with the tuned token set; Soot, Lacquer, the untuned Dead Channel and today’s Straylight on the same inbox-and-card frame for comparison. Critique and the tuned tokens with contrast checks follow. Light mode, the mark and the type are untouched.')}</div>
  ${deadBand}
  ${compareBand}
  ${critBand}
  ${recBand}
</section>
</x-dc>
<script type="text/x-dc" data-dc-script>
class Component extends DCLogic {
  renderVals() { return {}; }
}
</script>
</body>
</html>
`;
await saveFile('Dark Ground - 30 Oct 2026.dc.html', doc);
log('chars', doc.length);
log('checks', ['ink', 'dim', 'faint', 'acc', 'wax', 'water', 'add', 'rem', 'rs', 'accB', 'sec', 'itemB', 'mp'].map(k => `${k} ${f2(CR(dead[k], dead.g))}/${f2(CR(dead[k], dead.g2))}/${f2(CR(dead[k], dead.g3))}`).join('  '));
log('ladder', ladder(dead), 'g2 on acc', f2(CR(dead.g2, dead.acc)), 'IC', Object.entries(deadIC).map(([k, v]) => `${k} ${f2(CR(v, dead.g2))}`).join(' '));
