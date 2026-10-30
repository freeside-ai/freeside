// Dark ground candidates · 30 Oct 2026. Evaluated inside run_script (readFile/saveFile/log in scope).
const SERIF = "'Source Serif Pro', 'Source Serif 4', serif", SANS = "'IBM Plex Sans', sans-serif", MONO = "'IBM Plex Mono', monospace";
const L = h => { const n = parseInt(h.slice(1), 16); const c = [16, 8, 0].map(s => { let v = ((n >> s) & 255) / 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4) }); return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2] };
const CR = (a, b) => { const x = L(a), y = L(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05) };
const f2 = n => n.toFixed(2);

// Token sets. Keys: g ground, g2, g3, sb sidebarGround, rule, sec secondaryBorder, rs ruleStrong, ink, dim, faint,
// acc accentText, accB accentBorder, accW accentWash, accWS accentWashSoft, wax waxText, waxW, water waterText, waterW,
// add/rem diff, addW/remW, mp milestonePrior, mc milestoneConnector, itemB itemBorder, nAccW/nWaxW notice washes, hover.
const stray = { name: 'Straylight (current)', g: '#16120E', g2: '#1E1812', g3: '#292117', sb: '#1E1812', rule: '#322A1E', sec: '#3D3426', rs: '#786D58', ink: '#EAE3CF', dim: '#B3A88E', faint: '#7D7460', acc: '#C2912E', accB: '#8A6A26', accW: '#26200F', accWS: '#221C11', wax: '#D26D4A', waxW: '#241310', water: '#7FAAAF', waterW: '#17201F', add: '#7FB38A', rem: '#E07A62', addW: '#16261A', remW: '#2E1812', mp: '#4A3F2C', mc: '#292117', itemB: '#4A3F2C', nAccW: '#2C2412', nWaxW: '#2E1812' };
const dead0 = { name: 'Dead Channel (as given)', g: '#131416', g2: '#1A1C1F', g3: '#232629', sb: '#1A1C1F', rule: '#2B2F33', sec: '#363B40', rs: '#7B828A', ink: '#E9E7E0', dim: '#B2B0A6', faint: '#7E8187', acc: '#D4A23A', accB: '#96742B', accW: '#272214', accWS: '#211F18', wax: '#D97553', waxW: '#281715', water: '#86B1B6', waterW: '#172124', add: '#82B78E', rem: '#E27E66', addW: '#172619', remW: '#2D1A16', mp: '#43474D', mc: '#232629', itemB: '#43474D', nAccW: '#272214', nWaxW: '#281715' };
// Tuned Dead Channel: mid-tones and washes first. See the notes rendered on the page.
const dead = { name: 'Dead Channel (tuned)', g: '#131416', g2: '#1A1C1F', g3: '#25282C', sb: '#1A1C1F', rule: '#2B2F33', sec: '#393E44', rs: '#7E838A', ink: '#E9E7E0', dim: '#B2B0A6', faint: '#7F8287', acc: '#D4A23A', accB: '#96742B', accW: '#2B2515', accWS: '#221F19', wax: '#D97553', waxW: '#2A1816', water: '#86B1B6', waterW: '#18252A', add: '#82B78E', rem: '#E27E66', addW: '#17281B', remW: '#2F1A17', mp: '#4A4E55', mc: '#25282C', itemB: '#3E434A', nAccW: '#2E2716', nWaxW: '#311B17' };
const deadIC = { acc: '#E3B449', accB: '#E3B449', faint: dead.dim, rule: dead.rs, sec: dead.rs, itemB: dead.rs, wax: '#E2855F', water: '#9FC5CA' };
const soot = { name: 'Soot', g: '#111010', g2: '#191715', g3: '#232120', sb: '#191715', rule: '#2B2826', sec: '#353230', rs: '#7B7569', ink: '#ECE6D8', dim: '#B5AC98', faint: '#7F7869', acc: '#CD9A33', accB: '#92702A', accW: '#262013', accWS: '#201C14', wax: '#D47050', waxW: '#261512', water: '#86B0B5', waterW: '#182021', add: '#80B48B', rem: '#E17B63', addW: '#162519', remW: '#2C1813', mp: '#4A443A', mc: '#232120', itemB: '#4A443A', nAccW: '#262013', nWaxW: '#261512' };
const lacq = { name: 'Lacquer', g: '#0A0908', g2: '#11100E', g3: '#1A1816', sb: '#0E0D0B', rule: '#262321', sec: '#312E2A', rs: '#807870', ink: '#F0EADB', dim: '#B8AF9A', faint: '#827A6A', acc: '#DBA63A', accB: '#9C7A2E', accW: '#231D0E', accWS: '#1B1710', wax: '#DC7853', waxW: '#261310', water: '#88B4B9', waterW: '#141F20', add: '#86BA91', rem: '#E58068', addW: '#122218', remW: '#2C1612', mp: '#403A32', mc: '#1A1816', itemB: '#403A32', nAccW: '#231D0E', nWaxW: '#261310' };

// Ladders. Mac in pt; iPhone from the 8 Oct sheet.
const MAC = { ask: 20, stmt: 15, row: 14, body: 13, mono: 12, kw: 11, chip: 11, ctl: 28, pad: '22px 22px 20px' };
const IOS = { ask: 24, stmt: 16, row: 16, body: 13.5, mono: 13.5, kw: 11.5, chip: 11.5, ctl: 44, pad: '18px 16px 16px' };

// ---- atoms -------------------------------------------------------------
const kw = (t, s, text, color) => `<span style="font-family: ${SANS}; font-size: ${s.kw}px; font-weight: 500; letter-spacing: 0.08em; text-transform: uppercase; color: ${color || t.dim}; white-space: nowrap;">${text}</span>`;
const chip = (t, s, text, kind) => { const c = kind === 'accent' ? [t.acc, t.acc] : kind === 'wax' ? [t.wax, t.wax] : kind === 'faint' ? [t.sec, t.dim] : [t.rs, t.ink]; return `<span style="font-family: ${SANS}; font-size: ${s.chip}px; font-weight: 500; letter-spacing: 0.04em; text-transform: uppercase; border: 1px solid ${c[0]}; color: ${c[1]}; border-radius: 3px; padding: 2px 7px; white-space: nowrap; flex: none; line-height: 1.3;">${text}</span>`; };
const mono = (t, s, text, color, size) => `<span style="font-family: ${MONO}; font-size: ${size || s.mono}px; color: ${color || t.ink}; white-space: nowrap;">${text}</span>`;
const ask = (t, s, text) => `<div style="font-family: ${SERIF}; font-weight: 500; font-size: ${s.ask}px; line-height: 1.22; color: ${t.ink}; text-wrap: pretty;">${text}</div>`;
const stmt = (t, s, text) => `<div style="font-family: ${SERIF}; font-weight: 400; font-size: ${s.stmt}px; line-height: 1.45; color: ${t.ink}; text-wrap: pretty;">${text}</div>`;
const body = (t, s, text, color) => `<div style="font-family: ${SANS}; font-size: ${s.body}px; line-height: 1.5; color: ${color || t.ink}; text-wrap: pretty;">${text}</div>`;
const col = (gap, ...k) => `<div style="display: flex; flex-direction: column; gap: ${gap}px;">${k.join('')}</div>`;
const rowf = (gap, ...k) => `<div style="display: flex; gap: ${gap}px; align-items: center; flex-wrap: wrap;">${k.join('')}</div>`;
const between = (a, b, align) => `<div style="display: flex; justify-content: space-between; align-items: ${align || 'baseline'}; gap: 16px;">${a}${b}</div>`;
const eyebrow = (t, s, text, right) => between(kw(t, s, text), right || '', 'center');
const quote = (t, s, inner) => `<div style="display: flex; background: ${t.qW || t.g3}; border-radius: 6px; overflow: hidden;"><div style="width: 2px; background: ${t.qR || t.accB}; flex: none;"></div><div style="padding: 10px 14px; display: flex; flex-direction: column; gap: 6px; min-width: 0; flex: 1;">${inner}</div></div>`;
const fact = (t, s, label, value, color) => between(`<span style="font-family: ${SANS}; font-size: ${s.body}px; color: ${t.ink};">${label}</span>`, mono(t, s, value, color));
const facts = (t, s, title, ...rows) => col(9, kw(t, s, title), ...rows);
const diffCount = (t, s, add, rem, extra) => `<span style="font-family: ${MONO}; font-size: ${s.mono}px; white-space: nowrap;"><span style="color: ${t.add};">+${add}</span> <span style="color: ${t.rem};">−${rem}</span>${extra ? ` <span style="color: ${t.dim};">${extra}</span>` : ''}</span>`;
const notice = (t, s, kind, label, text) => { const c = kind === 'wax' ? [t.nWaxW, t.wax, t.wax] : kind === 'accent' ? [t.nAccW, t.accB, t.acc] : [t.g3, t.rule, t.dim]; return `<div style="display: flex; flex-direction: column; gap: 4px; background: ${c[0]}; border: 1px solid ${c[1]}; border-radius: 6px; padding: 10px 14px;">${kw(t, s, label, c[2])}${body(t, s, text)}</div>`; };
const item = (t, s, inner, pad, gap) => `<div style="border: 1px solid ${t.itemB}; border-radius: 8px; padding: ${pad || '12px 14px'}; display: flex; flex-direction: column; gap: ${gap || 9}px;">${inner}</div>`;
const btn = (t, s, label, kind, grow) => { const base = `display: flex; align-items: center; justify-content: center; height: ${s.ctl}px; border-radius: 6px; font-family: ${SANS}; font-weight: 500; font-size: ${s.body}px; white-space: nowrap; padding: 0 14px; box-sizing: border-box; ${grow ? 'flex: 1;' : 'flex: none;'}`; const k = kind === 'fill' ? `background: ${t.acc}; color: ${t.g2}; border: 1px solid ${t.accB};` : kind === 'wax' ? `border: 1px solid ${t.wax}; color: ${t.wax};` : kind === 'text' ? `color: ${t.dim}; padding: 0 6px;` : `border: 1px solid ${t.rs}; color: ${t.ink};`; return `<div style="${base}${k}">${label}</div>`; };
const more = (t, s) => `<span style="font-family: ${SANS}; color: ${t.dim}; font-weight: 500; font-size: ${s.body}px;">More Actions ▾</span>`;
const chev = (t, open) => `<span style="color: ${t.acc}; font-size: 10px; flex: none; width: 10px;">${open ? '▼' : '▶'}</span>`;
const disc = (t, s, label, sum, open) => `<div style="display: flex; align-items: center; gap: 8px; min-width: 0;">${chev(t, open)}<span style="font-family: ${SANS}; font-size: ${s.body}px; color: ${t.ink};">${label}</span>${sum ? mono(t, s, sum, t.dim) : ''}</div>`;
const link = (t, s, text) => `<span style="font-family: ${SANS}; font-size: ${s.body}px; color: ${t.water}; text-decoration: underline; text-decoration-color: ${t.water}; text-underline-offset: 2px;">${text}</span>`;
const marker = (t, kind) => kind === 'current' ? `<span style="width: 9px; height: 9px; border-radius: 50%; border: 1.5px solid ${t.ink}; box-sizing: border-box; flex: none;"></span>` : kind === 'start' ? `<span style="width: 9px; height: 9px; border-radius: 50%; background: ${t.add}; flex: none;"></span>` : `<span style="width: 9px; height: 9px; border-radius: 50%; border: 1.5px solid ${t.mp}; box-sizing: border-box; flex: none;"></span>`;
const card = (t, s, inner, width) => `<div style="${width ? `width: ${width}px; ` : ''}box-sizing: border-box; background: ${t.g2}; border: 1px solid ${t.itemB}; border-radius: 12px; padding: ${s.pad}; display: flex; flex-direction: column; gap: 18px;">${inner}</div>`;
const rule = t => `<div style="border-top: 1px solid ${t.rule};"></div>`;

// ---- Mac chrome ----------------------------------------------------------
const titleBar = (t, title) => `<div style="height: 52px; display: flex; align-items: center; padding: 0 16px 0 20px; gap: 8px; flex: none;"><span style="width: 12px; height: 12px; border-radius: 50%; background: #FF5F57;"></span><span style="width: 12px; height: 12px; border-radius: 50%; background: #FEBC2E;"></span><span style="width: 12px; height: 12px; border-radius: 50%; background: #28C840;"></span><span style="margin-left: 196px; width: 18px; height: 14px; border: 1.5px solid ${t.dim}; border-radius: 3px; box-sizing: border-box; position: relative;"><span style="position: absolute; left: 4px; top: 0; bottom: 0; width: 1.5px; background: ${t.dim};"></span></span><span style="margin-left: 28px; font-weight: 600; font-size: 14px; color: ${t.dim};">${title}</span><span style="margin-left: auto; font-size: 12.5px; color: ${t.dim};">Updated recently</span></div>`;
const seg = (t, s, items, sel) => `<div style="display: flex; background: ${t.g}; border: 1px solid ${t.sec}; border-radius: 6px; padding: 2px; font-size: 12px; font-weight: 500;">${items.map((it, i) => `<span style="flex: 1; text-align: center; padding: 5px 0; ${i === sel ? `background: ${t.g3}; border-radius: 4px; color: ${t.ink};` : `color: ${t.dim};`}">${it}</span>`).join('')}</div>`;
const sideRow = (t, s, r) => `<div style="display: flex; ${r.sel ? `background: ${t.g3}; border-radius: 8px;` : ''} overflow: hidden; margin-top: ${r.first ? 4 : 0}px;"><div style="width: 3px; background: ${r.sel ? t.acc : 'transparent'}; flex: none;"></div><div style="padding: 10px 12px; display: flex; flex-direction: column; gap: 6px; min-width: 0; flex: 1;"><div style="display: flex; justify-content: space-between; align-items: center; gap: 8px;">${kw(t, s, r.kw)}${chip(t, s, r.chip, r.chipKind)}</div><div style="font-family: ${SERIF}; font-size: ${s.row}px; line-height: 1.4; color: ${t.ink};">${r.title}</div><div style="display: flex; justify-content: space-between; font-family: ${MONO}; font-size: ${s.mono}px; color: ${t.dim};"><span>${r.meta}</span><span>${r.time}</span></div></div></div>`;
const sidebar = (t, s, tab, rows, counts) => `<div style="width: 272px; flex: none; background: ${t.sb}; border-right: 1px solid ${t.rule}; padding: 10px 14px; display: flex; flex-direction: column; gap: 10px; box-sizing: border-box;">${seg(t, s, [`Inbox <span style="font-family: ${MONO}; font-size: 11px;">${counts[0]}</span>`, `Tasks <span style="font-family: ${MONO}; font-size: 11px;">${counts[1]}</span>`], tab)}${seg(t, s, ['Open', 'Resolved', 'All'], 0)}<div style="display: flex; justify-content: space-between; align-items: center; background: ${t.g}; border: 1px solid ${t.sec}; border-radius: 6px; padding: 7px 10px; font-size: 12px; font-weight: 500; color: ${t.ink};"><span>All projects</span><span style="color: ${t.dim}; font-size: 10px;">◇</span></div>${rows.map((r, i) => sideRow(t, s, { ...r, first: i === 0 })).join('')}</div>`;
const mac = (t, title, side, detail, height, width) => `<div style="background: ${t.g}; border: 1px solid ${t.sec}; border-radius: 12px; overflow: hidden; height: ${height || 800}px; width: ${width || 1300}px; display: flex; flex-direction: column; font-size: 13px; color: ${t.ink}; font-family: ${SANS}; position: relative;">${titleBar(t, title)}<div style="display: flex; flex: 1; min-height: 0;">${side}<div style="flex: 1; min-width: 0; padding: 20px 24px 40px; display: flex; flex-direction: column; gap: 18px; align-items: flex-start; overflow: hidden;">${detail}</div></div></div>`;

const inboxRows = (t, s) => [
  { kw: 'Final review', chip: 'Needs decision', chipKind: 'accent', title: 'Is this change ready for final GitHub review?', meta: 'freeside · #731', time: '2h', sel: true },
  { kw: 'Implementation', chip: 'Running', chipKind: 'ink', title: 'Repair the acceptance rig so the publish gate runs unattended.', meta: 'freeside · #724', time: '14m' }
];
const taskRows = (t, s) => [
  { kw: 'Task', chip: 'On hold', chipKind: 'ink', title: 'Repair the acceptance rig so the publish gate runs unattended.', meta: 'freeside · #724', time: '14m', sel: true },
  { kw: 'Task', chip: 'Running', chipKind: 'ink', title: 'Add a retry budget to the credential renewal path.', meta: 'freeside · #731', time: '2h' }
];

// ---- the final-review card -----------------------------------------------
const finalCard = (t, s, width, stacked) => card(t, s, [
  col(10, eyebrow(t, s, 'Ready for final review', chip(t, s, 'Needs decision', 'accent')), ask(t, s, 'Is this change ready for final GitHub review?')),
  col(8, kw(t, s, 'Agent summary (unverified)'), quote(t, s, stmt(t, s, 'Renewal now re-reads the credential file on each 401 and retries once. The reviewer raised no concerns; the acceptance rig passed on the bound head.')), disc(t, s, 'Full Report', 'inv-7f3a · 3 rounds')),
  facts(t, s, 'Facts', fact(t, s, 'Head', '4be1d0a7 on main'), fact(t, s, 'Base', 'main@9c21f0e3'), fact(t, s, 'Review', 'Round 3 · clean'), fact(t, s, 'Change', diffCount(t, s, 412, 88, '· 9 files'))),
  notice(t, s, 'wax', 'Unverified', 'Independent review was waived by the operator. The agent’s summary has not been checked against the diff.'),
  col(8, stacked ? col(8, btn(t, s, 'Approve and Publish', 'fill', true), btn(t, s, 'Return to Agent', 'out', true), btn(t, s, 'Discuss', 'out', true)) : rowf(8, btn(t, s, 'Approve and Publish', 'fill'), btn(t, s, 'Return to Agent', 'out'), btn(t, s, 'Discuss', 'out'), more(t, s))),
  `<div style="display: flex; flex-direction: column; gap: 10px; border-top: 1px solid ${t.rule}; padding-top: 14px;">${disc(t, s, 'Run and Binding Details', 'Head 4be1d0a7 · Base main@9c21f0e3')}${disc(t, s, 'Recorded Context')}</div>`
].join(''), width);

// ---- task timeline ----------------------------------------------------------
const railRow = (t, s, kind, label, time, last) => `<div style="display: flex; gap: 12px; align-items: stretch;"><div style="display: flex; flex-direction: column; align-items: center; width: 9px; flex: none;"><div style="height: 6px; flex: none;"></div>${marker(t, kind)}${last ? '' : `<div style="width: 1px; flex: 1; background: ${t.mc}; margin-top: 3px;"></div>`}</div><div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline; flex: 1; padding-bottom: ${last ? 0 : 14}px;"><span style="font-family: ${SANS}; font-size: ${s.body}px; color: ${kind === 'prior' ? t.dim : t.ink};">${label}</span>${mono(t, s, time, t.dim)}</div></div>`;
const timelineCard = (t, s, width) => card(t, s, [
  col(10, eyebrow(t, s, 'Task timeline', chip(t, s, 'On hold', 'ink')), ask(t, s, 'Repair the acceptance rig so the publish gate runs unattended.')),
  notice(t, s, 'accent', 'Hold', 'Round 1 verification findings block publication. The run waits for a decision in Inbox.'),
  `<div>${link(t, s, 'Review the pull request in Inbox')}</div>`,
  item(t, s, [
    between(rowf(8, `<span style="font-family: ${SERIF}; font-weight: 500; font-size: ${s.stmt}px;">Attempt 2</span>`, chip(t, s, 'Current', 'ink')), link(t, s, 'Open run history'), 'center'),
    col(10, kw(t, s, 'Milestones'), `<div style="display: flex; flex-direction: column;">${railRow(t, s, 'current', 'Review · Round 2 · 2 findings open', 'Oct 30, 10:10 AM')}${railRow(t, s, 'prior', 'Implementation · Pass 2', 'Oct 30, 10:01 AM')}${railRow(t, s, 'prior', 'Review · Round 1 · clean at bound head', 'Oct 30, 9:58 AM')}${railRow(t, s, 'start', 'Attempt started', 'Oct 30, 9:41 AM', true)}</div>`),
    disc(t, s, 'Run Details', 'retry of attempt 1 · Freeside-invoked')
  ].join(''), '14px 16px', 14),
  `<div style="border: 1px solid ${t.itemB}; border-radius: 8px; padding: 9px 14px; display: flex; justify-content: space-between; align-items: center; gap: 12px;"><div style="display: flex; gap: 8px; align-items: center;">${chev(t)}<span style="font-size: ${s.body}px; color: ${t.dim}; white-space: nowrap;">Attempt 1</span>${chip(t, s, 'Failed · Superseded', 'faint')}</div>${mono(t, s, 'Oct 29, 4:58 PM', t.dim)}</div>`,
  col(10, disc(t, s, 'Task Events', '6 recorded · newest Oct 30, 10:10 AM', true), `<div style="padding-left: 18px; display: flex; flex-direction: column;">${railRow(t, s, 'current', 'Review round 2 · findings', 'Oct 30, 10:10 AM')}${railRow(t, s, 'prior', 'Specification approved', 'Oct 29, 1:40 PM')}${railRow(t, s, 'prior', 'Task created from freeside#724', 'Oct 28, 9:02 AM', true)}</div>`),
  rowf(8, btn(t, s, 'Stop Task', 'wax'), btn(t, s, 'Refresh Task Status', 'out'), more(t, s))
].join(''), width);

// ---- New Task sheet -------------------------------------------------------------
const field = (t, s, label, control) => col(6, `<span style="font-family: ${SANS}; font-size: ${s.body}px; color: ${t.ink};">${label}</span>`, control);
const input = (t, s, text, placeholder, tall) => `<div style="min-height: ${tall ? 96 : s.ctl}px; box-sizing: border-box; border: 1px solid ${t.sec}; border-radius: 6px; background: ${t.g}; padding: ${tall ? '8px 10px' : '0 10px'}; display: flex; align-items: ${tall ? 'flex-start' : 'center'}; font-family: ${SANS}; font-size: ${s.body}px; color: ${placeholder ? t.faint : t.ink}; line-height: 1.5;">${text}</div>`;
const select = (t, s, text) => `<div style="height: ${s.ctl}px; box-sizing: border-box; border: 1px solid ${t.sec}; border-radius: 6px; background: ${t.g}; padding: 0 10px; display: flex; align-items: center; justify-content: space-between; font-family: ${SANS}; font-size: ${s.body}px; color: ${t.ink};"><span>${text}</span><span style="color: ${t.dim}; font-size: 10px;">◇</span></div>`;
const sheet = (t, s) => `<div style="position: absolute; inset: 52px 0 0 0; background: rgba(0,0,0,0.38); display: flex; align-items: flex-start; justify-content: center; padding-top: 56px;"><div style="width: 560px; background: ${t.g2}; border: 1px solid ${t.sec}; border-radius: 12px; box-shadow: 0 24px 60px rgba(0,0,0,0.45); display: flex; flex-direction: column; overflow: hidden;"><div style="padding: 22px 22px 18px; display: flex; flex-direction: column; gap: 18px;">${col(8, kw(t, s, 'New task'), ask(t, s, 'What should the agent take on?'))}${field(t, s, 'Project', select(t, s, 'freeside · freeside-ai/freeside'))}${field(t, s, 'Title', input(t, s, 'Add a retry budget to the credential renewal path'))}${field(t, s, 'Instructions', input(t, s, 'What done looks like, which paths are in scope, anything the agent must not touch.', true, true))}${field(t, s, 'Mode', seg(t, s, ['Unattended', 'Ask before publishing', 'Ask at each round'], 1))}${body(t, s, 'The agent works in an isolated checkout. It opens a pull request only after the acceptance rig passes and the review is clean.', t.dim)}</div><div style="border-top: 1px solid ${t.rule}; padding: 12px 22px; display: flex; justify-content: flex-end; gap: 8px; background: ${t.g2};">${btn(t, s, 'Cancel', 'out')}${btn(t, s, 'Submit Task', 'fill')}</div></div></div>`;

// ---- diff reader -------------------------------------------------------------------
const dl = (t, s, kind, text) => { const c = kind === 'add' ? [t.add, t.addW] : kind === 'rem' ? [t.rem, t.remW] : kind === 'hunk' ? [t.dim, 'transparent'] : [t.ink, 'transparent']; const sign = kind === 'add' ? '+' : kind === 'rem' ? '−' : ' '; return `<div style="display: flex; font-family: ${MONO}; font-size: ${s.mono}px; line-height: 1.7; background: ${c[1]}; color: ${c[0]};"><span style="width: 44px; text-align: right; padding-right: 10px; color: ${t.faint}; flex: none; user-select: none;">${kind === 'hunk' ? '' : text.n}</span><span style="width: 14px; flex: none;">${sign}</span><span style="white-space: pre;">${kind === 'hunk' ? text : text.s}</span></div>`; };
const diffReader = (t, s, width) => card(t, s, [
  col(10, eyebrow(t, s, 'Diff · final review', diffCount(t, s, 412, 88, '· 9 files')), ask(t, s, 'Is this change ready for final GitHub review?')),
  `<div style="display: flex; gap: 8px; align-items: center; flex-wrap: wrap;">${chip(t, s, '9 files', 'ink')}${mono(t, s, 'daemon/internal/signet/renew.go', t.ink)}${mono(t, s, '2 of 9', t.dim)}</div>`,
  `<div style="border: 1px solid ${t.itemB}; border-radius: 8px; overflow: hidden; padding: 6px 0;">${[
    dl(t, s, 'hunk', `@@ −214,7 +214,12 @@ func (r *Renewer) renew(ctx context.Context) error`),
    dl(t, s, 'ctx', { n: 214, s: '  tok, err := r.store.Read(ctx)' }),
    dl(t, s, 'ctx', { n: 215, s: '  if err != nil {' }),
    dl(t, s, 'rem', { n: 216, s: '    return err' }),
    dl(t, s, 'add', { n: 216, s: '    return fmt.Errorf("read credential: %w", err)' }),
    dl(t, s, 'ctx', { n: 217, s: '  }' }),
    dl(t, s, 'add', { n: 218, s: '  resp, err := r.client.Do(req.WithContext(ctx))' }),
    dl(t, s, 'add', { n: 219, s: '  if err == nil && resp.StatusCode == http.StatusUnauthorized {' }),
    dl(t, s, 'add', { n: 220, s: '    // Re-read once: the file may have rotated under us.' }),
    dl(t, s, 'add', { n: 221, s: '    tok, err = r.store.Read(ctx)' }),
    dl(t, s, 'add', { n: 222, s: '    resp, err = r.client.Do(req.WithContext(ctx))' }),
    dl(t, s, 'add', { n: 223, s: '  }' }),
    dl(t, s, 'rem', { n: 218, s: '  return r.apply(tok)' }),
    dl(t, s, 'add', { n: 224, s: '  return r.apply(tok, resp)' }),
    dl(t, s, 'ctx', { n: 225, s: '}' })
  ].join('')}</div>`,
  disc(t, s, '2 Later Hunks', '+31 −4'),
  rowf(8, btn(t, s, 'Next File', 'out'), btn(t, s, 'Open in Editor', 'out'), more(t, s))
].join(''), width);

// ---- menu-bar panel -----------------------------------------------------------------
const menuBar = (t, s) => `<div style="display: flex; flex-direction: column; gap: 10px; align-items: flex-start;"><div style="height: 28px; display: flex; align-items: center; gap: 14px; padding: 0 12px; background: ${t.g2}; border: 1px solid ${t.rule}; border-radius: 6px; font-family: -apple-system, 'SF Pro Text', 'Helvetica Neue', sans-serif; font-size: 13px; color: ${t.ink};"><span style="display: inline-flex; align-items: center; gap: 4px; background: ${t.g3}; border-radius: 5px; padding: 3px 7px; position: relative;"><img src="assets/key/freeside-key-dusk.svg" alt="" style="height: 16px; width: auto;" /><span style="position: absolute; right: 3px; top: 3px; width: 7px; height: 7px; border-radius: 50%; background: ${t.acc}; box-shadow: 0 0 0 1.5px ${t.g2};"></span></span><span>Fri Oct 30 &nbsp;10:12 AM</span></div>
  <div style="width: 340px; background: ${t.g2}; border: 1px solid ${t.sec}; border-radius: 12px; padding: 8px; display: flex; flex-direction: column; gap: 3px; font-size: 13px; color: ${t.ink}; box-shadow: 0 14px 34px rgba(0,0,0,0.32); font-family: ${SANS}; box-sizing: border-box;">
    <div style="display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 6px 10px; border-radius: 6px;"><span>Open Freeside</span></div>
    <div style="display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 6px 10px; border-radius: 6px; background: ${t.g3};"><span>Show Inbox</span><span style="display: flex; gap: 8px; align-items: center;">${mono(t, s, '3', t.dim)}${chip(t, s, '1 Needs decision', 'accent')}</span></div>
    <div style="border-top: 1px solid ${t.rule}; margin: 4px 4px;"></div>
    <div style="padding: 6px 10px 6px; display: flex; flex-direction: column; gap: 10px;">
      ${between(kw(t, s, 'Daemon'), chip(t, s, 'Running', 'ink'), 'center')}
      <div style="display: flex; flex-direction: column; gap: 3px;"><div>The supervised daemon is running on this Mac.</div>${mono(t, s, 'freesided 0.9.4 · since 8:12 AM', t.dim)}</div>
      ${between(kw(t, s, 'Tasks'), mono(t, s, '1 running · 1 on hold', t.dim), 'center')}
    </div>
    <div style="display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 6px 10px; border-radius: 6px; box-shadow: inset 0 0 0 1px ${t.acc};"><span>New Task…</span>${mono(t, s, '⌘N', t.dim)}</div>
    <div style="display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 6px 10px; border-radius: 6px;"><span>Stop Daemon</span></div>
    <div style="border-top: 1px solid ${t.rule}; margin: 4px 4px;"></div>
    <div style="display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 6px 10px; border-radius: 6px;"><span>Quit Freeside</span>${mono(t, s, '⌘Q', t.dim)}</div>
  </div></div>`;

// ---- iPhone ---------------------------------------------------------------------------------
const phone = (t, s, title, inner, backLabel, trailing) => `<div style="width: 390px; background: ${t.g}; border: 1px solid ${t.sec}; border-radius: 44px; padding: 14px 16px 34px; box-sizing: border-box; display: flex; flex-direction: column; gap: 14px; font-size: ${s.body}px; line-height: 1.5; color: ${t.ink}; font-family: ${SANS}; min-height: 844px; overflow: hidden;"><div style="display: flex; justify-content: space-between; align-items: center; padding: 0 12px; height: 28px; font-family: -apple-system, 'SF Pro Text', 'Helvetica Neue', sans-serif; font-size: 15px; font-weight: 600; color: ${t.ink};"><span>9:41</span><span style="display: flex; gap: 5px; align-items: center;"><span style="font-size: 12px; letter-spacing: -1px;">▪▪▪▪</span><span style="width: 22px; height: 11px; border: 1.5px solid ${t.ink}; border-radius: 3px; box-sizing: border-box; position: relative;"><span style="position: absolute; left: 1.5px; top: 1.5px; bottom: 1.5px; width: 14px; background: ${t.ink};"></span></span></span></div><div style="display: grid; grid-template-columns: 1fr auto 1fr; align-items: center; gap: 6px; padding: 0 2px;"><span style="color: ${t.acc}; font-weight: 500; font-size: 15px;">${backLabel || ''}</span><span style="font-family: -apple-system, 'SF Pro Text', 'Helvetica Neue', sans-serif; font-weight: 600; font-size: 17px; color: ${t.ink};">${title}</span><span style="color: ${t.acc}; font-weight: 500; font-size: 15px; text-align: right;">${trailing || ''}</span></div>${inner}</div>`;
const phoneRow = (t, s, r) => `<div style="display: flex; flex-direction: column; gap: 6px; padding: 12px 14px; background: ${t.g2}; border: 1px solid ${t.itemB}; border-radius: 10px;"><div style="display: flex; justify-content: space-between; align-items: center; gap: 8px;">${kw(t, s, r.kw)}${chip(t, s, r.chip, r.chipKind)}</div><div style="font-family: ${SERIF}; font-size: ${s.row}px; line-height: 1.4; color: ${t.ink};">${r.title}</div><div style="display: flex; justify-content: space-between; font-family: ${MONO}; font-size: ${s.mono}px; color: ${t.dim};"><span>${r.meta}</span><span>${r.time}</span></div></div>`;
const phoneInbox = (t, s) => phone(t, s, 'Inbox · 3', col(14, seg(t, s, ['Open', 'Resolved', 'All'], 0), col(10, ...inboxRows(t, s).map(r => phoneRow(t, s, { ...r, sel: false })), phoneRow(t, s, { kw: 'Specification', chip: 'Needs decision', chipKind: 'accent', title: 'Approve the specification for write-once command identity?', meta: 'freeside · #718', time: '1d' }))), '', 'Tasks');
const phoneCard = (t, s) => phone(t, s, 'Final review', finalCard(t, s, null, true).replace(`width: `, 'width: '), '‹ Inbox', '');

// ---- canvas chrome ----------------------------------------------------------------------------
const cap = (text) => `<div style="font-family: ${MONO}; font-size: 11px; letter-spacing: 0.14em; text-transform: uppercase; color: #6E6450; line-height: 1.6;">${text}</div>`;
const frame = (label, inner, width) => `<div style="display: flex; flex-direction: column; gap: 12px; flex: none; ${width ? `width: ${width}px;` : ''}" data-screen-label="${label.replace(/<[^>]+>/g, '')}">${cap(label)}${inner}</div>`;
const prose = (html, max) => `<div style="max-width: ${max || 760}px; font-size: 13px; line-height: 1.6; color: #4A4234; font-family: ${SANS}; text-wrap: pretty;">${html}</div>`;
const h1 = (text) => `<div style="font-family: ${SERIF}; font-weight: 500; font-size: 26px; color: #2B2416; margin-bottom: 8px;">${text}</div>`;
const h2 = (text, sub) => `<div style="display: flex; align-items: baseline; gap: 14px; border-bottom: 1px solid #C9BFA2; padding-bottom: 10px; max-width: 2800px;"><span style="font-family: ${SERIF}; font-weight: 500; font-size: 22px; color: #2B2416;">${text}</span><span style="font-size: 12.5px; color: #6E6450;">${sub || ''}</span></div>`;
const band = (title, sub, ...k) => `<div style="display: flex; flex-direction: column; gap: 22px;">${h2(title, sub)}<div style="display: flex; gap: 48px; align-items: flex-start; flex-wrap: wrap; row-gap: 64px;">${k.join('')}</div></div>`;
const swatchRow = (t) => `<div style="display: flex; gap: 0; border: 1px solid #C9BFA2; border-radius: 6px; overflow: hidden; width: 360px;">${['g', 'g2', 'g3', 'rule', 'sec', 'rs'].map(k => `<div style="flex: 1; height: 44px; background: ${t[k]}; display: flex; align-items: flex-end; justify-content: center; padding-bottom: 4px;"><span style="font-family: ${MONO}; font-size: 9px; color: ${t.dim};">${k}</span></div>`).join('')}<div style="width: 60px; height: 44px; background: ${t.g}; display: flex; align-items: center; justify-content: center;"><span style="width: 18px; height: 18px; border-radius: 3px; background: ${t.acc};"></span></div></div>`;

// ---- critique & tokens ------------------------------------------------------------------------------
const crit = (title, t, verdict, items) => `<div style="width: 560px; display: flex; flex-direction: column; gap: 12px; flex: none;"><div style="display: flex; align-items: center; gap: 12px;"><span style="width: 28px; height: 28px; border-radius: 6px; background: ${t.g}; border: 1px solid #C9BFA2; display: inline-flex; align-items: center; justify-content: center;"><span style="width: 10px; height: 10px; border-radius: 2px; background: ${t.acc};"></span></span><span style="font-family: ${SERIF}; font-weight: 500; font-size: 20px; color: #2B2416;">${title}</span><span style="font-family: ${SANS}; font-size: 11px; font-weight: 500; letter-spacing: 0.08em; text-transform: uppercase; color: #6E6450; margin-left: auto;">${verdict}</span></div>${swatchRow(t)}<div style="display: flex; flex-direction: column; gap: 10px; font-size: 13px; line-height: 1.55; color: #4A4234; font-family: ${SANS};">${items.map(([k, v]) => `<div style="display: grid; grid-template-columns: 96px 1fr; gap: 12px;"><span style="font-family: ${SANS}; font-size: 11px; font-weight: 500; letter-spacing: 0.08em; text-transform: uppercase; color: #6E6450; padding-top: 3px;">${k}</span><span>${v}</span></div>`).join('')}</div></div>`;

const TOKENS = [
  ['ground', 'g', 'fill'], ['ground2', 'g2', 'fill'], ['ground3', 'g3', 'fill'], ['sidebarGround', 'sb', 'fill'],
  ['rule', 'rule', 'hair'], ['secondaryBorder', 'sec', 'hair'], ['itemBorder', 'itemB', 'hair'], ['ruleStrong', 'rs', 'hair'], ['accentBorder', 'accB', 'hair'], ['milestonePrior', 'mp', 'deco'], ['milestoneConnector', 'mc', 'deco'],
  ['ink', 'ink', 'text'], ['inkDim', 'dim', 'text'], ['inkFaint', 'faint', 'disabled'], ['accentText', 'acc', 'text'], ['waxText', 'wax', 'text'], ['waterText', 'water', 'text'], ['diffAdd', 'add', 'text'], ['diffRemove', 'rem', 'text'],
  ['accentWash', 'accW', 'wash'], ['accentWashSoft', 'accWS', 'wash'], ['noticeAccentWash', 'nAccW', 'wash'], ['waxWash', 'waxW', 'wash'], ['noticeWaxWash', 'nWaxW', 'wash'], ['waterWash', 'waterW', 'wash'], ['diffAddWash', 'addW', 'wash'], ['diffRemoveWash', 'remW', 'wash']
];
const ICKEYS = { acc: 'acc', accB: 'accB', faint: 'faint', rule: 'rule', sec: 'sec', itemB: 'itemB', wax: 'wax', water: 'water' };
const sw = (hex) => `<span style="display: inline-flex; align-items: center; gap: 6px; font-family: ${MONO}; font-size: 12px; color: #2B2416;"><span style="width: 14px; height: 14px; border-radius: 3px; background: ${hex}; border: 1px solid #C9BFA2; flex: none;"></span>${hex}</span>`;
const ratio = (v, floor) => { const pass = v >= floor; return `<span style="font-family: ${MONO}; font-size: 12px; color: ${pass ? '#2B2416' : '#8A2D1C'}; white-space: nowrap;">${f2(v)}${pass ? '' : ' ✕'}</span>`; };
const tokenTable = (t0, t, ic, notes) => {
  const head = ['Token', 'Given', 'Tuned', 'on ground', 'on ground2', 'Increased Contrast', 'IC on ground2', 'Note'];
  const rows = TOKENS.map(([name, k, kind]) => {
    const floor = kind === 'text' ? 4.5 : (kind === 'hair' || kind === 'disabled') ? 3 : 0;
    const changed = t0[k].toUpperCase() !== t[k].toUpperCase();
    const g = floor ? ratio(CR(t[k], t.g), floor) : (kind === 'wash' ? ratio(CR(t[k], t.g), 0) : '');
    const g2 = floor ? ratio(CR(t[k], t.g2), floor) : (kind === 'wash' || kind === 'fill' || kind === 'deco' ? ratio(CR(t[k], t.g2), 0) : '');
    const icv = ic[ICKEYS[k]];
    const icCell = icv ? sw(icv) : '<span style="color: #94896E;">—</span>';
    const icR = icv ? ratio(CR(icv, t.g2), kind === 'text' ? 4.5 : 3) : '';
    const note = (notes || NOTES)[name] || '';
    return `<tr style="border-top: 1px solid #D6CDB2;"><td style="padding: 7px 10px; font-family: ${MONO}; font-size: 12px; color: #2B2416;">${name}</td><td style="padding: 7px 10px;">${sw(t0[k])}</td><td style="padding: 7px 10px; ${changed ? 'background: #E9DFC2;' : ''}">${sw(t[k])}</td><td style="padding: 7px 10px;">${g}</td><td style="padding: 7px 10px;">${g2}</td><td style="padding: 7px 10px;">${icCell}</td><td style="padding: 7px 10px;">${icR}</td><td style="padding: 7px 10px; font-size: 12px; line-height: 1.45; color: #4A4234; max-width: 380px;">${note}</td></tr>`;
  });
  return `<table style="border-collapse: collapse; background: #F3EEE1; border: 1px solid #C9BFA2; border-radius: 8px; font-family: ${SANS}; font-size: 13px;"><thead><tr>${head.map(h => `<th style="text-align: left; padding: 9px 10px; font-family: ${SANS}; font-size: 11px; font-weight: 500; letter-spacing: 0.08em; text-transform: uppercase; color: #6E6450;">${h}</th>`).join('')}</tr></thead><tbody>${rows.join('')}</tbody></table>`;
};
const NOTES = {
  ground3: 'Lifted one step (1.12 → 1.15 over ground2) so the selected nav row and the quote block read without a hairline. Hover and <code style="font-family: ${MONO}; font-size: 12px;">segmentSelected</code> follow.',
  secondaryBorder: 'Lifted so the segmented control and sheet outline survive on sidebarGround. Still decorative (<3:1); promotes to ruleStrong under IC.',
  itemBorder: 'No longer milestonePrior. A 1.5:1 hairline is the Straylight value; on graphite the card edge needs the same step. Promotes to ruleStrong under IC.',
  ruleStrong: 'Nudged off the blue: #7B828A → #7E838A. 4.47:1 on ground2, 3.9:1 on ground3 — an outlined control stays a button on hover.',
  accentBorder: 'As given. 3.9:1 on ground2 clears the hairline floor; the filled button’s outline and the quote rule.',
  milestonePrior: 'Lifted; the prior ring sat at 1.8:1 and vanished beside a 1.7:1 connector. Decoration, paired with a label.',
  inkFaint: 'As given, pulled a hair toward neutral. Disabled floor is 3:1; it clears 4.4. IC promotes to inkDim.',
  accentText: 'As given. The one memorable move: tawny at 7.3:1 on neutral graphite. IC takes the brighter cut.',
  waxText: 'As given. 5.4:1 on ground2, 4.8:1 on ground3, so a wax outline on a hovered control keeps the text floor.',
  waterText: 'As given. Teal is finally a hue, not a tint of dim ink — links separate from body copy.',
  accentWash: 'Warmed and lifted; on a cool ground a warm wash reads as colour, so it needs less lightness than it did on umber.',
  noticeAccentWash: 'Lifted with accentWash; the hold notice is bordered, the wash only has to tint.',
  waxWash: 'Lifted one step; on graphite the given value read as a dirty grey, not a wash.',
  noticeWaxWash: 'Lifted with waxWash. waxText on it: ' + f2(CR('#D97553', '#311B17')) + ':1.',
  waterWash: 'Cooled slightly toward the water hue so the in-progress wash is not mistaken for ground3.',
  diffAddWash: 'A hair lifted; diffAdd on it ' + f2(CR('#82B78E', '#17281B')) + ':1.',
  diffRemoveWash: 'A hair lifted; diffRemove on it ' + f2(CR('#E27E66', '#2F1A17')) + ':1.',
  accentWashSoft: 'Press fill. Must sit between ground2 and ground3; it does.',
  milestoneConnector: 'Follows ground3.',
  sidebarGround: 'As given: ground2, same as Straylight.'
};


const wrapDoc = (title, intro, bands) => `<!DOCTYPE html>
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
  <div style="display: flex; flex-direction: column; gap: 8px;">${h1(title)}${prose(intro)}</div>
  ${bands.join('\n  ')}
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
