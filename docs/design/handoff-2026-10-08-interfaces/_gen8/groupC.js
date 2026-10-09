// Group C · the remaining Mac surfaces on the Mac ladder: nine card types, run timeline, 1,000pt two-column with inspector, pairing, readers, accessibility.
const CARDS8 = {};
CARDS8.spec = (t) => [
  head(t, 'Spec approval', 'Approve this specification for implementation?', chip(t, 'High', 'accent')),
  col(8, kwInfo(t, 'Agent summary (unverified)'), quote(t, stmt(t, 'Revision 1 keeps the existing migration order and narrows the rollback step to the compatibility check; one decision is open on reader ordering.'))),
  col(t.S.mod, kw(t, 'Specification'), item(t, `<div style="display: flex; justify-content: space-between; align-items: baseline; gap: 12px;"><div style="display: flex; flex-direction: column; gap: 2px;"><span style="font-size: ${t.S.label}px;">Revision 1</span>${dimText(t, 'Bound by the daemon to this approval')}</div>${link(t, 'Open Reader')}</div>`, '12px 14px')),
  col(10, kw(t, 'Conversation'), msgYou(t, 'Can the revised spec preserve the existing migration order?'), msgAgent(t, 'Yes. The revision keeps the order and narrows the rollback step. First, migrate the stored rows while the old API shape remains available. Then enable the updated reader after the migration has completed.'), reply(t)),
  actions(t, row(btnOut(t, 'Approve'), btnOut(t, 'Request Changes')), more(t)),
  folds(t, disc(t, 'Source and Original Report', 'inv-agent-spec-approval'), disc(t, 'Details', 'Specification digest · item version'))
].join('');
CARDS8.question = (t) => { const opt = (n, label, cons, rec) => quote(t, `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${kw(t, 'Option ' + n)}${rec ? kw(t, 'Agent recommends', t.accT) : ''}</div>${stmt(t, label, t.S.stmt, 500)}<div>${cons}</div>`); return [
  head(t, 'Agent question (unverified)', 'Which order should the migration run in?', '', true),
  quote(t, dimText(t, 'The implementation stopped at the first migration step because the two orders change which clients break during the transition.')),
  col(t.S.mod, opt(1, 'Store first, then API', 'Existing rows migrate before any client can read them, and the API keeps the old shape for one release.', true), opt(2, 'API first, then store', 'Clients move immediately, and the daemon reads both shapes until the store migration lands.')),
  actions(t, row(btnFill(t, 'Answer and Retry'), btnOut(t, 'Answer Without Retry'), btnOut(t, 'Discuss')), more(t)),
  folds(t, disc(t, 'Run and Binding Details', 'Implementation · blocked on this answer'), disc(t, 'Recorded Context'), disc(t, 'Source and Original Report', 'inv-agent-question-731'))
].join(''); };
CARDS8.dispute = (t) => [
  head(t, 'Review dispute', 'How should this review dispute be resolved?'),
  dimText(t, 'The agent disputes a shadow-reviewer finding; the run is held until the dispute is resolved.'),
  col(t.S.mod, kw(t, 'Positions'), col(6, kwInfo(t, 'Reviewer (unverified)'), quote(t, stmt(t, 'P1 shadow finding at daemon/main.go:42: the sampled reviewer found a blocking defect.'))), col(6, kw(t, 'Agent (unverified)'), quote(t, stmt(t, 'The path is unreachable in production; the finding should be withdrawn.')))),
  actions(t, row(btnOut(t, 'Approve'), btnOut(t, 'Discuss')), more(t)),
  folds(t, disc(t, 'Run and Binding Details', 'attempt 2 · round 3 · 1 disputed'), disc(t, 'Source and Supporting Details', 'two claims · digests'), disc(t, 'Details'))
].join('');
CARDS8.dimin = (t) => { const fix = (label, cons, id) => quote(t, stmt(t, label, t.S.stmt, 500) + dimText(t, `${cons ? cons + ' · ' : ''}<span style="font-family: ${MONO}; font-size: ${t.S.sum}px;">${id}</span>`, t.S.sum));
  const yr = (n, nw, rc, total) => `<div style="display: flex; flex-direction: column; gap: 4px;"><div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;"><span style="font-size: ${t.S.label}px;">Round ${n}</span><span style="font-family: ${MONO}; font-size: ${t.S.mono}px; white-space: nowrap;"><span style="color: ${t.accT};">${nw} new</span> · <span style="color: ${t.waxT};">${rc} recurring</span></span></div><div style="display: flex; height: 6px; width: ${Math.round(total / 8 * 100)}%; border-radius: 3px; overflow: hidden;">${nw ? `<span style="flex: ${nw}; background: ${t.acc};"></span>` : ''}${rc ? `<span style="flex: ${rc}; background: ${t.waxT};"></span>` : ''}</div></div>`;
  return [
    head(t, 'Review diminishing returns', 'Stop review here?'),
    facts(t, [fact(t, 'Verdict', 'Simplification drift'), fact(t, 'Confidence', 'High')], 'Drift audit'),
    col(8, kwInfo(t, 'Fixes to undo (unverified)'), quote(t, stmt(t, 'Round 3 traded a guard clause for a comment; rounds 2–4 reverted each other’s renames.')), fix('Restore the nil guard in service.go:214', 'The comment does not prevent the panic', 'review-finding-21'), fix('Revert the rename to applyOnce', 'It hides the retry semantics', 'review-finding-23'), disc(t, '3 More Fixes', '5 in total')),
    col(t.S.mod, kw(t, 'Review yield'), yr(1, 6, 2, 8), yr(2, 2, 3, 5), yr(3, 1, 1, 2), yr(4, 0, 1, 1), `<div style="display: flex; gap: 14px; font-size: ${t.S.sum}px; color: ${t.dim};"><span style="display: flex; gap: 6px; align-items: center;"><span style="width: 7px; height: 7px; border-radius: 50%; background: ${t.acc};"></span>new findings</span><span style="display: flex; gap: 6px; align-items: center;"><span style="width: 7px; height: 7px; border-radius: 50%; background: ${t.waxT};"></span>recurring</span></div>`),
    facts(t, [fact(t, 'Cost so Far', '$41.20'), fact(t, 'Diff Growth', `<span style="font-family: ${MONO}; font-size: ${t.S.mono}px; white-space: nowrap;">${diff(t, 312, 40)} → ${diff(t, 388, 61)}</span>`)], 'Facts'),
    actions(t, row(btnOut(t, 'Finish Now'), btnOut(t, 'Apply Then Finish'), btnOut(t, 'Discuss')), more(t), dimText(t, 'Continue Under Policy, in More Actions, runs the simplification round and undoes the fixes above.', t.S.sum)),
    folds(t, disc(t, 'Recorded Context'), disc(t, 'Details', 'audit digest · run · round'))
  ].join(''); };
CARDS8.execfail = (t) => [
  head(t, 'Execution failure', 'Choose a recovery for this failed execution?', chip(t, 'Urgent', 'wax')),
  dimText(t, 'The implementation attempt exited before publishing; nothing from it is on the PR.'),
  facts(t, [fact(t, 'Outcome', 'Exited non-zero'), fact(t, 'Stage', 'Implementation · Pass 2'), fact(t, 'Invocation', 'inv-9c21f0e3')], 'Failure'),
  col(8, kwInfo(t, 'Diagnostic (unverified)'), quote(t, stmt(t, 'The container ran out of memory while compiling the generated schema; a larger ward or a narrower generation target should let the attempt complete.'))),
  col(t.S.mod, kw(t, 'Stages'), railRow(t, 'wax', 'Implementation · Pass 2', 'Sep 12, 8:12 AM', 'Execution failed'), railRow(t, 'prior', 'Implementation · Pass 1', 'Sep 11, 4:58 PM'), railRow(t, 'prior', 'Specification · Round 1', 'Sep 11, 2:02 PM')),
  actions(t, row(btnFill(t, 'Retry'), btnOut(t, 'Retry With…'), btnOut(t, 'Discuss')), more(t)),
  folds(t, disc(t, 'Recorded Context'), disc(t, 'Run and Binding Details', 'attempt 2 · export none'), disc(t, 'Details', 'run · invocation · digest'))
].join('');
CARDS8.taskprop = (t) => [
  head(t, 'Task proposal', 'Start this proposed task?'),
  col(8, kwInfo(t, 'Proposal (unverified)'), quote(t, stmt(t, 'Task names are free text and collide across projects; binding each to the operator who set it and rejecting duplicates within a project removes the ambiguity the inbox shows today.'))),
  facts(t, [fact(t, 'Project', 'owner/repo'), fact(t, 'Source', 'owner/repo#731'), fact(t, 'Proposed Name', 'Bound operator task names'), fact(t, 'Declared Paths', '4')], 'Facts'),
  actions(t, row(btnFill(t, 'Start'), btnOut(t, 'Start With Changes'), btnOut(t, 'Discuss')), more(t)),
  folds(t, disc(t, 'Recorded Context'), disc(t, 'Details', 'proposal digest · declaration · item version'))
].join('');
CARDS8.effect = (t) => [
  head(t, 'Effect proposal', 'Close the source issue when this merges?'),
  dimText(t, 'The issue could not be closed automatically: its closure keyword names a PR the daemon does not own.'),
  stmt(t, `Merging head <span style="font-family: ${MONO}; font-size: ${t.S.mono + 1}px;">4be1d0a7</span> into main would close owner/repo#724, the issue this task was taken from.`),
  facts(t, [fact(t, 'Target Issue', link(t, 'owner/repo#724')), fact(t, 'Reference', 'Verified'), fact(t, 'Closure Flag', 'From intake'), fact(t, 'Binds to', 'head 4be1d0a7 · main@9c21f0e3')], 'Facts'),
  actions(t, row(btnFill(t, 'Approve'), btnOut(t, 'Approve With Changes'), btnOut(t, 'Discuss')), more(t)),
  folds(t, disc(t, 'Details', 'proposal digest · item version'))
].join('');
CARDS8.health = (t) => [
  head(t, 'System health', 'Acknowledge this finding?', chip(t, 'Urgent', 'wax')),
  stmt(t, 'Backup encryption key is unreadable; encrypted backups are paused until it is restored or re-enrolled.'),
  facts(t, [fact(t, 'Diagnostic', 'backup_key_unreadable'), fact(t, 'Impairs', 'Encrypted backups'), fact(t, 'Posture', chip(t, 'Degraded', 'accent')), fact(t, 'Re-enrollment', 'Required · machine key')], 'Facts'),
  actions(t, row(btnFill(t, 'Acknowledge'), btnOut(t, 'Discuss')), more(t)),
  folds(t, disc(t, 'Recorded Context'), disc(t, 'Details', 'finding id · observed at'))
].join('');
CARDS8.blocked = (t) => [
  head(t, 'Blocked', 'Waiting on specification approval.'),
  facts(t, [fact(t, 'Waiting', '3h'), fact(t, 'Blocked on', link(t, 'Spec approval · #731'))], 'Facts'),
  folds(t, disc(t, 'Details', 'reason · wait start · item id'))
].join('');
const WINDOWS8 = [
  {key:'spec', title:'Spec Approval', surface:'Inbox → Spec approval'},
  {key:'question', title:'Agent Question', surface:'Inbox → Agent question'},
  {key:'dispute', title:'Review Dispute', surface:'Inbox → Review dispute'},
  {key:'dimin', title:'Review Diminishing Returns', surface:'Inbox → Review diminishing returns'},
  {key:'execfail', title:'Execution Failure', surface:'Inbox → Execution failure'},
  {key:'taskprop', title:'Task Proposal', surface:'Inbox → Task proposal'},
  {key:'effect', title:'Effect Proposal', surface:'Inbox → Effect proposal, closure notice'},
  {key:'health', title:'System Health', surface:'Inbox → System health'},
  {key:'blocked', title:'Blocked', surface:'Inbox → Blocked (read-only, no control group)'},
];

// Run timeline in place of the task
const runTimeline = (t) => `<div style="display: flex; flex-direction: column; gap: 10px; width: ${t.S.card}px;"><div style="color: ${t.accT}; font-weight: 500; font-size: ${t.S.body}px;">‹ Back to Task</div>${card(t, [
  col(10, eyebrow(t, 'Run timeline · Repair the acceptance rig', chip(t, 'On Hold', 'ink')), ask(t, 'Attempt 2'), `<div style="font-family: ${MONO}; font-size: ${t.S.sum}px; color: ${t.dim};">Implementation · Round 2 · Review round 2 running</div>`, sysBar(t, `<span style="font-size: ${t.S.body}px;">Round 2</span><span style="font-family: ${SERIF}; font-size: ${t.S.stmt}px; line-height: 1.4;">Hold: Verification findings block publication</span>`)),
  reviewSection(t),
  col(t.S.mod, kw(t, 'Milestones'), railRow(t, 'current', 'Review · Round 2', 'Aug 11, 10:10 PM'), railRow(t, 'prior', 'Implementation · Pass 2 · Round 1', 'Aug 11, 10:01 PM'), railRow(t, 'prior', 'Review · Round 1', 'Aug 11, 9:58 PM')),
  col(t.S.mod, kw(t, 'Latest invocation observations'), `<div style="color: ${t.dim};">Review · Round 2</div>`, `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: center;"><span style="font-size: ${t.S.label}px;">Reviewer invocation</span><div style="display: flex; gap: 8px; align-items: center;">${chip(t, 'Running', 'ink')}${mono(t, 'observed 10:12 PM', t.S.sum, t.dim)}</div></div>`),
  folds(t, disc(t, 'Technical Details', 'run · task · campaign · parent · digest · hold code'))
].join(''))}</div>`;

// 7.8 two columns + 6.9 inspector, 1,440pt window; right column 360 as main draws it
const twoColCard = (t) => `<div style="width: 100%; box-sizing: border-box; background: ${t.g2}; border: 1px solid ${t.rule}; border-radius: 12px; padding: ${t.S.pad}; display: flex; flex-direction: column; gap: ${t.S.sec}px; font-size: ${t.S.body}px; line-height: 1.5; color: ${t.ink};">
  ${head(t, 'Finding adjudication', 'Approve the proposed dispositions for these findings?')}
  ${dimText(t, 'Accepting applies each proposed route and the run continues to publication.')}
  <div style="display: grid; grid-template-columns: minmax(0, 1fr) 360px; gap: 28px; align-items: start;">
    ${col(t.S.sec, item(t, `<div style="display: flex; gap: 8px; align-items: baseline; flex-wrap: wrap;">${kw(t, 'Finding 1')}${stmt(t, 'Command handler retries without preserving the write-once command identity.')}</div>` + kwInfo(t, 'Model proposal (unverified)') + quote(t, stmt(t, 'Decline the finding', t.S.stmt, 500)) + disc(t, 'Reason and Alternatives'), '12px 14px'), item(t, `<div style="display: flex; gap: 8px; align-items: baseline; flex-wrap: wrap;">${kw(t, 'Finding 2')}${stmt(t, 'Review-level observation: the change lacks a regression test.')}</div>` + kw(t, 'Model judgment with engine-authorized remediation (unverified)') + quote(t, stmt(t, 'Fix in this PR', t.S.stmt, 500)) + disc(t, 'Reason and Alternatives'), '12px 14px'), col(8, kw(t, 'Evidence'), `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;"><span style="font-size: ${t.S.label}px;">3 attachments</span>${link(t, 'In inspector')}</div>`))}
    ${col(10, kw(t, 'Recommendation (unverified)'), `<div>The agent recommends accepting the proposed dispositions for all 2 findings, with high confidence.</div>`, dimText(t, 'The proposals resolve both findings without reopening the specification.'), actions(t, btnFill(t, 'Accept All Dispositions'), row(btnOut(t, 'Choose Another Route'), btnOut(t, 'Discuss')), more(t)), folds(t, disc(t, 'Recorded Context'), disc(t, 'Details', 'audit digest · run · round')))}
  </div>
</div>`;
const inspector = (t) => `<div style="width: 300px; flex: none; background: ${t.side}; border-left: 1px solid ${t.rule}; padding: 18px 16px; display: flex; flex-direction: column; gap: 14px; font-size: ${t.S.body}px; color: ${t.ink};">${kw(t, 'Inspector')}${col(6, disc(t, 'Claims', '2'), `<div style="padding-left: 18px;">${kw(t, 'Agent claims (unverified)')}</div>`)}${disc(t, 'Evidence', '3', {open: true})}<div style="padding-left: 18px; display: flex; flex-direction: column; gap: 6px;">${['screenshot-after.png · image/png · 412 KB', 'reviewer-log.txt · text/plain · 8 KB', 'service.go.patch · text/x-diff · 2 KB'].map(x => `<div style="border: 1px solid ${t.itemB}; border-radius: 6px; padding: 7px 9px; font-family: ${MONO}; font-size: ${t.S.sum}px; color: ${t.dim}; line-height: 1.4;">${x}</div>`).join('')}</div>${disc(t, 'Technical Bindings', '', {open: true})}<div style="padding-left: 18px; display: flex; flex-direction: column; gap: 8px;">${fact(t, 'Head', `<span style="font-family: ${MONO}; font-size: ${t.S.sum}px; color: ${t.dim}; word-break: break-all;">4be1d0a7c3f2e9b8d6a5f4e3c2b1a0998877</span>`, {stack: true})}${fact(t, 'Base', `<span style="font-family: ${MONO}; font-size: ${t.S.sum}px; color: ${t.dim};">main@9c21f0e3</span>`, {stack: true})}</div></div>`;
const window1620 = (t) => `<div style="background: ${t.ground}; border: 1px solid ${t.winB}; border-radius: 12px; overflow: hidden; width: 1620px; min-height: 760px; display: flex; font-size: 13px; color: ${t.ink}; font-family: ${SANS};">${inboxSidebar(t, 'finding')}<div style="flex: 1; display: flex; flex-direction: column; min-width: 0;">${toolbar(t, 'Finding Adjudication')}<div style="flex: 1; display: flex; min-height: 0;"><div style="flex: 1; padding: 20px 24px 40px; min-width: 0; display: flex; align-items: flex-start;">${twoColCard(t)}</div>${inspector(t)}</div></div></div>`;

// 5.7 pairing
const pairingBody = (t, o) => { o = o || {}; const empty = o.state === 'empty';
  const factsBlock = empty ? dimText(t, 'Enter a code to see host details') : facts(t, [fact(t, 'Host', 'studio.local'), fact(t, 'Code', `<span style="font-family: ${MONO}; font-size: ${t.S.mono}px; white-space: nowrap; color: ${t.accT}; font-weight: 500;">Expires in 4 min</span>`), fact(t, 'Connection', 'Local'), fact(t, 'Scope', 'Operator')]);
  return col(t.S.sec, col(10, kw(t, 'Pairing'), ask(t, 'Pair this device')), dimText(t, `Enter the one-time code from <span style="font-family: ${MONO}; font-size: ${t.S.sum}px;">freesided pair</span>. The daemon lists this device under Devices and records every decision it makes.`), col(12, field(t, 'Code', empty ? '' : '7K2M-Q9ZD', empty ? 'XXXX-XXXX' : '', false, empty), field(t, 'Device name', 'Studio Mac')), col(t.S.mod, kw(t, 'Host facts'), factsBlock), `<div style="display: flex; justify-content: flex-end; gap: 8px;">${empty ? btnDisabled(t, 'Pair', false) : btnFill(t, 'Pair', false, false)}</div>`); };
const macPairing = (t, o) => `<div style="background: ${t.ground}; border: 1px solid ${t.winB}; border-radius: 12px; overflow: hidden; width: 560px; display: flex; flex-direction: column; font-size: ${t.S.body}px; color: ${t.ink}; font-family: ${SANS};">${trafficLights(t)}<div style="padding: 12px 32px 32px; display: flex; justify-content: center;"><div style="width: 440px;">${pairingBody(t, o)}</div></div></div>`;

// 6.6 readers
const diffLine = (t, kind, text) => `<div style="font-family: ${MONO}; font-size: ${t.S.mono}px; line-height: 1.6; padding: 0 10px; white-space: pre; color: ${kind === 'add' ? t.add : kind === 'rem' ? t.rem : kind === 'hunk' ? t.dim : t.ink}; background: ${kind === 'add' ? t.addW : kind === 'rem' ? t.remW : 'transparent'};">${text}</div>`;
const readerBody = (t, w) => `<div style="display: flex; flex-direction: column; gap: ${t.S.sec}px; font-size: ${t.S.body}px; line-height: 1.5; color: ${t.ink};">
  ${stmt(t, 'Bound by the daemon to this approval')}
  ${col(8, kw(t, 'Specification (unverified)'), `<div style="border: 1px dashed ${t.acc}; border-radius: 8px; padding: 12px 14px; display: flex; flex-direction: column; gap: 8px;"><div style="font-family: ${SERIF}; font-weight: 500; font-size: ${t.S.stmt}px;">Migration order</div><div>Migrate stored rows first; keep the old API shape for one release. See RFC 12 <span style="font-family: ${MONO}; font-size: ${t.S.sum}px; color: ${t.dim};">(docs/rfc/12.md)</span>.</div></div>`)}
  ${col(8, kw(t, 'Diff'), `<div style="border: 1px solid ${t.itemB}; border-radius: 8px; padding: 6px 0; overflow: hidden;">${diffLine(t, 'hunk', `@@ <span style="color: ${t.rem};">−214,7</span> <span style="color: ${t.add};">+214,9</span> @@ func apply(cmd Command)`)}${diffLine(t, 'ctx', '  func apply(cmd Command) error {')}${diffLine(t, 'rem', '−   id := newID()')}${diffLine(t, 'add', '+   id := cmd.ID')}${diffLine(t, 'add', '+   if id == "" { return ErrNoID }')}${diffLine(t, 'ctx', '    return store.Put(id, cmd)')}</div>`, disc(t, '2 Later Hunks', 'payload truncated at 64 KiB', {stack: w < 400}))}
  ${folds(t, `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: flex-start;">${disc(t, 'Technical Details', 'sha256:675a3f2208ee1b5e…', {stack: true})}<span style="color: ${t.accT}; font-weight: 500; white-space: nowrap;">Copy</span></div>`)}
</div>`;
const readerPane = (t, w) => `<div style="width: ${w}px; background: ${t.g2}; border: 1px solid ${t.winB}; border-radius: 12px; overflow: hidden; display: flex; flex-direction: column; font-family: ${SANS}; color: ${t.ink};"><div style="display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 12px ${w < 400 ? 14 : 18}px; border-bottom: 1px solid ${t.rule}; flex: none;"><div style="display: flex; align-items: center; gap: 8px; min-width: 0; flex-wrap: wrap; row-gap: 6px;">${kw(t, w < 400 ? 'Specification' : 'Specification reader')}${chip(t, 'Revision 1', 'ink')}</div>${btnOut(t, w < 400 ? 'Close' : 'Close Reader', false, false)}</div><div style="padding: 18px ${w < 400 ? 14 : 18}px 20px;">${readerBody(t, w)}</div></div>`;

// 7.7 accessibility 1 on the Mac ladder: everything up two steps, everything stacks
const A8 = {kw: 13, ask: 25, stmt: 18, label: 16, value: 15, body: 15, btn: 40};
const accCard = (t, w) => { const akw = (text) => `<span style="font-family: ${MONO}; font-size: ${A8.kw}px; font-weight: 500; letter-spacing: 0.08em; text-transform: uppercase; color: ${t.dim};">${text}</span>`; const abtn = (label, fill) => `<div style="display: flex; align-items: center; justify-content: center; height: ${A8.btn}px; border-radius: 6px; font-weight: 500; font-size: ${A8.body}px; white-space: nowrap; ${fill ? `background: ${t.fillBg}; color: ${t.fillT};` : `border: 1px solid ${t.outB}; color: ${t.ink};`}">${label}</div>`; const achip = (text) => `<span style="font-family: ${MONO}; font-size: 13px; font-weight: 500; letter-spacing: 0.04em; border: 1px solid ${t.accB}; color: ${t.accT}; border-radius: 3px; padding: 3px 9px;">${text}</span>`;
  return `<div style="width: ${w}px; box-sizing: border-box; background: ${t.g2}; border: 1px solid ${t.rule}; border-radius: 12px; padding: 24px 24px 20px; display: flex; flex-direction: column; gap: 22px; font-size: ${A8.body}px; line-height: 1.45; color: ${t.ink};">
  <div style="display: flex; flex-direction: column; gap: 10px;">${akw('Ready for final review')}<div style="display: flex;">${achip('High')}</div><div style="font-family: ${SERIF}; font-weight: 500; font-size: ${A8.ask}px; line-height: 1.2;">Is this change ready for final GitHub review?</div></div>
  <div style="display: flex; flex-direction: column; gap: 10px;"><div style="display: flex; align-items: center; gap: 8px;">${akw('Agent summary (unverified)')}<span style="width: 18px; height: 18px; border-radius: 50%; border: 1.5px solid ${t.dim}; box-sizing: border-box; display: inline-flex; align-items: center; justify-content: center; font-family: ${SERIF}; font-size: 12px; color: ${t.dim}; font-style: italic;">i</span></div>${quote(t, `<div style="font-family: ${SERIF}; font-size: ${A8.stmt}px; line-height: 1.4;">Renewal now re-reads the credential file on each 401 and retries once.</div>`, '12px 14px')}</div>
  <div style="display: flex; flex-direction: column; gap: 5px;">${akw('Change')}${diff(t, 412, 88, '· 9 files', A8.value)}</div>
  <div style="border: 1px solid ${t.itemB}; border-radius: 8px; padding: 14px 16px; display: flex; flex-direction: column; gap: 12px;">${akw('Readiness checklist')}<div style="display: flex;">${achip('Degraded')}</div><div style="display: flex; flex-direction: column; gap: 3px;"><span style="display: flex; gap: 10px; align-items: center; font-size: ${A8.label}px;"><span style="width: 8px; height: 8px; border-radius: 50%; background: ${t.waxT};"></span>Independent Review</span><span style="font-family: ${MONO}; font-size: ${A8.value}px; padding-left: 18px;">Waived · granted by operator</span></div><div style="display: flex; flex-direction: column; gap: 3px;"><span style="display: flex; gap: 10px; align-items: center; font-size: ${A8.label}px;"><span style="width: 8px; height: 8px; border-radius: 50%; background: ${t.ink};"></span>Commit Plan</span><span style="font-family: ${MONO}; font-size: ${A8.value}px; padding-left: 18px;">Present, not honored</span></div><div style="display: flex; flex-direction: column; gap: 3px;"><span style="display: flex; gap: 8px; align-items: center; font-size: ${A8.label}px;"><span style="color: ${t.accT}; font-size: 11px;">▶</span>3 Passed</span><span style="font-family: ${MONO}; font-size: ${A8.value - 1}px; color: ${t.dim}; padding-left: 20px;">Bound to · clean-verification · Terminal review</span></div></div>
  <div style="display: flex; flex-direction: column; gap: 8px;">${abtn('View PR', true)}${abtn('Return to Agent')}<div style="text-align: center; color: ${t.dim}; font-weight: 500; font-size: ${A8.body}px; padding-top: 2px;">More Actions ▾</div></div>
  <div style="display: flex; flex-direction: column; gap: 12px; border-top: 1px solid ${t.rule}; padding-top: 16px;">${['Run and Binding Details', 'Review Yield', 'Recorded Context'].map(l => `<div style="display: flex; gap: 8px; align-items: center; font-size: ${A8.label}px;"><span style="color: ${t.accT}; font-size: 11px;">▶</span>${l}</div>`).join('')}</div>
</div>`; };

BANDS.push({id: 'c', title: '3 · Mac — the rest on the native ladder', note: 'nine card types · run timeline · 1,620pt two-column card with the inspector (the width main needs) · pairing · readers 320 / 480 / 720 · accessibility 1', frames: (t0) => { const t = mac(t0); return [
  ...WINDOWS8.map(w => frame({width: 1180, label: `Mac ${t.name} — ${w.surface}`, caption: cap(`macOS · ${t.name}`, w.surface, 'issues 1 · 6'), html: macWindow(t, {title: w.title, sidebar: inboxSidebar(t, w.key), detail: card(t, CARDS8[w.key](t))}), diamonds: []})),
  frame({width: 1180, label: `Mac ${t.name} — Tasks → run timeline`, caption: cap(`macOS · ${t.name}`, 'Tasks → run timeline in place of the task, Back to Task above', 'issues 1 · 6'), html: macWindow(t, {title: 'Run Timeline', sidebar: tasksSidebar(t, 'rig'), detail: runTimeline(t)}), diamonds: []}),
  frame({width: 1620, label: `Mac ${t.name} — 1,620pt two-column card with inspector`, caption: cap(`macOS · ${t.name} · 1,000pt detail`, 'Inbox → finding adjudication, two columns beside the inspector', 'issues 1 · 6'), html: window1620(t), note: 'Drawn at 1,620pt, the window width main needs for two columns beside an open inspector (sidebar 300 + inspector 300 leave 1,000 for the pane). Right column 360 as built. The two-column card fills the pane rather than capping at 640.', diamonds: [['7.8', 'Narrow the inspector so 1,440pt reaches two columns, or accept 1,620pt.']]}),
  frame({width: 560, label: `Mac ${t.name} — pairing`, caption: cap(`macOS · ${t.name}`, 'Pairing, facts returned — Pair trailing, Mac sheet convention', 'issue 1'), html: macPairing(t, {}), diamonds: []}),
  frame({width: 560, label: `Mac ${t.name} — pairing, empty code`, caption: cap(`macOS · ${t.name}`, 'Pairing, empty code — Pair disabled', 'issues 1 · 4'), html: macPairing(t, {state: 'empty'}), diamonds: []}),
  frame({width: 320, label: `Mac ${t.name} — reader at 320`, caption: cap(`macOS · ${t.name} · pane 320`, 'Reader, Close as a button in the header', 'issues 1 · 3'), html: readerPane(t, 320), diamonds: []}),
  frame({width: 480, label: `Mac ${t.name} — reader at 480`, caption: cap(`macOS · ${t.name} · pane 480`, 'Reader', 'issues 1 · 3'), html: readerPane(t, 480), diamonds: []}),
  frame({width: 720, label: `Mac ${t.name} — reader at 720`, caption: cap(`macOS · ${t.name} · pane 720`, 'Reader', 'issues 1 · 3'), html: readerPane(t, 720), diamonds: []}),
  frame({width: 1180, label: `Mac ${t.name} — accessibility 1`, caption: cap(`macOS · ${t.name} · accessibility 1`, 'Ready for final review — two steps up from the Mac ladder, everything stacks', 'issue 1'), html: macWindow(t, {title: 'Ready for Final Review', sidebar: inboxSidebar(t, 'final'), detail: accCard(t, 640)}), diamonds: []}),
]; }});
