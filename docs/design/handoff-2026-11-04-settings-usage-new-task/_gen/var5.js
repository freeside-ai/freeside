// Frames for "Usage - 4 Nov 2026": the usage page, one cut, iterated by eye in both grounds. Evaluated after var4.js.
// Structure from cut h (III); scale from the references; colour decided by looking, not inherited.

const POOLS4 = {
  claude: [
    {id: 'pool A', plan: 'Ultra', refreshed: '8:20 AM', agents: ['claude-opus', 'claude-sonnet'], limits: [{label: 'Weekly', used: 61, reset: '3d 0h'}, {label: 'Weekly · Fable', used: 100, usedUp: true, reset: '3d 0h', cap: 50, parked: 2}], parked: 2},
    {id: 'pool C', plan: 'Max', refreshed: '8:19 AM', agents: ['claude-opus-c'], limits: [{label: '5-hour', used: 10, reset: '4h 02m'}, {label: 'Weekly', used: 22, reset: '5d 11h'}, {label: 'Weekly · Fable', used: 40, reset: '5d 11h', cap: 50}]},
    {id: 'pool D', plan: 'Pro', refreshed: '7:05 AM', stale: true, agents: ['claude-sonnet-d'], limits: [{label: '5-hour', used: 88, stale: true, reset: '41m'}, {label: 'Weekly', used: 73, stale: true, reset: '1d 11h'}, {label: 'Weekly · Fable', used: 95, stale: true, reset: '1d 11h', cap: 50}]},
  ],
  codex: [
    {id: 'pool B', plan: 'ChatGPT Pro', refreshed: '8:21 AM', agents: ['codex'], limits: [{label: 'Weekly', used: 48, reset: '3d 15h'}]},
    {id: 'pool E', plan: 'ChatGPT Plus', refreshed: '8:21 AM', agents: ['codex-e'], limits: [{label: '5-hour', used: 70, reset: '1h 20m'}, {label: 'Weekly', used: 35, reset: '2d 4h'}]},
  ],
};
const tightest = (p) => p.limits.slice().sort((a, b) => (b.usedUp ? 101 : b.used) - (a.usedUp ? 101 : a.used))[0];
const RESET_ABS5 = {'3d 0h': 'Thu 9:00 PM', '5d 11h': 'Sun 8:30 PM', '1d 11h': 'Tomorrow 8:30 PM', '3d 15h': 'Fri 12:00 AM', '2d 4h': 'Thu 1:15 PM'};
const resetText = (l) => /d/.test(l.reset) ? `Resets ${RESET_ABS5[l.reset] || l.reset}` : `Resets in ${l.reset}`;
// The meter: 10pt, fill = left. `fillColor` is the one knob: 'ink' or 'accent'.
let PARKED = 'line';
const meter5 = (t, l, fillColor) => { const left = l.usedUp ? 0 : 100 - l.used; const low = !l.usedUp && left <= 15; const fill = low ? t.waxT : fillColor === 'accent' ? t.acc : t.ink; return `<div style="position: relative; height: 10px; background: ${t.g3}; border-radius: 5px; overflow: hidden; box-sizing: border-box; ${l.usedUp ? `box-shadow: inset 0 0 0 1.5px ${t.waxT};` : ''}"><div style="position: absolute; left: 0; top: 0; bottom: 0; width: ${left}%; background: ${fill}; opacity: ${fillColor === 'accent' ? 1 : t.name === 'day' ? 0.72 : 0.88}; border-radius: 5px;"></div>${l.cap ? `<div style="position: absolute; left: ${l.cap}%; top: 0; bottom: 0; width: 2px; background: ${t.ground};"></div>` : ''}</div>`; };
// The window block: name on the first line with the value trailing; the reset as its own dim line; the meter under both.
const block5 = (t, l, fillColor) => { const left = l.usedUp ? 0 : 100 - l.used; const warn = l.usedUp || left <= 15; return `<div style="display: flex; flex-direction: column; gap: 6px;"><div style="display: flex; justify-content: space-between; align-items: baseline; gap: 12px;"><span style="font-size: ${t.S.label + 1}px; color: ${t.ink};">${l.label}</span><span style="font-family: ${MONO}; font-size: ${t.S.label}px; color: ${warn ? t.waxT : t.ink}; white-space: nowrap;">${l.usedUp ? 'Used up' : left + '% left'}</span></div><span style="font-size: ${t.S.sum}px; color: ${t.dim};">${resetText(l)}${l.cap ? ` · up to ${l.cap}% of weekly` : ''}${PARKED === 'line' && l.usedUp && l.parked ? ` · <span style="color: ${t.waxT};">${l.parked} runs parked</span>` : ''}</span>${meter5(t, l, fillColor)}</div>`; };
// The pool head: id and plan on one line, the read time trailing; a wax line only when something is wrong.
const poolHead5 = (t, p) => { const tg = tightest(p); const wrong = tg.usedUp ? `${tg.label} is used up · ${p.parked || 0} runs parked until ${RESET_ABS5[tg.reset]}` : p.stale ? `Stale since ${p.refreshed} · the collector hasn’t answered` : null; return `<div style="display: flex; flex-direction: column; gap: 4px;"><div style="display: flex; justify-content: space-between; align-items: baseline; gap: 12px;"><span style="display: flex; gap: 8px; align-items: baseline;"><span style="font-size: ${t.S.stmt}px; font-family: ${SERIF}; font-weight: 500; color: ${t.ink};">${p.id}</span>${mono(t, p.plan, t.S.sum, t.dim)}</span><span style="display: flex; gap: 8px; align-items: center;">${PARKED === 'chip' && p.parked ? chip(t, p.parked + ' Parked', 'wax') : ''}${mono(t, (p.stale ? 'stale since ' : 'read ') + p.refreshed, 11, p.stale ? t.waxT : t.faint)}</span></div></div>`; };
const pool5 = (t, p, first, fillColor) => `<div style="display: flex; flex-direction: column; gap: 14px; padding-top: ${first ? 0 : 16}px; ${first ? '' : `border-top: 1px solid ${t.rule};`}">${poolHead5(t, p)}${p.limits.map(l => block5(t, l, fillColor)).join('')}</div>`;
const provider5 = (t, name, pools, fillColor) => col(t.S.mod, kw(t, name), `<div style="border: 1px solid ${t.itemB}; border-radius: 8px; padding: 16px 18px; display: flex; flex-direction: column; gap: 0;">${pools.map((p, i) => pool5(t, p, i === 0, fillColor)).join('')}</div>`);
const poolCard5 = (t, p, fillColor) => `<div style="border: 1px solid ${t.itemB}; border-radius: 8px; overflow: hidden; display: flex; flex-direction: column;"><div style="background: ${t.g3}; padding: 9px 18px 10px; border-bottom: 1px solid ${t.rule};">${poolHead5(t, p)}</div><div style="padding: 14px 18px 16px; display: flex; flex-direction: column; gap: 14px;">${p.limits.map(l => block5(t, l, fillColor)).join('')}</div></div>`;
const providerCards5 = (t, name, pools, fillColor) => col(t.S.mod, kw(t, name), col(10, ...pools.map(p => poolCard5(t, p, fillColor))));
const usage5cards = (t, fillColor) => sheetShell(t, [sheetHeader(t, 'Usage', 'What is left on each account'), providerCards5(t, 'Claude', POOLS4.claude, fillColor), providerCards5(t, 'Codex', POOLS4.codex, fillColor), `<div style="display: flex; justify-content: flex-end; gap: 10px; align-items: center;">${link(t, 'Collectors')}${btnOut(t, 'Refresh', false, false)}</div>`].join(''), btnOut(t, 'Done', false, false), 520);
// B: one provider card, each pool section headed by a strip on ground-3 (full bleed inside the card).
const poolStrip5 = (t, p, first, fillColor) => `<div style="display: flex; flex-direction: column; gap: 14px; ${first ? '' : 'margin-top: 18px;'}"><div style="margin: 0 -18px; padding: 8px 18px; background: ${t.g3}; ${first ? 'margin-top: -16px; padding-top: 12px; border-radius: 7px 7px 0 0;' : ''}">${poolHead5(t, p)}</div>${p.limits.map(l => block5(t, l, fillColor)).join('')}</div>`;
const providerStrips5 = (t, name, pools, fillColor) => col(t.S.mod, kw(t, name), `<div style="border: 1px solid ${t.itemB}; border-radius: 8px; padding: 16px 18px; display: flex; flex-direction: column; overflow: hidden;">${pools.map((p, i) => poolStrip5(t, p, i === 0, fillColor)).join('')}</div>`);
const usage5strips = (t, fillColor) => sheetShell(t, [sheetHeader(t, 'Usage', 'What is left on each account'), providerStrips5(t, 'Claude', POOLS4.claude, fillColor), providerStrips5(t, 'Codex', POOLS4.codex, fillColor), `<div style="display: flex; justify-content: flex-end; gap: 10px; align-items: center;">${link(t, 'Collectors')}${btnOut(t, 'Refresh', false, false)}</div>`].join(''), btnOut(t, 'Done', false, false), 520);
const usage5 = (t, fillColor) => sheetShell(t, [sheetHeader(t, 'Usage', 'What is left on each account'), provider5(t, 'Claude', POOLS4.claude, fillColor), provider5(t, 'Codex', POOLS4.codex, fillColor), `<div style="display: flex; justify-content: flex-end; gap: 10px; align-items: center;">${link(t, 'Collectors')}${btnOut(t, 'Refresh', false, false)}</div>`].join(''), btnOut(t, 'Done', false, false), 520);
// Inside the Settings window: the same column at the pane's width.
const usagePane5 = (t, fillColor) => win(t, 'usage', [paneHead(t, 'Usage', 'What is left on each account, as the daemon last read it.'), provider5(t, 'Claude', POOLS4.claude, fillColor), provider5(t, 'Codex', POOLS4.codex, fillColor), `<div style="display: flex; justify-content: flex-end; gap: 10px; align-items: center;">${link(t, 'Collectors')}${btnOut(t, 'Refresh', false, false)}</div>`].join(''), 1180);

const buildVar5 = () => {
  const D = mac(DAY), K = mac(DUSK);
  const bands = [];
  bands.push(band('Usage · a card per pool, the pool name on a ground-3 band, parked runs on the reset line', 'The card layout with the pool’s name row tinted, so the pools separate at a glance; a used-up window’s reset line ends with how many runs wait on it.',
    fr({label: 'A parked dusk', width: 560, caption: 'A · Dead Channel', rules: 'R1 R3 R8', src: 'AccountUsageView.swift · capacity holds', html: usage5cards(K, 'accent')}),
    fr({label: 'A parked day', width: 560, caption: 'A · Overcast', rules: 'R1 R3 R8', src: 'AccountUsageView.swift · capacity holds', html: usage5cards(D, 'accent')})));
  bands.push(band('Pool separation, two cuts', 'Accent fill (decided). A: one bordered card per pool. B: one card per provider with a ground-3 strip heading each pool.',
    fr({label: 'cards dusk', width: 560, caption: 'A · a card per pool · Dead Channel', rules: 'R1 R3 R8', src: 'AccountUsageView.swift', html: usage5cards(K, 'accent')}),
    fr({label: 'cards day', width: 560, caption: 'A · a card per pool · Overcast', rules: 'R1 R3 R8', src: 'AccountUsageView.swift', html: usage5cards(D, 'accent')}),
    fr({label: 'strips dusk', width: 560, caption: 'B · provider card, a strip per pool · Dead Channel', rules: 'R1 R3 R8', src: 'AccountUsageView.swift', html: usage5strips(K, 'accent')}),
    fr({label: 'strips day', width: 560, caption: 'B · provider card, a strip per pool · Overcast', rules: 'R1 R3 R8', src: 'AccountUsageView.swift', html: usage5strips(D, 'accent')})));
  bands.push(band('Earlier: pools as hairline sections', 'Structure from cut h; scale from the references (10pt meter, the value in the label face); a wax line under a pool only when something is wrong; the reset as its own line. Fill colour drawn both ways.',
    fr({label: 'ink dusk', width: 560, caption: 'ink fill · Dead Channel', rules: 'R1 R3 R8', src: 'AccountUsageView.swift', html: usage5(K, 'ink')}),
    fr({label: 'ink day', width: 560, caption: 'ink fill · Overcast', rules: 'R1 R3 R8', src: 'AccountUsageView.swift', html: usage5(D, 'ink')}),
    fr({label: 'accent dusk', width: 560, caption: 'accent fill · Dead Channel', rules: 'R1 R3 R8', src: 'AccountUsageView.swift', html: usage5(K, 'accent')}),
    fr({label: 'accent day', width: 560, caption: 'accent fill · Overcast', rules: 'R1 R3 R8', src: 'AccountUsageView.swift', html: usage5(D, 'accent')})));
  bands.push(band('In the Settings window', 'The same column inside the 1A window’s Usage pane.',
    fr({label: 'pane dusk', width: 760, caption: 'Usage pane · Dead Channel', rules: 'R1 R3 R8', src: 'SettingsSheet.swift', html: usagePane5(K, 'ink')})));
  return wrapDoc('Usage · 4 Nov 2026', 'The usage page as one cut, rendered and checked in both grounds, with the fill colour drawn both ways for the one decision left.', bands);
};
