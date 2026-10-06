// Group 1 · the eleven decision cards as Mac Inbox → card windows. Evaluated after lib.js.
const CARDS = {};

CARDS.finding = (t, o) => { o = o || {}; const expanded = o.expanded !== false;
  const opt = (label, cons, sel, proposed) => `<div style="display: flex; gap: 10px; align-items: flex-start;">${selMark(t, sel)}<div style="flex: 1; min-width: 0; display: flex; background: ${t.qW}; border-radius: 6px; overflow: hidden; ${sel ? `box-shadow: inset 0 0 0 1px ${t.ink};` : ''}"><div style="width: 3px; background: ${t.qR}; flex: none;"></div><div style="padding: 10px 14px; display: flex; flex-direction: column; gap: 3px; min-width: 0; flex: 1;"><div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${stmt(t, label, 17, 500)}${proposed ? kw(t, 'Proposed', t.accT) : ''}</div>${dimText(t, cons)}</div></div></div>`;
  const expandedBody = expanded ? col(16,
    col(8, kw(t, 'Evidence (unverified)'), quote(t, `<div>service.go:214 re-derives the command id on each retry</div><div>The contract requires a stable command identity</div>`, '10px 14px')),
    facts(t, [fact(t, 'Finding', 'review-finding-17'), fact(t, 'Location', 'daemon/internal/signet/service.go:214-227', {stack: true}), fact(t, 'Run · Round', 'attempt 2 · 3'), fact(t, 'Cited Rule', 'CONTRIBUTING § Idempotent commands')], 'Daemon facts'),
    col(8, kw(t, 'Assumes (unverified)'), quote(t, `<div>The retry path is reachable in production.</div>`, '10px 14px')),
    col(10, kw(t, 'Route'),
      opt('Decline the finding', 'Nothing changes in the PR; the finding is recorded as declined.', false, true),
      opt('Fix in this PR', 'Starts a remediator limited to the run’s allowed paths and re-reviews the PR.', true, false),
      opt('Park: needs separate work', 'Parks the run with nothing fixed or published.', false, false),
      dimText(t, 'Choosing sends this route for Finding 1 only. Accept All Dispositions still applies the proposals to the rest.'),
      btnOut(t, 'Use This Route for Finding 1')),
    col(8, kw(t, 'Open question (unverified)'), quote(t, `<div>Is the retry path exercised by any caller today?</div>`, '10px 14px'))
  ) : '';
  return [
    head(t, 'Finding adjudication', 'Approve the proposed dispositions for these findings?'),
    item(t, kw(t, 'Finding 1') + stmt(t, 'Command handler retries without preserving the write-once command identity.') + kwInfo(t, 'Model proposal (unverified)') + quote(t, stmt(t, 'Decline the finding', 17, 500) + dimText(t, 'The finding assumes a retry guarantee the approved work-unit contract explicitly rejects.') + dimText(t, 'Contradicts the goal · High confidence · compatibility not assessed', 13.5), '10px 14px') + (expanded ? dimText(t, 'Chosen: Fix in this PR — Accept All Dispositions does not send it.') : '') + disc(t, 'Reason and Alternatives', '', {open: expanded}) + expandedBody),
    item(t, kw(t, 'Finding 2') + stmt(t, 'Review-level observation: the change lacks a regression test.') + kw(t, 'Model judgment with engine-authorized remediation (unverified)') + quote(t, stmt(t, 'Fix in this PR', 17, 500), '10px 14px') + disc(t, 'Reason and Alternatives')),
    col(12, kw(t, 'Recommendation (unverified)'), `<div>The agent recommends accepting the proposed dispositions for all 2 findings above, with high confidence.</div>`, actions(t, btnFill(t, 'Accept All Dispositions'), row(btnOut(t, 'Discuss'), btnOut(t, 'Choose Another Route')), more(t))),
    folds(t, disc(t, 'Recorded Context'), disc(t, 'Details', 'audit digest · run · round'))
  ].join('');
};

CARDS.spec = (t) => [
  head(t, 'Spec approval', 'Approve this specification for implementation?', chip(t, 'High', 'accent')),
  col(10, kwInfo(t, 'Agent summary (unverified)'), quote(t, stmt(t, 'Revision 1 keeps the existing migration order and narrows the rollback step to the compatibility check; one decision is open on reader ordering.'))),
  col(11, kw(t, 'Specification'), item(t, `<div style="display: flex; justify-content: space-between; align-items: baseline; gap: 12px;"><div style="display: flex; flex-direction: column; gap: 2px;"><span style="font-size: 16px;">Revision 1</span>${dimText(t, 'Bound by the daemon to this approval')}</div>${link(t, 'Open Reader')}</div>`, '14px 18px')),
  col(12, kw(t, 'Conversation'), msgYou(t, 'Can the revised spec preserve the existing migration order?'), msgAgent(t, 'Yes. The revision keeps the order and narrows the rollback step. First, migrate the stored rows while the old API shape remains available. Then enable the updated reader after the migration has completed.'), reply(t)),
  actions(t, row(btnOut(t, 'Approve'), btnOut(t, 'Request Changes')), more(t)),
  folds(t, disc(t, 'Source and Original Report', 'inv-agent-spec-approval'), disc(t, 'Details', 'Specification digest · item version'))
].join('');

CARDS.question = (t) => {
  const opt = (n, label, cons, rec) => quote(t, `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${kw(t, 'Option ' + n)}${rec ? kw(t, 'Agent recommends', t.accT) : ''}</div>${stmt(t, label, 17, 500)}<div>${cons}</div>`, '10px 14px');
  return [
    head(t, 'Agent question (unverified)', 'Which order should the migration run in?', '', true),
    col(11, opt(1, 'Store first, then API', 'Existing rows migrate before any client can read them, and the API keeps the old shape for one release.', true), opt(2, 'API first, then store', 'Clients move immediately, and the daemon reads both shapes until the store migration lands.')),
    actions(t, row(btnFill(t, 'Answer and Retry'), btnOut(t, 'Answer Without Retry')), more(t)),
    folds(t, disc(t, 'Run and Binding Details', 'Implementation · blocked on this answer'), disc(t, 'Recorded Context'), disc(t, 'Source and Original Report', 'inv-agent-question-731'))
  ].join('');
};

CARDS.final = (t, o) => { o = o || {}; const stale = !!o.stale;
  const checkRow = (kind, label, value) => `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;"><span style="display: flex; gap: 10px; align-items: center; font-size: 16px;"><span style="width: 8px; height: 8px; border-radius: 50%; ${kind === 'wax' ? `background: ${t.waxT};` : `background: ${t.ink};`} flex: none;"></span>${label}</span>${mono(t, value, 14.5, kind === 'wax' && stale ? t.waxT : t.ink)}</div>`;
  return [
    head(t, 'Ready for final review', 'Is this change ready for final GitHub review?', chip(t, 'High', 'accent')),
    stale ? notice(t, 'Stale', 'The base advanced after verification. The verdict below was clean at head 4be1d0a7 against main@9c21f0e3; main is now at e7a2c0d1.', 'wax', '', true) : '',
    col(10, kwInfo(t, 'Agent summary (unverified)'), quote(t, stmt(t, 'Renewal now re-reads the credential file on each 401 and retries once; the reviewer flagged no concerns.')), disc(t, 'Full Report', 'inv-7f3a · 2 concerns')),
    `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${kw(t, 'Change')}${diff(t, 412, 88, '· 9 files', 16)}</div>`,
    item(t, kw(t, 'Readiness checklist') + `<div style="display: flex; align-items: center; gap: 10px;">${stale ? chip(t, 'Stale', 'wax') + mono(t, 'was Clean · 1 note · 4 passed') : chip(t, 'Degraded', 'accent') + mono(t, '1 waived · 1 note · 3 passed')}</div>` + (stale ? checkRow('wax', 'Bound to', 'Base main@9c21f0e3 → e7a2c0d1') : checkRow('wax', 'Independent Review', 'Waived · granted by operator')) + checkRow('ink', 'Commit Plan', 'Present, not honored') + disc(t, stale ? '4 Passed' : '3 Passed', stale ? 'Head 4be1d0a7 · clean-verification · Terminal review' : 'Bound to · clean-verification · Terminal review', {stack: true})),
    stale ? actions(t, row(btnOut(t, 'Return to Agent'), btnOut(t, 'View PR', true)), more(t)) : actions(t, btnFill(t, 'View PR', true), btnOut(t, 'Return to Agent'), more(t)),
    folds(t, disc(t, 'Run and Binding Details', stale ? 'Head 4be1d0a7 · Base main@9c21f0e3 · observed e7a2c0d1' : 'Head 4be1d0a7 · Base main@9c21f0e3', {stack: true}), disc(t, 'Review Yield', '3 rounds · last clean'), disc(t, 'Recorded Context'))
  ].join('');
};

CARDS.dispute = (t) => [
  head(t, 'Review dispute', 'How should this review dispute be resolved?'),
  col(11, kw(t, 'Positions'), col(8, kw(t, 'Reviewer (unverified)'), quote(t, stmt(t, 'P1 shadow finding at daemon/main.go:42: the sampled reviewer found a blocking defect.'))), col(8, kwInfo(t, 'Agent (unverified)'), quote(t, stmt(t, 'The path is unreachable in production; the finding should be withdrawn.')))),
  actions(t, row(btnOut(t, 'Approve'), btnOut(t, 'Discuss')), more(t)),
  folds(t, disc(t, 'Source and Supporting Details', 'two claims · digests'), disc(t, 'Run and Binding Details', 'attempt 2 · round 3 · 1 disputed'), disc(t, 'Details'))
].join('');

CARDS.dimin = (t) => {
  const fix = (label, cons, id) => quote(t, stmt(t, label, 17, 500) + dimText(t, `${cons ? cons + ' · ' : ''}<span style="font-family: ${MONO}; font-size: 13.5px;">${id}</span>`, 13.5), '10px 14px');
  const yr = (n, nw, rc, total) => `<div style="display: flex; flex-direction: column; gap: 5px;"><div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;"><span style="font-size: 16px;">Round ${n}</span><span style="font-family: ${MONO}; font-size: 14.5px; white-space: nowrap;"><span style="color: ${t.accT};">${nw} new</span> · <span style="color: ${t.waxT};">${rc} recurring</span></span></div><div style="display: flex; height: 8px; width: ${Math.round(total / 8 * 100)}%; border-radius: 4px; overflow: hidden;">${nw ? `<span style="flex: ${nw}; background: ${t.acc};"></span>` : ''}${rc ? `<span style="flex: ${rc}; background: ${t.waxT};"></span>` : ''}</div></div>`;
  return [
    head(t, 'Review diminishing returns', 'Stop review here?'),
    stmt(t, 'Round 4 found only style-level issues; the reviewer’s confidence that further rounds will find defects has fallen below the policy floor.'),
    facts(t, [fact(t, 'Verdict', 'Simplification drift'), fact(t, 'Confidence', 'High')], 'Drift audit'),
    col(10, kwInfo(t, 'Fixes to undo (unverified)'), quote(t, stmt(t, 'Round 3 traded a guard clause for a comment; rounds 2–4 reverted each other’s renames.', 16)), fix('Restore the nil guard in service.go:214', 'The comment does not prevent the panic', 'review-finding-21'), fix('Revert the rename to applyOnce', 'It hides the retry semantics', 'review-finding-23'), fix('Reinstate the test round 2 deleted', '', 'review-finding-24'), disc(t, '2 More Fixes', '5 in total')),
    col(11, kw(t, 'Review yield'), yr(1, 6, 2, 8), yr(2, 2, 3, 5), yr(3, 1, 1, 2), yr(4, 0, 1, 1), `<div style="display: flex; gap: 16px; font-size: 13.5px; color: ${t.dim};"><span style="display: flex; gap: 6px; align-items: center;"><span style="width: 8px; height: 8px; border-radius: 50%; background: ${t.acc};"></span>new findings</span><span style="display: flex; gap: 6px; align-items: center;"><span style="width: 8px; height: 8px; border-radius: 50%; background: ${t.waxT};"></span>recurring</span></div>`),
    facts(t, [fact(t, 'Cost so Far', '$41.20'), fact(t, 'Diff Growth', `<span style="font-family: ${MONO}; font-size: 14.5px; white-space: nowrap;">${diff(t, 312, 40)} → ${diff(t, 388, 61)}</span>`)], 'Facts'),
    actions(t, row(btnOut(t, 'Finish Now'), btnOut(t, 'Apply Then Finish')), more(t), dimText(t, 'Continue Under Policy, in More Actions, runs the simplification round and undoes the fixes above.', 13.5)),
    folds(t, disc(t, 'Recorded Context'), disc(t, 'Details', 'audit digest · run · round'))
  ].join('');
};

CARDS.execfail = (t) => [
  head(t, 'Execution failure', 'Choose a recovery for this failed execution?', chip(t, 'Urgent', 'wax')),
  facts(t, [fact(t, 'Outcome', 'Exited non-zero'), fact(t, 'Stage', 'Implementation · Pass 2'), fact(t, 'Invocation', 'inv-9c21f0e3')], 'Failure'),
  col(10, kwInfo(t, 'Diagnostic (unverified)'), quote(t, stmt(t, 'The container ran out of memory while compiling the generated schema; a larger ward or a narrower generation target should let the attempt complete.'))),
  col(11, kw(t, 'Stages'), railRow(t, 'wax', 'Implementation · Pass 2', 'Sep 12, 8:12 AM', 'Execution failed'), railRow(t, 'prior', 'Implementation · Pass 1', 'Sep 11, 4:58 PM'), railRow(t, 'prior', 'Specification · Round 1', 'Sep 11, 2:02 PM')),
  actions(t, row(btnFill(t, 'Retry'), btnOut(t, 'Discuss')), more(t)),
  folds(t, disc(t, 'Recorded Context'), disc(t, 'Run and Binding Details', 'attempt 2 · export none'), disc(t, 'Details', 'run · invocation · digest'))
].join('');

CARDS.taskprop = (t) => [
  head(t, 'Task proposal', 'Start this proposed task?'),
  col(10, kwInfo(t, 'Proposal (unverified)'), quote(t, stmt(t, 'Task names are free text and collide across projects; binding each to the operator who set it and rejecting duplicates within a project removes the ambiguity the inbox shows today.'))),
  facts(t, [fact(t, 'Project', 'owner/repo'), fact(t, 'Source', 'owner/repo#731'), fact(t, 'Proposed Name', 'Bound operator task names'), fact(t, 'Declared Paths', '4')], 'Facts'),
  actions(t, row(btnFill(t, 'Start'), btnOut(t, 'Start With Changes')), more(t)),
  folds(t, disc(t, 'Recorded Context'), disc(t, 'Details', 'proposal digest · declaration · item version'))
].join('');

CARDS.effect = (t) => [
  head(t, 'Effect proposal', 'Close the source issue when this merges?'),
  stmt(t, `Merging head <span style="font-family: ${MONO}; font-size: 15px;">4be1d0a7</span> into main would close owner/repo#724, the issue this task was taken from.`),
  facts(t, [fact(t, 'Target Issue', link(t, 'owner/repo#724')), fact(t, 'Reference', 'Verified'), fact(t, 'Closure Flag', 'From intake'), fact(t, 'Binds to', 'head 4be1d0a7 · main@9c21f0e3')], 'Facts'),
  actions(t, row(btnFill(t, 'Approve'), btnOut(t, 'Approve With Changes')), more(t)),
  folds(t, disc(t, 'Recorded Context'), disc(t, 'Details', 'proposal digest · item version'))
].join('');

CARDS.health = (t) => [
  head(t, 'System health', 'Acknowledge this finding?', chip(t, 'Urgent', 'wax')),
  stmt(t, 'Backup encryption key is unreadable; encrypted backups are paused until it is restored or re-enrolled.'),
  facts(t, [fact(t, 'Diagnostic', 'backup_key_unreadable'), fact(t, 'Impairs', 'Encrypted backups'), fact(t, 'Posture', chip(t, 'Degraded', 'accent')), fact(t, 'Re-enrollment', 'Required · machine key')], 'Facts'),
  actions(t, row(btnFill(t, 'Acknowledge'), btnOut(t, 'Run Doctor')), more(t)),
  folds(t, disc(t, 'Recorded Context'), disc(t, 'Details', 'finding id · observed at'))
].join('');

CARDS.blocked = (t) => [
  head(t, 'Blocked', 'Waiting on specification approval.'),
  facts(t, [fact(t, 'Waiting', '3h'), fact(t, 'Blocked on', link(t, 'Spec approval · #731'))], 'Facts'),
  folds(t, disc(t, 'Details', 'wait start · item id'))
].join('');

const WINDOWS = [
  {key:'finding', title:'Finding Adjudication', surface:'Inbox → Finding adjudication, Reason and Alternatives open on Finding 1', rules:'R4 R5 R9 R19 R20 R21 R22 R25 R26 R27 R29 R31', diamonds:[['7.1','Rationale and qualities folded into the route quote (drawn) or kept as their own section.'],['7.1','Per-finding submit as a visible outline once a pick differs from the proposal (drawn) — the existing choose_alternative_route command, not a new approval.']]},
  {key:'spec', title:'Spec Approval', surface:'Inbox → Spec approval', rules:'R0 R3 R4 R5 R6 R7 R17 R19 R25 R26 R27 R31', diamonds:[['5.1','Thread above the actions (drawn: prior questions are part of deciding) or below as history.'],['5.1','Approve unfilled (drawn) on a card with no recommendation.']]},
  {key:'question', title:'Agent Question', surface:'Inbox → Agent question', rules:'R0 R4 R5 R6 R19 R21 R23 R24 R25 R26 R27 R31', diamonds:[]},
  {key:'final', title:'Ready for Final Review', surface:'Inbox → Ready for final review (4b)', rules:'R2 R3 R4 R5 R6 R7 R9 R10 R19 R26 R27 R28 R31', diamonds:[]},
  {key:'dispute', title:'Review Dispute', surface:'Inbox → Review dispute, both positions', rules:'R4 R5 R7 R19 R24 R25 R26 R27 R31', diamonds:[['7.4','Reviewer position quoted (drawn — an agent’s words too) or bordered as authenticated when the daemon vouches for the finding text.'],['7.4','“Accept Dispute” and its scope sentence wait on the contract; the button keeps the existing “Approve” label until it confirms what approve does on an observation-only shadow finding.']]},
  {key:'dimin', title:'Review Diminishing Returns', surface:'Inbox → Review diminishing returns', rules:'R0 R1 R4 R5 R6 R9 R19 R24 R26 R27 R28 R31', diamonds:[['5.2','Chart grammar: horizontal capsules, new in the accent and recurring in wax, label left and counts right, legend kept, no restating sentence (drawn).'],['5.2','Continue Under Policy in More Actions (drawn) given it undoes fixes, or a visible third outline.']]},
  {key:'execfail', title:'Execution Failure', surface:'Inbox → Execution failure', rules:'R0 R4 R6 R9 R15 R19 R23 R26 R27 R31', diamonds:[['5.3','Diagnostic before the rail (drawn: P1, the thing to weigh; the rail is the where).'],['5.3','#869’s provider facts (cost owner, independence) in a picker sheet (R11) rather than on the card.']]},
  {key:'taskprop', title:'Task Proposal', surface:'Inbox → Task proposal', rules:'R0 R4 R5 R6 R9 R19 R24 R26 R27 R31', diamonds:[['5.4','Snooze as an action in More Actions (drawn) or a fold holding a snoozed-until fact.'],['5.4','Start With Changes as the one outline (drawn): it opens a sheet, not the same act as Start.'],['5.4','Declared paths as a fact (drawn) or in Details.']]},
  {key:'effect', title:'Effect Proposal', surface:'Inbox → Effect proposal, source-issue closure', rules:'R0 R3 R4 R6 R9 R19 R26 R27 R31', diamonds:[['7.9','Binding as a serif statement (drawn) or as four fact rows.'],['7.9','Approve With Changes opens a two-way control (drawn) or the native toggle.']]},
  {key:'health', title:'System Health', surface:'Inbox → System health', rules:'R0 R4 R6 R9 R19 R23 R24 R26 R27 R31', diamonds:[['5.5','Resume Unattended in More Actions (drawn) or a visible outline, since it reopens the gate (#980).']]},
  {key:'blocked', title:'Blocked', surface:'Inbox → Blocked', rules:'R0 R3 R4 R9 R19 R24 R26 R27 R31', diamonds:[['5.5','No-action cards: an absent action row (drawn) or a sentence saying so.'],['5.5','Blocked’s lead: the wait sentence (drawn) or the blocking item’s name.']]},
];

const inboxWindow = (t, w) => macWindow(t, {title: w.title, sidebar: inboxSidebar(t, w.key), detail: card(t, CARDS[w.key](t))});
BANDS.push({id: 'g1', title: '1 · Mac — Inbox → decision, one window per card type', note: 'sidebar under R27 R4 R31 R19 · detail column carries the refined card at the 4b scale · day row, then dusk', frames: (t) => group1(t)});
const group1 = (t) => WINDOWS.map(w => frame({width: 1180, label: `Mac ${t.name} — ${w.surface}`, caption: cap(`macOS · ${t.name}`, w.surface, w.rules, w.diamonds.length), html: inboxWindow(t, w), diamonds: w.diamonds}));
