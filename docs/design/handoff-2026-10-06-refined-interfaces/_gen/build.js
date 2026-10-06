// Assembles "Interfaces - Refined - 6 Oct 2026.dc.html" from _gen/lib.js and every _gen/group*.js present.
const lib = await readFile('_gen/lib.js');
const files = (await ls('_gen')).filter(f => /^group\d+\.js$/.test(f)).sort();
let src = lib + '\n';
for (const f of files) src += (await readFile('_gen/' + f)) + '\n';
const BANDS = [];
new Function('BANDS', src)(BANDS);
const T = new Function(lib + '\nreturn {DAY, DUSK, SERIF, MONO};')();
const esc = (s) => s;
const bandsHtml = BANDS.map(b => `
<!-- ═══════════ ${b.title} ═══════════ -->
<div style="display: flex; flex-direction: column; gap: 22px;" data-screen-label="${b.title}">
  <div style="display: flex; align-items: baseline; gap: 14px; border-bottom: 1px solid #C9BFA2; padding-bottom: 10px; max-width: 2400px;"><span style="font-family: ${T.SERIF}; font-weight: 500; font-size: 22px; color: #2B2416;">${b.title}</span><span style="font-size: 12.5px; color: #94896E;">${b.note || ''}</span></div>
  <sc-if value="{{ showDay }}" hint-placeholder-val="{{ true }}"><div style="display: flex; gap: 48px; align-items: flex-start; flex-wrap: wrap; row-gap: 72px;">${b.frames(T.DAY).join('')}</div></sc-if>
  <sc-if value="{{ showDusk }}" hint-placeholder-val="{{ true }}"><div style="display: flex; gap: 48px; align-items: flex-start; flex-wrap: wrap; row-gap: 72px; margin-top: 24px;">${b.frames(T.DUSK).join('')}</div></sc-if>
</div>`).join('\n');

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
<div style="position: relative; box-sizing: border-box; padding: 48px 48px 120px; display: flex; flex-direction: column; gap: 110px; font-family: 'IBM Plex Sans', sans-serif; color: #2B2416; min-width: 5000px;">

<div style="display: flex; gap: 40px; align-items: flex-start; max-width: 2400px;" data-screen-label="Intro and legend">
  <div style="flex: 1; font-size: 12.5px; line-height: 1.6; color: #675D49;">
    <div style="font-family: ${T.SERIF}; font-weight: 500; font-size: 26px; color: #2B2416; margin-bottom: 8px;">Interfaces, refined — 6 Oct 2026</div>
    Every remaining Freeside surface drawn as a complete screen under the design-language rules of the <a href="Design Language Survey - 6 Oct 2026.dc.html">6 Oct survey</a> (R0–R33, the emphasis ladder, the 4b scale and gap ladder), beside the as-built frames on <a href="Interfaces - 6 Oct 2026.dc.html">Interfaces, 6 Oct</a>. Each frame is at real size; its caption names platform · surface · the rules applied · the count of owner decisions still open. Where the survey draws a surface, the frame matches it; nothing here re-decides a drawn treatment. Content is lifted from SURFACES.md and the SwiftUI sources — no fact, action or module that the source does not have. Day first; dusk by the token mirror plus the lifted cuts (quote wash #292117, quote rule #8A6A26, item border #4A3F2C, notice washes #2C2412 / #2E1812, hover #2F261A). Native chrome (window title, toolbar, inspector toggle, popups, context menus, pickers) stays native and is drawn in the system face.
  </div>
  <div style="width: 560px; flex: none; background: #F3EEE1; border: 1px solid #D6CDB2; border-radius: 12px; padding: 18px 20px; display: flex; flex-direction: column; gap: 10px; font-size: 12.5px; line-height: 1.55; color: #675D49;">
    <div style="display: flex; align-items: center; gap: 8px;"><span style="display: inline-flex; align-items: center; gap: 6px; background: #2B2416; color: #EDE7D6; border-radius: 4px; padding: 3px 8px; font-family: ${T.MONO}; font-size: 11px; font-weight: 600; letter-spacing: 0.06em;"><span style="color: #E0AE46;">◆</span>N OPEN</span><span style="font-family: ${T.SERIF}; font-size: 16px; color: #2B2416;">Open owner decisions</span></div>
    <div>A ◆ on a frame counts the decisions from the survey’s Parts 5, 6 and 7 that the frame draws a default for; each is captioned under the frame with its survey row. None is settled by being drawn. Four rule-level ◆ from Part 3 apply to every frame and are not re-counted: <b style="color: #2B2416;">R2</b> one disclosure shape · <b style="color: #2B2416;">R3</b> pills become disclosures, away is a link · <b style="color: #2B2416;">R5</b> the quote on the agent summary D07/D08 drew spaced · <b style="color: #2B2416;">R7</b> one claim marker. R10 and R28 were decided on 6 Oct.</div>
    <div>Not settled here by design: what <i>approve</i> means on an observation-only dispute (7.4’s relabel waits on the contract — the button keeps “Approve”), and the native chrome listed under System Chrome in SURFACES.md.</div>
    <div>Part 8 of the survey is the ledger of every ◆ on this canvas with a blank Decided column.</div>
  </div>
</div>

${bandsHtml}

</div>
</x-dc>
<script type="text/x-dc" data-dc-script data-props="{&quot;showDay&quot;: {&quot;editor&quot;: &quot;boolean&quot;, &quot;default&quot;: true, &quot;tsType&quot;: &quot;boolean&quot;, &quot;section&quot;: &quot;Canvas&quot;}, &quot;showDusk&quot;: {&quot;editor&quot;: &quot;boolean&quot;, &quot;default&quot;: true, &quot;tsType&quot;: &quot;boolean&quot;, &quot;section&quot;: &quot;Canvas&quot;}}">
class Component extends DCLogic {
  renderVals() {
    return { showDay: this.props.showDay ?? true, showDusk: this.props.showDusk ?? true };
  }
}
</script>
</body>
</html>
`;
await saveFile('Interfaces - Refined - 6 Oct 2026.dc.html', doc);
log('bands', BANDS.map(b => b.id).join(', '), 'chars', doc.length);
