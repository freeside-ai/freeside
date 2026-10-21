// Closed-card mockup for #2097 on the 8 Oct ladders. Appended to lib8.js at build time.
const FONTS = `<link href="https://fonts.googleapis.com/css2?family=Source+Serif+Pro:ital,wght@0,400;0,500;1,400&family=IBM+Plex+Sans:wght@400;500;600&family=IBM+Plex+Mono:wght@400;500;600&display=swap" rel="stylesheet">`;

// ── closed-card atoms ──
// Faint-cut status chip trailing the eyebrow: the historical register (18 Sep Chip cuts).
const closedHead = (t, keyword, status, lead) => `<div style="display: flex; flex-direction: column; gap: 10px;">${eyebrow(t, keyword, chip(t, status, 'faint'))}${ask(t, lead)}</div>`;
// R9 fact rows; a link value is the FactLinkRow (R3: away is a link).
const factLink = (t, label, text) => `<div style="display: flex; justify-content: space-between; align-items: baseline; gap: 12px;"><span style="font-size: ${t.S.label}px; color: ${t.ink}; flex: none;">${label}</span>${link(t, text)}</div>`;
const factStack = (t, label, text) => `<div style="display: flex; flex-direction: column; gap: 2px;"><span style="font-size: ${t.S.label}px; color: ${t.ink};">${label}</span><span style="color: ${t.dim}; font-size: ${t.S.body}px; line-height: 1.5;">${text}</span></div>`;
const timelineLink = (t) => `<div>${link(t, 'Open task timeline')}</div>`;
// Compact marks in the FAVORED / PROPOSED family: Applied in ink, Favored dim (R6, R23: quiet success).
const mark = (t, text, color) => kw(t, text, color);
// Inert route: the open card's quote block without its selection mark column, so it reads as a list, not a control.
const inertRoute = (t, label, cons, marks, applied) => `<div style="display: flex; background: ${t.qW}; border-radius: 6px; overflow: hidden; ${applied ? `box-shadow: inset 0 0 0 1px ${t.ink};` : ''}"><div style="width: 3px; background: ${t.qR}; flex: none;"></div><div style="padding: 8px 12px; display: flex; flex-direction: column; gap: 2px; min-width: 0; flex: 1;"><div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${stmt(t, label, t.S.stmt, 500)}<span style="display: flex; gap: 10px; flex: none;">${(marks || []).join('')}</span></div>${cons ? dimText(t, cons) : ''}</div></div>`;
const finding = (t, n, sev, title, where, routes) => item(t, [
  `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${kw(t, `Finding ${n} · ${sev}`)}${mono(t, where, t.S.sum, t.dim)}</div>`,
  stmt(t, title),
  col(6, ...routes)
].join(''));

// ── cards ──
const resolvedDispute = (t) => [
  closedHead(t, 'Review dispute', 'Resolved', 'Findings adjudicated.'),
  dimText(t, 'The agent disputed two shadow-reviewer findings; the run was held until the dispute was resolved.'),
  facts(t, [fact(t, 'Action', 'Adjudicate findings'), fact(t, 'Decided by', 'Ben’s iPhone'), fact(t, 'When', 'Oct 12, 14:32')], 'Outcome'),
  timelineLink(t),
  col(t.S.mod, kw(t, 'Findings'),
    finding(t, 1, 'P2', 'The retry loop re-reads the config on every attempt; a transient read error aborts the run.', 'daemon/retry.go:118', [
      inertRoute(t, 'Fix in the PR', 'Read once, retry the call', [mark(t, 'Favored', t.dim)], true),
      inertRoute(t, 'Decline', 'Keep as reported'),
      inertRoute(t, 'Defer to an issue', 'File and merge without it')
    ]),
    finding(t, 2, 'P3', 'The error message names the internal package path instead of the user-facing flag.', 'daemon/cmd/flags.go:41', [
      inertRoute(t, 'Fix in the PR', 'Rename in the message', [mark(t, 'Favored', t.dim)]),
      inertRoute(t, 'Decline', 'The path is what the log greps for', [], true),
      inertRoute(t, 'Defer to an issue', 'File and merge without it')
    ])),
  folds(t, disc(t, 'Run and Binding Details', 'attempt 2 · round 3 · 2 adjudicated'), disc(t, 'Source and Supporting Details', 'two claims · digests'), disc(t, 'Details'))
].join('');

const expiredBlocked = (t) => [
  closedHead(t, 'Publish blocked', 'Expired', 'Expired when the task was abandoned.'),
  dimText(t, 'Publication was held: the readiness checklist degraded after the base advanced.'),
  facts(t, [fact(t, 'Closed by', 'Daemon'), fact(t, 'When', 'Oct 9, 09:14'), factStack(t, 'Reason', 'The owner abandoned the task; the daemon retired its open items with the workflow.')], 'Outcome'),
  timelineLink(t),
  folds(t, disc(t, 'Readiness Checklist', 'degraded · 2 of 5 checks'), disc(t, 'Details'))
].join('');

const supersededEffect = (t) => [
  closedHead(t, 'Effect proposal', 'Superseded', 'Superseded by a revised proposal.'),
  dimText(t, 'Merging would close the source issue; the agent proposed the closing comment below.'),
  facts(t, [fact(t, 'Closed by', 'Daemon'), fact(t, 'When', 'Oct 12, 15:02'), factLink(t, 'Replaced by', 'Effect proposal · Oct 12, 15:02')], 'Outcome'),
  timelineLink(t),
  col(t.S.mod, kw(t, 'Proposed effect'), quote(t, stmt(t, 'Close owner/repo#724 on merge', t.S.stmt, 500) + dimText(t, 'Comment: “Fixed by #731; the retry loop now reads the config once.”'))),
  folds(t, disc(t, 'Details'))
].join('');

// Open card beside it for contrast: the 8 Oct dispute card as drawn there.
const openDispute = (t) => CARDS8_DISPUTE(t);

// ── sidebar in the Resolved scope ──
const CLOSED_ROWS = [
  {type:'Review dispute', chip:'Resolved', cut:'faint', summary:'Findings adjudicated.', context:'owner/repo · #724 · Ben’s iPhone', time:'Oct 12', key:'dispute'},
  {type:'Effect proposal', chip:'Superseded', cut:'faint', summary:'Superseded by a revised proposal.', context:'owner/repo · #724', time:'Oct 12', key:'effect'},
  {type:'Specification approval', chip:'Resolved', cut:'faint', summary:'Specification approved.', context:'owner/repo · #731 · This device', time:'Oct 11', key:'spec'},
  {type:'System health', chip:'Resolved', cut:'faint', summary:'Resolved by the daemon: the projection recovered.', context:'owner/repo', time:'Oct 10', key:'health'},
  {type:'Publish blocked', chip:'Expired', cut:'faint', summary:'Expired when the task was abandoned.', context:'owner/repo · #731', time:'Oct 9', key:'blocked'},
  {type:'Question', chip:'Dismissed', cut:'faint', summary:'Dismissed without an answer.', context:'owner/repo · #718 · Ben’s MacBook', time:'Oct 8', key:'question'},
];
const resolvedSidebar = (t, selectedKey) => sidebar(t, sidebarControls(t, 0, 1, ['Open', 'Resolved', 'All'], '', undefined) + `<div style="padding: 0 12px 12px; display: flex; flex-direction: column; gap: 6px; overflow: hidden;">${CLOSED_ROWS.map(r => listRow(t, Object.assign({}, r, {selected: r.key === selectedKey}))).join('')}</div>`);

// ── frames ──
const framesFor = (t0) => { const t = mac(t0), p = ph(t0); return [
  frame({width: 1180, label: `Mac ${t.name} — resolved review dispute`, caption: cap(`macOS · ${t.name}`, 'Inbox (Resolved) → review dispute, resolved by adjudication', '#2097'), html: macWindow(t, {title: 'Review Dispute', subtitle: 'Resolved · owner/repo', sidebar: resolvedSidebar(t, 'dispute'), detail: card(t, resolvedDispute(t))}), note: 'The ask face carries one sentence saying what happened (R24). Under it, the Outcome facts (R9) and the timeline link (R3). Every finding keeps its routes, drawn as the open card’s quote blocks without the selection column, so VoiceOver reads a list; the applied route keeps the open card’s 1px ink selection ring, the only trace of the decision, beside the lean’s FAVORED mark (R6, R23). No control group, no guidance link; one hairline above the folds (R26). The sidebar rows repeat the lead sentence with the status chip in the faint cut.', diamonds: [['A', 'Facts as rows (drawn) or folded into one sentence under the lead (“Adjudicated from Ben’s iPhone, Oct 12, 14:32”).'], ['B', 'Routes drawn inert (drawn) or hidden behind a “Routes” fold with only the applied route shown per finding.'], ['C', 'Timeline link under the facts (drawn) or as the first fold.']]}),
  frame({width: 640, label: `Mac ${t.name} — expired publish blocked`, caption: cap(`macOS · ${t.name} · card 640`, 'Publish blocked, expired at abandonment', '#2097'), html: card(t, expiredBlocked(t)), note: 'A daemon close names the cause in the lead and the daemon’s sentence in a stacked Reason row; Closed by reads “Daemon”. The original hold reason stays where the type draws it.', diamonds: []}),
  frame({width: 640, label: `Mac ${t.name} — superseded effect proposal`, caption: cap(`macOS · ${t.name} · card 640`, 'Effect proposal, superseded by a revision', '#2097'), html: card(t, supersededEffect(t)), note: 'Replaced by is a FactLinkRow to the successor item. The proposal text stays readable; Approve and Decline are not drawn.', diamonds: []}),
  frame({width: 393, label: `iPhone ${t.name} — resolved review dispute`, caption: cap(`iPhone · ${t.name} · card 361`, 'Review dispute, resolved', '#2097'), html: phone(p, card(p, resolvedDispute(p), 361), {back: 'Inbox', navTitle: 'Review Dispute'}), note: 'Same composition on the iPhone ladder; facts stay as rows (label left, mono right) at 361.', diamonds: []}),
]; };


// ── alternatives, one per owner call ──
// A′: the facts folded into one sentence under the lead; no fact rows.
const altA = (t) => [
  closedHead(t, 'Review dispute', 'Resolved', 'Findings adjudicated.'),
  dimText(t, 'Adjudicated from Ben’s iPhone on Oct 12 at 14:32.'),
  dimText(t, 'The agent disputed two shadow-reviewer findings; the run was held until the dispute was resolved.'),
  timelineLink(t),
  col(t.S.mod, kw(t, 'Findings'),
    finding(t, 1, 'P2', 'The retry loop re-reads the config on every attempt; a transient read error aborts the run.', 'daemon/retry.go:118', [
      inertRoute(t, 'Fix in the PR', 'Read once, retry the call', [mark(t, 'Favored', t.dim)], true),
      inertRoute(t, 'Decline', 'Keep as reported'),
      inertRoute(t, 'Defer to an issue', 'File and merge without it')
    ]),
    finding(t, 2, 'P3', 'The error message names the internal package path instead of the user-facing flag.', 'daemon/cmd/flags.go:41', [
      inertRoute(t, 'Fix in the PR', 'Rename in the message', [mark(t, 'Favored', t.dim)]),
      inertRoute(t, 'Decline', 'The path is what the log greps for', [], true),
      inertRoute(t, 'Defer to an issue', 'File and merge without it')
    ])),
  folds(t, disc(t, 'Run and Binding Details', 'attempt 2 · round 3 · 2 adjudicated'), disc(t, 'Source and Supporting Details', 'two claims · digests'), disc(t, 'Details'))
].join('');
// B′: only the applied route per finding, as a fact row; the others behind a fold.
const appliedRow = (t, label, favored) => `<div style="display: flex; justify-content: space-between; align-items: baseline; gap: 12px;"><span style="font-size: ${t.S.label}px; color: ${t.ink}; flex: none;">Route</span><span style="display: flex; gap: 10px; align-items: baseline;">${mono(t, label)}${favored ? mark(t, 'Favored', t.dim) : ''}${mark(t, 'Applied', t.ink)}</span></div>`;
const findingB = (t, n, sev, title, where, route, favored, cons) => item(t, [
  `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${kw(t, `Finding ${n} · ${sev}`)}${mono(t, where, t.S.sum, t.dim)}</div>`,
  stmt(t, title),
  appliedRow(t, route, favored),
  dimText(t, cons),
  disc(t, '2 Other Routes', favored ? '' : 'Fix in the PR was favored')
].join(''));
const altB = (t) => [
  closedHead(t, 'Review dispute', 'Resolved', 'Findings adjudicated.'),
  dimText(t, 'The agent disputed two shadow-reviewer findings; the run was held until the dispute was resolved.'),
  facts(t, [fact(t, 'Action', 'Adjudicate findings'), fact(t, 'Decided by', 'Ben’s iPhone'), fact(t, 'When', 'Oct 12, 14:32')], 'Outcome'),
  timelineLink(t),
  col(t.S.mod, kw(t, 'Findings'),
    findingB(t, 1, 'P2', 'The retry loop re-reads the config on every attempt; a transient read error aborts the run.', 'daemon/retry.go:118', 'Fix in the PR', true, 'Read once, retry the call'),
    findingB(t, 2, 'P3', 'The error message names the internal package path instead of the user-facing flag.', 'daemon/cmd/flags.go:41', 'Decline', false, 'The path is what the log greps for')),
  folds(t, disc(t, 'Run and Binding Details', 'attempt 2 · round 3 · 2 adjudicated'), disc(t, 'Source and Supporting Details', 'two claims · digests'), disc(t, 'Details'))
].join('');
// C′: the timeline link as the first fold row instead of under the facts.
const altC = (t) => [
  closedHead(t, 'Review dispute', 'Resolved', 'Findings adjudicated.'),
  dimText(t, 'The agent disputed two shadow-reviewer findings; the run was held until the dispute was resolved.'),
  facts(t, [fact(t, 'Action', 'Adjudicate findings'), fact(t, 'Decided by', 'Ben’s iPhone'), fact(t, 'When', 'Oct 12, 14:32')], 'Outcome'),
  col(t.S.mod, kw(t, 'Findings'),
    finding(t, 1, 'P2', 'The retry loop re-reads the config on every attempt; a transient read error aborts the run.', 'daemon/retry.go:118', [
      inertRoute(t, 'Fix in the PR', 'Read once, retry the call', [mark(t, 'Favored', t.dim)], true),
      inertRoute(t, 'Decline', 'Keep as reported'),
      inertRoute(t, 'Defer to an issue', 'File and merge without it')
    ]),
    finding(t, 2, 'P3', 'The error message names the internal package path instead of the user-facing flag.', 'daemon/cmd/flags.go:41', [
      inertRoute(t, 'Fix in the PR', 'Rename in the message', [mark(t, 'Favored', t.dim)]),
      inertRoute(t, 'Decline', 'The path is what the log greps for', [], true),
      inertRoute(t, 'Defer to an issue', 'File and merge without it')
    ])),
  folds(t, `<div style="display: flex; align-items: center; gap: 8px;"><span style="font-size: ${t.S.disc}px; color: ${t.ink};">What Followed</span>${link(t, 'Open task timeline')}</div>`, disc(t, 'Run and Binding Details', 'attempt 2 · round 3 · 2 adjudicated'), disc(t, 'Source and Supporting Details', 'two claims · digests'), disc(t, 'Details'))
].join('');
const altFrames = (t0) => { const t = mac(t0); return [
  frame({width: 640, label: 'Alt A — facts as one sentence', caption: cap(`macOS · ${t.name} · card 640`, 'A′ · the outcome as one sentence under the lead', '#2097'), html: card(t, altA(t)), note: 'Shorter by three rows. Costs the mono values (R9) and the hover instant on When (R8/R17); the device name and time become prose, so a long device name wraps.', diamonds: []}),
  frame({width: 640, label: 'Alt B — applied route only', caption: cap(`macOS · ${t.name} · card 640`, 'B′ · only the applied route per finding, the rest behind a fold', '#2097'), html: card(t, altB(t)), note: 'Shorter by four quote blocks. The lean and the declined routes are a fold away; the reader no longer sees what was not chosen without opening it.', diamonds: []}),
  frame({width: 640, label: 'Alt C — timeline link as the first fold', caption: cap(`macOS · ${t.name} · card 640`, 'C′ · the timeline link as the first row under the hairline', '#2097'), html: card(t, altC(t)), note: 'Keeps the facts block to facts. The only navigation the card offers moves below the findings, past the fold on a long dispute.', diamonds: []}),
]; };


// ── the applied route without a label ──
// One route block, styled by variant. o = {applied, favored, v}
const checkGlyph = (t, color) => `<span style="display: inline-block; width: 11px; height: 6px; border-left: 2px solid ${color}; border-bottom: 2px solid ${color}; transform: rotate(-45deg) translate(1px, -2px); flex: none;"></span>`;
const routeV = (t, label, cons, o) => {
  const fav = o.favored ? mark(t, 'Favored', t.dim) : '';
  if (o.v === 'only' && !o.applied) return '';
  const dimmed = o.v === 'weight' && !o.applied;
  const rail = o.v === 'rail' ? (o.applied ? t.ink : t.rule) : t.qR;
  const ring = o.v === 'ring' && o.applied ? `box-shadow: inset 0 0 0 1px ${t.ink};` : '';
  const bg = dimmed ? 'transparent' : t.qW;
  const labelHtml = dimmed ? `<span style="font-family: ${SERIF}; font-size: ${t.S.stmt}px; line-height: 1.45; color: ${t.dim};">${label}</span>` : stmt(t, label, t.S.stmt, 500);
  const trailing = `<span style="display: flex; gap: 10px; align-items: center; flex: none;">${fav}${o.v === 'check' && o.applied ? checkGlyph(t, t.ink) : ''}</span>`;
  return `<div style="display: flex; background: ${bg}; border-radius: 6px; overflow: hidden; ${ring}"><div style="width: 3px; background: ${dimmed ? 'transparent' : rail}; flex: none;"></div><div style="padding: ${dimmed ? '4px 12px' : '8px 12px'}; display: flex; flex-direction: column; gap: 2px; min-width: 0; flex: 1;"><div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${labelHtml}${trailing}</div>${cons && !dimmed ? dimText(t, cons) : ''}</div></div>`;
};
const notTaken = (t, names) => `<div style="color: ${t.dim}; font-size: ${t.S.sum}px; padding-left: 15px;">Not taken: ${names.join(' · ')}</div>`;
const findingV = (t, v, n, sev, title, where, routes) => { const blocks = routes.map(r => routeV(t, r[0], r[1], {applied: r[2], favored: r[3], v})).filter(Boolean); if (v === 'only') blocks.push(notTaken(t, routes.filter(r => !r[2]).map(r => r[0]))); return item(t, [
  `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${kw(t, `Finding ${n} · ${sev}`)}${mono(t, where, t.S.sum, t.dim)}</div>`,
  stmt(t, title),
  col(6, ...blocks)
].join('')); };
const findingsModule = (t, v) => col(t.S.mod, kw(t, 'Findings'),
  findingV(t, v, 1, 'P2', 'The retry loop re-reads the config on every attempt; a transient read error aborts the run.', 'daemon/retry.go:118', [['Fix in the PR', 'Read once, retry the call', true, true], ['Decline', 'Keep as reported', false, false], ['Defer to an issue', 'File and merge without it', false, false]]),
  findingV(t, v, 2, 'P3', 'The error message names the internal package path instead of the user-facing flag.', 'daemon/cmd/flags.go:41', [['Fix in the PR', 'Rename in the message', false, true], ['Decline', 'The path is what the log greps for', true, false], ['Defer to an issue', 'File and merge without it', false, false]]));
const VARIANTS = [
  ['ring', 'V1 · ink ring', 'The open card’s selected treatment kept as the record: a 1px ink ring on the block, no mark column. Same grammar before and after the decision; the ring is the only trace. FAVORED stays on the lean.'],
  ['rail', 'V2 · ink rail', 'The quote rail carries it: ink on the applied route, the hairline tone on the others. Lightest of the five; may be too quiet beside the FAVORED mark.'],
  ['check', 'V3 · checkmark', 'A checkmark glyph in ink where the word was, in the mark position. A glyph, not a label; the timeline’s “done” register.'],
  ['weight', 'V4 · weight only', 'The applied route keeps its block and consequence; the others drop to dim serif lines with no block, no rail, no consequence. The choice reads from what is drawn in full.'],
  ['only', 'V5 · applied only', 'Only the applied route is drawn; the others collapse to one dim “Not taken” line. Shortest, and the lean is lost unless it was applied.'],
];
const variantFrames = (t0) => { const t = mac(t0); return VARIANTS.map(([v, title, note]) => frame({width: 640, label: `Applied route — ${title}`, caption: cap(`macOS · ${t.name} · card 640 · findings module`, title, '#2097'), html: card(t, findingsModule(t, v)), note, diamonds: []})); };


// ── ring colour ──
const RINGS = [
  ['ink', 'C1 · ink ring — chosen 21 Oct', (t) => ({ring: t.ink}), 'Chosen by the owner on 21 Oct: the selection ring as drawn on the open card. Ink is the one colour that carries no meaning of its own, and “applied” is a choice, not a success (a declined finding was applied too).'],
  ['green', 'C2 · green ring', (t) => ({ring: t.add}), 'The diff-add green (R28). Reads as “go” at a glance, but tokens.css reserves green for diffs and says success is quiet: shape plus a neutral tick on the neutral wash. Off the rule as drawn.'],
  ['quiet', 'C3 · quiet-success formula', (t) => ({ring: t.ink, bg: t.neuW, tick: t.dim}), 'What tokens.css prescribes for success: the shape (the ring), a neutral tick, the neutral wash behind it. Slightly more than C1, still no colour.'],
  ['greentick', 'C4 · ink ring, green tick', (t) => ({ring: t.ink, tick: t.add}), 'Green as a glyph only, the ring stays ink. The smallest dose of colour; the same R28 objection applies to the tick.'],
];
const routeR = (t, label, cons, applied, favored, st) => `<div style="display: flex; background: ${applied && st.bg ? st.bg : t.qW}; border-radius: 6px; overflow: hidden; ${applied ? `box-shadow: inset 0 0 0 1px ${st.ring};` : ''}"><div style="width: 3px; background: ${t.qR}; flex: none;"></div><div style="padding: 8px 12px; display: flex; flex-direction: column; gap: 2px; min-width: 0; flex: 1;"><div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${stmt(t, label, t.S.stmt, 500)}<span style="display: flex; gap: 10px; align-items: center; flex: none;">${favored ? mark(t, 'Favored', t.dim) : ''}${applied && st.tick ? checkGlyph(t, st.tick) : ''}</span></div>${dimText(t, cons)}</div></div>`;
const findingR = (t, st, n, sev, title, where, routes) => item(t, [
  `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${kw(t, `Finding ${n} · ${sev}`)}${mono(t, where, t.S.sum, t.dim)}</div>`,
  stmt(t, title),
  col(6, ...routes.map(r => routeR(t, r[0], r[1], r[2], r[3], st)))
].join(''));
const ringModule = (t, st) => col(t.S.mod, kw(t, 'Findings'),
  findingR(t, st, 1, 'P2', 'The retry loop re-reads the config on every attempt; a transient read error aborts the run.', 'daemon/retry.go:118', [['Fix in the PR', 'Read once, retry the call', true, true], ['Decline', 'Keep as reported', false, false], ['Defer to an issue', 'File and merge without it', false, false]]),
  findingR(t, st, 2, 'P3', 'The error message names the internal package path instead of the user-facing flag.', 'daemon/cmd/flags.go:41', [['Fix in the PR', 'Rename in the message', false, true], ['Decline', 'The path is what the log greps for', true, false], ['Defer to an issue', 'File and merge without it', false, false]]));
const ringFrames = () => RINGS.map(([k, title, style, note]) => frame({width: 1330, label: `Ring colour — ${title}`, caption: cap('macOS · day and dusk · card 640 · findings module', title, '#2097'), html: `<div style="display: flex; gap: 48px; align-items: flex-start;">${card(mac(DAY), ringModule(mac(DAY), style(mac(DAY))))}${card(mac(DUSK), ringModule(mac(DUSK), style(mac(DUSK))))}</div>`, note, diamonds: []}));

const page = () => { const bands = (process.env.ALT_ONLY || process.env.VARIANTS_ONLY || process.env.RING_ONLY ? [] : process.env.DAY_ONLY ? [DAY] : [DAY, DUSK]).map(t => `<div style="display: flex; flex-direction: column; gap: 28px;" data-screen-label="${t.name}"><div style="font-family: ${SERIF}; font-weight: 500; font-size: 20px; color: #2B2416;">${t.name === 'day' ? 'Day' : 'Dusk'}</div><div style="display: flex; gap: 48px; align-items: flex-start; flex-wrap: wrap;">${framesFor(t).join('')}</div></div>`).join('');
  const ringBand = `<div style="display: flex; flex-direction: column; gap: 28px;" data-screen-label="ring-colour"><div style="font-family: ${SERIF}; font-weight: 500; font-size: 20px; color: #2B2416;">The ring’s colour — day beside dusk, Mac 640</div><div style="display: flex; flex-direction: column; gap: 48px; align-items: flex-start;">${ringFrames().join('')}</div></div>`;
  const variantBand = `<div style="display: flex; flex-direction: column; gap: 28px;" data-screen-label="applied-variants"><div style="font-family: ${SERIF}; font-weight: 500; font-size: 20px; color: #2B2416;">The applied route without a label — five readings of the Findings module, day, Mac 640</div><div style="display: flex; gap: 48px; align-items: flex-start; flex-wrap: wrap;">${variantFrames(DAY).join('')}</div></div>`;
  const altBand = `<div style="display: flex; flex-direction: column; gap: 28px;" data-screen-label="alternatives"><div style="font-family: ${SERIF}; font-weight: 500; font-size: 20px; color: #2B2416;">Alternatives — one per owner call, day, Mac 640</div><div style="display: flex; gap: 48px; align-items: flex-start; flex-wrap: wrap;">${altFrames(DAY).join('')}</div></div>`;
  return `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Closed Cards — #2097</title>${FONTS}<style>body { margin: 0; background: #DAD3BF; } a { color: #8F6B14; }</style></head><body>
<div style="box-sizing: border-box; padding: 48px 48px 120px; display: flex; flex-direction: column; gap: 90px; font-family: ${SANS}; color: #2B2416;">
<div data-screen-label="Intro" style="font-size: 12.5px; line-height: 1.6; color: #675D49; max-width: 820px;"><div style="font-family: ${SERIF}; font-weight: 500; font-size: 26px; color: #2B2416; margin-bottom: 8px;">Closed Cards — #2097 planning mockup, 21 Oct 2026</div>What a resolved, expired, or superseded card shows once the item read carries its conclusion. Drawn on the <a href="../handoff-2026-10-08-interfaces/Interfaces%20-%208%20Oct%202026.dc.html">8 Oct ladders</a> with the 8 Oct atoms and tokens; nothing new is introduced: the applied route keeps the open card’s selection ring, and the status chip on the card header takes the faint cut. Fixture data is the Wave 8 exit run’s dispute, two findings with three routes each. ◆ marks the owner calls; the drawn frames are the defaults, and the Alternatives band at the end draws the other side of each call. The fact group takes one keyword, OUTCOME, on decided and daemon-closed cards alike.<div style="margin-top: 14px; display: flex; flex-direction: column; gap: 6px;"><div><b style="color: #2B2416; font-weight: 500;">Rules applied</b> — R3 away is a link · R6 quiet success, no fill · R8/R17 short time, exact instant on hover · R9 fact rows · R23 compact marks · R24 the ask is not drawn once closed · R26 one hairline above the folds · chip cuts: faint for historical.</div><div><b style="color: #2B2416; font-weight: 500;">Precedent</b> — the retired <i>blocked</i> card: daemon facts only, no control group, ends at its facts.</div></div></div>
${bands}
${process.env.VARIANTS_ONLY || process.env.RING_ONLY || process.env.DAY_ONLY ? '' : altBand}
${process.env.ALT_ONLY || process.env.RING_ONLY || process.env.DAY_ONLY ? '' : variantBand}
${process.env.ALT_ONLY || process.env.VARIANTS_ONLY || process.env.DAY_ONLY ? '' : ringBand}
</div></body></html>`; };
process.stdout.write(page());
