// Light ground page · 30 Oct 2026. Evaluate after lib.js.
const dayDerive = t => ({ ...t, itemB: t.rule, nAccW: t.accW, nWaxW: t.waxW, mc: t.rule, qW: t.accWS, qR: t.rs });
const free = dayDerive({ name: 'Freeside (current)', g: '#EDE7D6', g2: '#F3EEE1', g3: '#E4DDC7', sb: '#E4DDC7', rule: '#D6CDB2', sec: '#C9BFA2', rs: '#877D5C', ink: '#2B2416', dim: '#675D49', faint: '#827858', acc: '#7D5C0E', accB: '#8F6B14', accW: '#E9DFC2', accWS: '#ECE4CD', wax: '#8A2D1C', waxW: '#E8D5C9', water: '#3E6D72', waterW: '#DCE6E4', add: '#377142', rem: '#A93E2D', addW: '#DCE8D9', remW: '#F0D9D2', mp: '#B9AF92' });
const day = dayDerive({ name: 'Daylight', g: '#F4F4F2', g2: '#FCFCFB', g3: '#EAEAE7', sb: '#EDEDEA', rule: '#D9D9D5', sec: '#CBCBC6', rs: '#7B7B76', ink: '#212120', dim: '#5C5C58', faint: '#777772', acc: '#7A5A0C', accB: '#8F6B14', accW: '#EFE6CF', accWS: '#F3EEDF', wax: '#8A2D1C', waxW: '#F0DCD5', water: '#3B6B70', waterW: '#DEE8E7', add: '#357040', rem: '#A83D2C', addW: '#DFEADC', remW: '#F4DDD6', mp: '#B9B9B3' });
const plaster = dayDerive({ name: 'Plaster', g: '#ECEAE6', g2: '#F5F4F1', g3: '#E1DFDA', sb: '#E4E2DD', rule: '#D2D0CA', sec: '#C4C1BA', rs: '#7F7B73', ink: '#25231F', dim: '#605C55', faint: '#7A766E', acc: '#7D5C0E', accB: '#8F6B14', accW: '#EBE2CB', accWS: '#EEE8D8', wax: '#8A2D1C', waxW: '#EBD9D2', water: '#3E6D72', waterW: '#DCE5E4', add: '#377142', rem: '#A93E2D', addW: '#DDE8DA', remW: '#F1DAD3', mp: '#B5B2AA' });
const bone = dayDerive({ name: 'Bone', g: '#EBE8E1', g2: '#F3F0EA', g3: '#E2DED5', sb: '#E2DED5', rule: '#D4D0C6', sec: '#C7C2B6', rs: '#837C6B', ink: '#282319', dim: '#645D4E', faint: '#7E7765', acc: '#7D5C0E', accB: '#8F6B14', accW: '#EAE0C7', accWS: '#EDE6D3', wax: '#8A2D1C', waxW: '#E9D6CC', water: '#3E6D72', waterW: '#DBE5E3', add: '#377142', rem: '#A93E2D', addW: '#DCE7D8', remW: '#F0D9D1', mp: '#B7B0A0' });
const low0 = dayDerive({ name: 'Daylight, lowered (as given)', g: '#EEEEEB', g2: '#FAFAF8', g3: '#E4E4E0', sb: '#E7E7E3', rule: '#D5D5D1', sec: '#C8C8C3', rs: '#7B7B76', ink: '#212120', dim: '#5C5C58', faint: '#777772', acc: '#7A5A0C', accB: '#8F6B14', accW: '#EDE4CD', accWS: '#F1ECDD', wax: '#8A2D1C', waxW: '#EEDAD3', water: '#3B6B70', waterW: '#DCE6E5', add: '#357040', rem: '#A83D2C', addW: '#DDE8DA', remW: '#F2DBD4', mp: '#B6B6B0' });
// Tuned: sidebar takes ground3 (today's day derivation) so sidebar, ground and card are three steps; waterWash cooled a hair so water on it clears the floor with room.
const low = dayDerive({ ...low0, name: 'Overcast', sb: '#E4E4E0', waterW: '#DAE6E5' });
const lowIC = { acc: '#6B4E0B', accB: '#6B4E0B', faint: low.dim, rule: low.rs, sec: low.rs, itemB: low.rs, wax: '#71230F', water: '#33595E' };

const inboxFrame = (t, label, h, w) => frame(label, mac(t, 'Inbox', sidebar(t, MAC, 0, inboxRows(t, MAC), [2, 2]), finalCard(t, MAC, 640), h || 780, w));
const onGround = (t, inner, label) => `<div style="display: flex; flex-direction: column; gap: 8px; flex: none;">${cap(label)}<div style="background: ${t.g}; border: 1px solid ${t.sec}; border-radius: 12px; padding: 24px; width: 640px; box-sizing: content-box;">${inner}</div></div>`;

const dayBand = band('Overcast · full fidelity', 'Daylight lowered one step, tuned · Mac detail column at 1,300pt · iPhone at 390pt · 1,680pt windows for the glare question',
  inboxFrame(low, 'Mac · Inbox and final-review card'),
  frame('Mac · Task timeline', mac(low, 'Tasks', sidebar(low, MAC, 1, taskRows(low, MAC), [2, 2]), timelineCard(low, MAC, 640), 900)),
  frame('Mac · New Task sheet', mac(low, 'Inbox', sidebar(low, MAC, 0, inboxRows(low, MAC), [2, 2]), finalCard(low, MAC, 640) + sheet(low, MAC), 780)),
  frame('Mac · Diff reader', mac(low, 'Inbox', sidebar(low, MAC, 0, inboxRows(low, MAC), [2, 2]), diffReader(low, MAC, 760), 780)),
  frame('Mac · Menu-bar panel', menuBar(low, MAC)),
  frame('iPhone · Inbox', phoneInbox(low, IOS)),
  frame('iPhone · Final-review card', phoneCard(low, IOS)),
  inboxFrame(low, 'Mac · Overcast · full window, 1,680 × 1,000', 1000, 1680),
  inboxFrame(day, 'Mac · Daylight as tabled · same window — the glare test', 1000, 1680)
);
const compareBand = band('Daylight, Plaster, Bone and the lowered variant as given · for comparison', 'candidate values as tabled · inbox and card only',
  inboxFrame(day, 'Daylight as tabled · Mac Inbox and card'),
  inboxFrame(plaster, 'Plaster · Mac Inbox and card'),
  inboxFrame(bone, 'Bone · Mac Inbox and card'),
  inboxFrame(low0, 'Daylight, lowered · as given'),
  inboxFrame(free, 'Freeside today · the ground being replaced')
);
const pairBand = `<div style="display: flex; flex-direction: column; gap: 22px;">${h2('Paired with Dead Channel', 'the final-review card, night beside each day candidate')}<div style="display: flex; gap: 32px; align-items: flex-start; flex-wrap: wrap;">
${onGround(dead, finalCard(dead, MAC, 640), 'Dead Channel · night')}
${onGround(day, finalCard(day, MAC, 640), 'Daylight')}
${onGround(low, finalCard(low, MAC, 640), 'Overcast')}
${onGround(plaster, finalCard(plaster, MAC, 640), 'Plaster')}
${onGround(bone, finalCard(bone, MAC, 640), 'Bone')}
</div></div>`;

const ladderL = t => `ground→card ${f2(CR(t.g, t.g2))}, card→ground3 ${f2(CR(t.g2, t.g3))}, ground→sidebar ${f2(CR(t.g, t.sb))}, sidebar→card ${f2(CR(t.sb, t.g2))}`;
const critBand = `<div style="display: flex; flex-direction: column; gap: 22px;">${h2('Critique and answers', 'glare · pairing · floors')}<div style="display: flex; gap: 48px; align-items: flex-start; flex-wrap: wrap; row-gap: 48px;">
${crit('Daylight', day, 'Pop yes, glare yes', [
  ['Q1 · Glare', `At 1,300pt the card is a lit sheet and bronze is the only warm thing on it — the pop is real. At 1,680 × 1,000 the detail pane is a #F4F4F2 field with a #FCFCFB card at 1.07:1, and the two merge into one white panel; the sidebar at 1.07:1 from ground merges too. The window stops being a room with a desk in it and becomes a page. That is glare, and it reads as a generic web app rather than a resort by daylight.`],
  ['Accent', `Deepened to #7A5A0C as tabled: ${f2(CR(day.acc, day.g2))}:1 on the card, label on the fill ${f2(CR(day.g2, day.acc))}:1. Strong, and the hue is right, but on near-pure white bronze starts to look like a printed yellow — the metal needs a little grey around it to read as metal.`],
  ['Wax · water', `Wax ${f2(CR(day.wax, day.g2))}:1, the Unverified notice is unmistakable. Water ${f2(CR(day.water, day.g2))}:1 and teal on cool white is clean.`],
  ['Elevation', `${ladderL(day)}. The given sidebar (#EDEDEA) is neither ground nor ground3, so the three steps the brief asks for are two and a half.`]
])}
${crit('Overcast (Daylight, lowered)', low, 'Recommended', [
  ['Q1 · Glare', `The pop survives, and the spread is wider, not narrower: ground→card ${f2(CR(low.g, low.g2))} against Daylight's 1.07, sidebar→card ${f2(CR(low.sb, low.g2))} against 1.14. At 1,680 the pane is a grey field with a white card on it: the card is the lit object, the pane is the room. Less white overall, more white where it matters.`],
  ['Accent', `Same #7A5A0C: ${f2(CR(low.acc, low.g2))}:1 on the card, ${f2(CR(low.acc, low.g))}:1 on the ground, label on fill ${f2(CR(low.g2, low.acc))}:1. The step of grey in the ground is what keeps bronze reading as metal rather than yellow.`],
  ['Tuning', `Two changes. <code style="font-family: ${MONO}; font-size: 12px;">sidebarGround</code> = ground3 (#E4E4E0), which is today's day derivation and gives sidebar, ground and card three clear steps (ground→sidebar ${f2(CR(low.g, low.sb))}). <code style="font-family: ${MONO}; font-size: 12px;">waterWash</code> cooled a hair to #DAE6E5 so water on its wash sits at ${f2(CR(low.water, low.waterW))}:1 with room above the floor. Everything else as given.`],
  ['Q2 · Pairing', `The closest day analogue to Dead Channel: neutral with a hairline cool cast, ground steps at 1.11 and 1.22 against night's 1.08 and 1.15, warm ink on neutral ground in both. The card beside the Dead Channel card reads as the same object lit from the other side.`]
])}
${crit('Plaster', plaster, 'Softest, half-way', [
  ['Goal', `The most direct hue analogue of Dead Channel and the softest surface of the four: ${ladderL(plaster)}, no glare at any size. But at 2% saturation the ground is still faintly warm, and bronze on warm grey is a lighter version of the floor again — exactly the complaint, only quieter. Plaster is where you land if the owner finds the lowered Daylight too clinical.`],
  ['Accent', `#7D5C0E unchanged: ${f2(CR(plaster.acc, plaster.g2))}:1 on the card. Holds, with less contrast of temperature than on Daylight.`],
  ['Wax · water', `Wax ${f2(CR(plaster.wax, plaster.g2))}:1. Water ${f2(CR(plaster.water, plaster.g2))}:1 on the card but ${f2(CR(plaster.water, plaster.waterW))}:1 on its own wash — on the floor exactly. If Plaster is chosen, <code style="font-family: ${MONO}; font-size: 12px;">waterWash</code> goes to #DAE5E4.`]
])}
${crit('Bone', bone, 'Conservative, two products', [
  ['Goal', 'Vellum with the chroma halved. It is plainly warm, and next to graphite night the two appearances read as two products: a paper app by day and a stone app at night. It fixes nothing the brief names; it is the Soot of this round.'],
  ['Accent', `${f2(CR(bone.acc, bone.g2))}:1 on the card. Fine, and still a lighter brown on brown.`],
  ['Floors', `Water on its wash is ${f2(CR(bone.water, bone.waterW))}:1 — the one tabled value under a floor. Fix if chosen: <code style="font-family: ${MONO}; font-size: 12px;">waterWash</code> #D9E4E2.`],
  ['Elevation', `${ladderL(bone)}. Same steps as today.`]
])}
</div></div>`;

const LNOTES = {
  sidebarGround: 'Takes ground3, as the day palette does today (<code style="font-family: ${MONO}; font-size: 12px;">sidebarGround</code> = ground3 by day). Sidebar, ground and card become three steps: ground→sidebar 1.10, ground→card 1.11, card→ground3 1.22.',
  waterWash: 'Cooled a hair. waterText on it ' + f2(CR(low.water, low.waterW)) + ':1 (was ' + f2(CR(low0.water, low0.waterW)) + ').',
  accentText: 'Deepened as tabled for Daylight; ' + f2(CR(low.acc, low.g2)) + ':1 on the card, ' + f2(CR(low.acc, low.g)) + ':1 on the ground. IC takes today’s #6B4E0B.',
  ruleStrong: 'As given. ' + f2(CR(low.rs, low.g2)) + ':1 on the card, ' + f2(CR(low.rs, low.g3)) + ':1 on ground3 — an outlined control stays a button on hover.',
  inkFaint: 'As given; disabled floor 3:1, clears ' + f2(CR(low.faint, low.g2)) + ' on the card. IC promotes to inkDim.',
  itemBorder: 'rule, as today. Below 3:1 by design; promotes to ruleStrong under IC.',
  rule: 'Below 3:1 by design — a tint of the ground; separation is the ground step. Promotes to ruleStrong under IC.',
  secondaryBorder: 'Below 3:1 by design. Promotes to ruleStrong under IC.',
  milestonePrior: 'Decoration, paired with a label.',
  milestoneConnector: 'Follows rule.',
  noticeAccentWash: '= accentWash by day.', noticeWaxWash: '= waxWash by day.',
  accentWashSoft: 'Press fill and the quote wash; sits between card and ground3.'
};
const recBand = `<div style="display: flex; flex-direction: column; gap: 22px;">${h2('Recommendation · Overcast', 'two tokens tuned · pairs with Dead Channel · every floor clears')}
${prose(`<p style="margin: 0 0 10px;">Overcast — Daylight, lowered one step. Daylight proper answers question 1 the wrong way: at window size the ground and the card fuse into one white panel, which is glare and reads as a web page. Pulling the surface down one step — Overcast — keeps everything the brief wants from Daylight — a neutral ground, bronze as the only warm thing, the card as the lit object — and widens the elevation spread instead of narrowing it. Paired with Dead Channel it is the same product lit from the other side; Plaster is the fallback if the result reads too clinical, and Bone should not be pursued.</p>
<p style="margin: 0;"><strong style="font-weight: 500; color: #2B2416;">Q3 · Floors.</strong> After tuning, nothing fails. Every text token clears 4.5:1 on ground and card; the closest are inkFaint on ground3 (${f2(CR(low.faint, low.g3))}:1, disabled, floor 3:1) and waterText on ground3 (${f2(CR(low.water, low.g3))}:1). ruleStrong, accentBorder clear 3:1 on ground, card and ground3. rule, secondaryBorder and itemBorder sit below 3:1 as they do today and promote to ruleStrong under Increased Contrast. Of the candidates as tabled, one value is under a floor: Bone’s waterText on waterWash at ${f2(CR(bone.water, bone.waterW))}:1; Plaster’s sits at ${f2(CR(plaster.water, plaster.waterW))}:1 exactly.</p>`, 880)}
${tokenTable(low0, low, lowIC, LNOTES)}
${prose(`Derived cuts, unchanged in role: hover = ground3, <code style="font-family: ${MONO}; font-size: 12px;">segmentSelected</code> = card, <code style="font-family: ${MONO}; font-size: 12px;">itemBorder</code> = rule, <code style="font-family: ${MONO}; font-size: 12px;">neutralWash</code> = <code style="font-family: ${MONO}; font-size: 12px;">accentWashSoft</code>, quote wash = <code style="font-family: ${MONO}; font-size: 12px;">accentWashSoft</code> with a <code style="font-family: ${MONO}; font-size: 12px;">ruleStrong</code> rule, <code style="font-family: ${MONO}; font-size: 12px;">milestoneConnector</code> = rule. Increased Contrast by day: rule, <code style="font-family: ${MONO}; font-size: 12px;">secondaryBorder</code>, <code style="font-family: ${MONO}; font-size: 12px;">itemBorder</code> → <code style="font-family: ${MONO}; font-size: 12px;">ruleStrong</code>; <code style="font-family: ${MONO}; font-size: 12px;">inkFaint</code> → <code style="font-family: ${MONO}; font-size: 12px;">inkDim</code>; <code style="font-family: ${MONO}; font-size: 12px;">accentText</code> and <code style="font-family: ${MONO}; font-size: 12px;">accentBorder</code> → #6B4E0B (${f2(CR('#6B4E0B', low.g2))}:1 on the card); <code style="font-family: ${MONO}; font-size: 12px;">waxText</code> → #71230F; <code style="font-family: ${MONO}; font-size: 12px;">waterText</code> → #33595E — today’s IC cuts, which all clear on the new card.`, 1200)}
</div>`;

const doc = wrapDoc('Light ground — 30 Oct 2026', 'Three day grounds for Freeside plus Overcast — Daylight lowered one step, the chosen ground — on the same surfaces as the dark round. Overcast at full fidelity with 1,680pt windows for the glare question; Plaster, Bone, the lowered variant and today’s vellum on the inbox-and-card frame; the final-review card paired with Dead Channel; critique, answers and the tuned token table. Type, controls, the mark and the semantic colours are untouched.', [dayBand, compareBand, pairBand, critBand, recBand]);
await saveFile('Light Ground - 30 Oct 2026.dc.html', doc);
log('light chars', doc.length, 'low checks', ['ink', 'dim', 'faint', 'acc', 'wax', 'water', 'rs', 'accB'].map(k => `${k} ${f2(CR(low[k], low.g))}/${f2(CR(low[k], low.g2))}/${f2(CR(low[k], low.g3))}`).join('  '), 'water/wash', f2(CR(low.water, low.waterW)), 'g/sb', f2(CR(low.g, low.sb)));
