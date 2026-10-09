// Group A · Mac at the native ladder. Evaluated after lib8.js.
const findingCard = (t) => {
  const opt = (label, cons, sel, proposed) => `<div style="display: flex; gap: 10px; align-items: flex-start;">${selMark(t, sel)}<div style="flex: 1; min-width: 0; display: flex; background: ${t.qW}; border-radius: 6px; overflow: hidden; ${sel ? `box-shadow: inset 0 0 0 1px ${t.ink};` : ''}"><div style="width: 3px; background: ${t.qR}; flex: none;"></div><div style="padding: 8px 12px; display: flex; flex-direction: column; gap: 2px; min-width: 0; flex: 1;"><div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${stmt(t, label, t.S.stmt, 500)}${proposed ? kw(t, 'Proposed', t.accT) : ''}</div>${dimText(t, cons)}</div></div></div>`;
  const expandedBody = col(14,
    col(6, kw(t, 'Evidence (unverified)'), quote(t, `<div>service.go:214 re-derives the command id on each retry</div><div>The contract requires a stable command identity</div>`)),
    facts(t, [fact(t, 'Finding', 'review-finding-17'), fact(t, 'Location', 'daemon/internal/signet/service.go:214-227', {stack: true}), fact(t, 'Run', 'attempt 2'), fact(t, 'Round', '3')], 'Daemon facts'),
    col(8, kw(t, 'Route'), opt('Decline the finding', 'Nothing changes in the PR; the finding is recorded as declined.', false, true), opt('Fix in this PR', 'Starts a remediator limited to the run’s allowed paths and re-reviews the PR.', true, false), opt('Park: needs separate work', 'Parks the run with nothing fixed or published.', false, false)),
    col(6, kw(t, 'Open question (unverified)'), quote(t, `<div>Is the retry path exercised by any caller today?</div>`)));
  return [
    head(t, 'Finding adjudication', 'Approve the proposed dispositions for these findings?'),
    dimText(t, 'Accepting applies each proposed route and the run continues to publication.'),
    item(t, `<div style="display: flex; gap: 8px; align-items: baseline; flex-wrap: wrap;">${kw(t, 'Finding 1')}${stmt(t, 'Command handler retries without preserving the write-once command identity.')}</div>` + kwInfo(t, 'Model proposal (unverified)') + quote(t, stmt(t, 'Decline the finding', t.S.stmt, 500) + dimText(t, 'The finding assumes a retry guarantee the approved work-unit contract explicitly rejects.') + dimText(t, 'Contradicts the goal · High confidence', t.S.sum)) + dimText(t, 'Chosen: Fix in this PR — Accept All Dispositions does not send it.') + disc(t, 'Reason and Alternatives', '', {open: true}) + expandedBody, '12px 14px'),
    item(t, `<div style="display: flex; gap: 8px; align-items: baseline; flex-wrap: wrap;">${kw(t, 'Finding 2')}${stmt(t, 'Review-level observation: the change lacks a regression test.')}</div>` + kw(t, 'Model judgment with engine-authorized remediation (unverified)') + quote(t, stmt(t, 'Fix in this PR', t.S.stmt, 500)) + disc(t, 'Reason and Alternatives'), '12px 14px'),
    col(10, kw(t, 'Recommendation (unverified)'), `<div>The agent recommends accepting the proposed dispositions for all 2 findings, with high confidence.</div>`, dimText(t, 'The proposals resolve both findings without reopening the specification; the remediation stays inside the run’s allowed paths.'), actions(t, row(btnFill(t, 'Accept All Dispositions'), btnOut(t, 'Choose Another Route'), btnOut(t, 'Discuss')), more(t))),
    folds(t, disc(t, 'Recorded Context'), disc(t, 'Details', 'audit digest · run · round'))
  ].join('');
};

const finalCard = (t) => {
  const checkRow = (kind, label, value) => `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;"><span style="display: flex; gap: 8px; align-items: center; font-size: ${t.S.label}px;"><span style="width: 7px; height: 7px; border-radius: 50%; background: ${kind === 'wax' ? t.waxT : t.ink}; flex: none;"></span>${label}</span>${mono(t, value)}</div>`;
  return [
    head(t, 'Ready for final review', 'Is this change ready for final GitHub review?', chip(t, 'High', 'accent')),
    col(8, kwInfo(t, 'Agent summary (unverified)'), quote(t, stmt(t, 'Renewal now re-reads the credential file on each 401 and retries once; the reviewer flagged no concerns.')), disc(t, 'Full Report', 'inv-7f3a · 2 concerns')),
    `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: baseline;">${kw(t, 'Change')}${diff(t, 412, 88, '· 9 files')}</div>`,
    item(t, kw(t, 'Readiness checklist') + `<div style="display: flex; align-items: center; gap: 10px;">${chip(t, 'Degraded', 'accent')}${mono(t, '1 waived · 1 note · 3 passed')}</div>` + checkRow('wax', 'Independent Review', 'Waived · granted by operator') + checkRow('ink', 'Commit Plan', 'Present, not honored') + disc(t, '3 Passed', 'Bound to · clean-verification · Terminal review'), '12px 14px'),
    actions(t, row(btnFill(t, 'View PR', true), btnOut(t, 'Return to Agent')), more(t)),
    folds(t, disc(t, 'Run and Binding Details', 'Head 4be1d0a7 · Base main@9c21f0e3'), disc(t, 'Review Yield', '3 rounds · last clean'), disc(t, 'Recorded Context'))
  ].join('');
};

// Task timeline at the Mac ladder; the header's stop state is a notice that only states — the actions stay buttons in the control group.
const reviewSection = (t) => col(t.S.mod, kw(t, 'Review'),
  `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: center;"><div style="display: flex; gap: 8px; align-items: center;">${marker(t, 'current')}<span style="font-size: ${t.S.label}px; font-weight: 600; white-space: nowrap;">Round 2</span>${chip(t, 'Findings · 2 Open', 'accent')}</div>${mono(t, 'Aug 11, 10:10 PM', t.S.sum, t.dim)}</div>`,
  `<div style="padding-left: 18px; display: flex; flex-direction: column; gap: 5px;">${disc(t, 'Round Facts', 'Head cccccccc · Base aaaaaaaa · Freeside-invoked')}<div style="padding-left: 18px;">${link(t, 'Inspect reviewer output')}</div></div>`,
  `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: center;"><div style="display: flex; gap: 8px; align-items: center; color: ${t.dim};">${marker(t, 'prior')}<span style="font-size: ${t.S.label}px; white-space: nowrap;">Round 1 · clean at its bound head</span></div>${mono(t, 'Aug 11, 9:58 PM', t.S.sum, t.dim)}</div>`,
  `<div style="display: flex;">${btnOut(t, 'Retry Review Details', false, false)}</div>`);
const runItem = (t) => item(t, [
  `<div style="display: flex; justify-content: space-between; gap: 12px; align-items: center;"><div style="display: flex; gap: 8px; align-items: center;"><span style="font-family: ${SERIF}; font-weight: 500; font-size: ${t.S.stmt}px; white-space: nowrap;">Attempt 2</span>${chip(t, 'On Hold', 'ink')}</div>${link(t, 'Open run history')}</div>`,
  reviewSection(t),
  col(t.S.mod, kw(t, 'Milestones'), railRow(t, 'current', 'Review · Round 2', 'Aug 11, 10:10 PM'), railRow(t, 'prior', 'Implementation · Pass 2 · Round 1', 'Aug 11, 10:01 PM'), railRow(t, 'prior', 'Review · Round 1', 'Aug 11, 9:58 PM')),
  disc(t, 'Run Details', 'Implementation · retry of attempt 1'),
  disc(t, 'Technical Details', 'run · parent · superseding · verification item')
].join(''), '14px 16px', 'gap: 14px;');
const taskHeader = (t, stop) => { const parts = [eyebrow(t, 'Task timeline', chip(t, stop === 'requested' ? 'Stopping' : 'On Hold', 'ink')), ask(t, 'Repair the acceptance rig')];
  if (!stop) parts.push(sysBar(t, `<span style="font-size: ${t.S.body}px;">Round 1</span><span style="font-family: ${SERIF}; font-size: ${t.S.stmt}px; line-height: 1.4;">Hold: Verification findings block publication</span>`), `<div>${link(t, 'Review the pull request in Inbox')}</div>`);
  if (stop === 'requested') parts.push(notice(t, 'Requested', 'Stop sent; waiting for the daemon to confirm.', 'neutral'));
  if (stop === 'unconfirmed') parts.push(notice(t, 'Unconfirmed', 'The daemon did not answer the stop. Nothing is assumed.', 'accent'), disc(t, 'What Happened'));
  if (stop === 'failed') parts.push(notice(t, 'Failed', 'The task did not stop. Execution may continue.', 'wax'), disc(t, 'What Happened'));
  if (stop === 'recorded') parts.push(notice(t, 'Recorded', 'Stop confirmed by the daemon. Existing history and PRs remain available.', 'neutral'));
  parts.push(disc(t, 'Technical Details', 'freeside · #724 · Active · freeside-ai/freeside#724'));
  const ctl0 = stop === 'unconfirmed' ? row(btnOut(t, 'Retry Sending Stop', false, false), btnOut(t, 'Refresh Task Status', false, false)) : stop ? row(btnOut(t, 'Refresh Task Status', false, false)) : '';
  const ctl = ctl0 ? ctl0.replace('display: flex; gap: 8px;', 'display: flex; gap: 8px; flex: none;') : '';
  parts.push(`<div style="display: flex; gap: 8px; align-items: center; flex-wrap: wrap;">${ctl}<span style="color: ${t.dim}; font-weight: 500; font-size: ${t.S.body}px; padding: 0 ${ctl ? 6 : 0}px;">More Actions ▾</span></div>`);
  return col(10, ...parts); };
const taskTimeline = (t, stop) => card(t, `
  ${taskHeader(t, stop)}
  ${col(12, disc(t, 'Campaign', 'current · 2 attempts · from Oct 5', {open: true}), runItem(t))}
  ${col(10, `<div style="border: 1px solid ${t.itemB}; border-radius: 8px; padding: 9px 14px; display: flex; justify-content: space-between; align-items: center; gap: 12px;"><div style="display: flex; gap: 8px; align-items: center;">${chev(t)}<span style="font-size: ${t.S.label}px; color: ${t.dim}; white-space: nowrap;">Attempt 1</span>${chip(t, 'Execution Failed · Superseded', 'faint')}</div>${mono(t, 'Sep 11, 4:58 PM', t.S.sum, t.dim)}</div>`, disc(t, 'Campaign 3a1c', '1 run · Sep 10'), disc(t, 'Task Events', '6 recorded · newest Sep 12, 9:41 AM', {open: true}), `<div style="padding-left: 18px; display: flex; flex-direction: column; gap: 8px;">${railRow(t, 'current', 'Review round 3 · findings', 'Sep 12, 9:41 AM')}${railRow(t, 'prior', 'Specification approved', 'Sep 11, 1:40 PM')}${railRow(t, 'prior', 'Task created from freeside#724', 'Sep 10, 9:02 AM')}</div>`)}
`);
// Stop-state header excerpts (7.5 / R12): the notice states; Retry stays a button.
const stopExcerpt = (t, stop) => `<div style="width: 520px; background: ${t.ground}; border: 1px solid ${t.winB}; border-radius: 10px; padding: 18px 20px; font-family: ${SANS}; font-size: ${t.S.body}px; line-height: 1.5; color: ${t.ink};">${taskHeader(t, stop)}</div>`;

// 6.7 operational summary, top-leading at the card's x and width.
const opSummary = (t) => card(t, `
  ${col(10, eyebrow(t, 'Inbox', chip(t, '1 Urgent', 'wax')), ask(t, '14 open items across 3 projects'))}
  ${col(t.S.mod, kw(t, 'Needs you first'), fact(t, 'Highest Priority', link(t, 'Execution failure · #724')), fact(t, 'Waiting Longest', link(t, 'Spec approval · #731')))}
  ${col(t.S.mod, kw(t, 'Tasks'), fact(t, 'Active', link(t, '3')), fact(t, 'Pending Stops', '1'))}
  ${folds(t, disc(t, 'Freshness', 'Updated recently · 15-second heartbeat'))}
`);
const emptyInboxSidebar = (t) => sidebar(t, sidebarControls(t, 0, 0, ['Open', 'Resolved', 'All'], '', 'freeside-docs') + emptyState(t, 'archivebox', 'No open items', 'Nothing in this project needs you.', '', 'archivebox'));
const emptyTasksSidebar = (t) => sidebar(t, sidebarControls(t, 1, 0, ['Active', 'Finished', 'All'], '', 'freeside-docs') + emptyState(t, 'checklist.checked', 'No active tasks', 'Nothing is running in this project.', '', 'checklist.checked'));

// 6.8 banners: a notice states; its action is a button on its own line. Revoked also empties the detail pane around a filled Pair Again.
const bannerStack = (t, which) => col(8, ...[
  notice(t, 'Stale', 'Last synced 14 minutes ago. Showing cached items; actions are disabled until sync resumes.', 'accent'),
  notice(t, 'Stopped', 'At the last successful refresh, unattended operation was stopped by operator decision. 1 other stop is in force. The current state is unknown.', 'wax', 'Review to Resume'),
  which === 'revoked' ? notice(t, 'Revoked', 'This device’s access was revoked. Cached items stay readable; actions are disabled.', 'wax', 'Pair Again') : '',
  notice(t, 'Mismatch', `Daemon contract <span style="font-family: ${MONO}; font-size: ${t.S.sum}px;">3f9a1c2e</span>, app <span style="font-family: ${MONO}; font-size: ${t.S.sum}px;">7b40d2aa</span>. Update the daemon or the app.`, 'accent')
].filter(Boolean));
const macBanners = (t) => macWindow(t, {title: 'Inbox', sidebar: inboxSidebar(t, ''), toolbarTrailing: `<span style="font-size: 11.5px; color: ${t.accT}; font-weight: 500;">Updated 14 min ago</span>`, detail: `<div style="width: ${t.S.card}px;">${bannerStack(t)}</div>${opSummary(t)}`, detailStyle: 'padding: 14px 24px 40px;', minHeight: 760});
const macRevoked = (t) => macWindow(t, {title: 'Inbox', sidebar: inboxSidebar(t, ''), toolbarTrailing: `<span style="font-size: 11.5px; color: ${t.accT}; font-weight: 500;">Updated 2 h ago</span>`, detail: `<div style="width: ${t.S.card}px;">${notice(t, 'Revoked', 'This device’s access was revoked. Cached items stay readable; actions are disabled.', 'wax')}</div>${emptyState(t, 'lock', 'This device is no longer paired', 'Pair again to act on items. Cached items stay readable.', btnFill(t, 'Pair Again', false, false), 'lock.slash')}`, detailStyle: 'padding: 14px 24px 40px;', minHeight: 620});

// New Task on the Mac (⌘N), two states: an empty form has no forward action yet, so Submit is disabled; once valid, Submit fills — identical in both modes.
const macSheet = (t, inner, footer, w) => `<div style="width: ${w || 520}px; background: ${t.g2}; border: 1px solid ${t.winB}; border-radius: 12px; box-shadow: 0 18px 48px rgba(0,0,0,0.22); display: flex; flex-direction: column; font-family: ${SANS}; font-size: ${t.S.body}px; color: ${t.ink};"><div style="padding: 22px 22px 18px; display: flex; flex-direction: column; gap: ${t.S.sec}px;">${inner}</div><div style="display: flex; justify-content: flex-end; gap: 8px; border-top: 1px solid ${t.rule}; padding: 12px 22px 16px;">${footer}</div></div>`;
const newTaskSheet = (t, valid) => macSheet(t, col(10, kw(t, 'New task'), ask(t, 'What should the agent work on?')) + col(12, field(t, 'Project', 'freeside'), field(t, 'Work to do', valid ? 'Rename the review-round metrics so the inbox summary reads them without a lookup, and update the two fixtures that pin the old names.' : '', valid ? '' : 'Describe the task…', true, !valid), field(t, 'Name (optional)', '', 'Name (optional)')), btnOut(t, 'Cancel', false, false) + (valid ? btnFill(t, 'Submit', false, false) : btnDisabled(t, 'Submit', false)));

// 5.8 menu-bar panel: Stopped states; Review to Resume is a button.
const menuRow = (t, label, trailing, state) => `<div style="display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 6px 10px; border-radius: 6px; font-size: 13px; ${state === 'hover' ? `background: ${t.hover};` : state === 'focus' ? `box-shadow: inset 0 0 0 1px ${t.acc};` : ''}"><span>${label}</span>${trailing || ''}</div>`;
const menuPanel = (t) => `<div style="width: 340px; display: flex; flex-direction: column; align-items: flex-end; gap: 6px;">
  <div style="height: 28px; display: flex; align-items: center; gap: 14px; padding: 0 12px; background: ${t.name === 'day' ? 'rgba(243,238,225,0.85)' : 'rgba(30,24,18,0.85)'}; border-radius: 6px; font-family: -apple-system, 'SF Pro Text', 'Helvetica Neue', sans-serif; font-size: 13px; color: ${t.ink};"><span style="display: inline-flex; align-items: center; gap: 4px; background: ${t.name === 'day' ? 'rgba(43,36,22,0.08)' : 'rgba(234,227,207,0.1)'}; border-radius: 5px; padding: 3px 7px; position: relative;"><img src="${t.keyImg}" alt="" style="height: 16px; width: auto;" /><span style="position: absolute; right: 3px; top: 3px; width: 7px; height: 7px; border-radius: 50%; background: ${t.waxT}; box-shadow: 0 0 0 1.5px ${t.g2};"></span></span><span>Wed Oct 8 &nbsp;9:41 AM</span></div>
  <div style="width: 340px; background: ${t.g2}; border: 1px solid ${t.winB}; border-radius: 12px; padding: 8px; display: flex; flex-direction: column; gap: 3px; font-size: 13px; color: ${t.ink}; box-shadow: 0 14px 34px rgba(0,0,0,0.22); font-family: ${SANS};">
    ${menuRow(t, 'Open Freeside')}
    ${menuRow(t, 'Show Inbox', `<span style="display: flex; gap: 8px; align-items: center;">${mono(t, '14', t.S.sum, t.dim)}${chip(t, '1 Urgent', 'wax')}</span>`, 'hover')}
    <div style="border-top: 1px solid ${t.rule}; margin: 4px 4px;"></div>
    <div style="padding: 6px 10px 6px; display: flex; flex-direction: column; gap: 10px;">
      ${kwRow(t, 'Daemon', chip(t, 'Running', 'ink'))}
      <div style="display: flex; flex-direction: column; gap: 3px;"><div>The supervised daemon is running on this Mac.</div><div style="font-family: ${MONO}; font-size: ${t.S.sum}px; color: ${t.dim};">freesided 0.9.3 · from Oct 8, 8:12 AM</div></div>
      ${notice(t, 'Stopped', 'Unattended operation is stopped by operator decision.', 'wax', 'Review to Resume')}
    </div>
    ${menuRow(t, 'Stop Daemon', '', 'focus')}
    <div style="border-top: 1px solid ${t.rule}; margin: 4px 4px;"></div>
    ${menuRow(t, 'Quit Freeside', `<span style="font-family: ${MONO}; font-size: ${t.S.sum}px; color: ${t.dim};">⌘Q</span>`)}
  </div>
</div>`;

BANDS.push({id: 'a', title: '1 · Mac at the native ladder', note: 'ask 20 · statement 15 · label and body 13 · mono 12 · keyword 11 · buttons 28 · one row grammar for Inbox and Tasks · detail pane top-leading · notices state, buttons act', frames: (t0) => { const t = mac(t0); return [
  frame({width: 1180, label: `Mac ${t.name} — Inbox → finding adjudication`, caption: cap(`macOS · ${t.name}`, 'Inbox → finding adjudication, the reference card', 'issues 1 · 6'), html: macWindow(t, {title: 'Finding Adjudication', sidebar: inboxSidebar(t, 'finding'), detail: card(t, findingCard(t))}), note: 'Card at 640 from the pane’s leading edge, where the operational summary also starts. Per-finding submit not drawn (main, #1863); run-in FINDING N heading as built.', diamonds: [['7.1', 'First-viewport budget: the Mac ladder lands the actions at about 470pt — well inside 520 — so the stacked heading could return. Owner call.']]}),
  frame({width: 1180, label: `Mac ${t.name} — Inbox → ready for final review`, caption: cap(`macOS · ${t.name}`, 'Inbox → ready for final review (4b at the Mac ladder)', 'issues 1 · 6'), html: macWindow(t, {title: 'Ready for Final Review', sidebar: inboxSidebar(t, 'final'), detail: card(t, finalCard(t))}), diamonds: []}),
  frame({width: 1180, label: `Mac ${t.name} — Tasks → task timeline`, caption: cap(`macOS · ${t.name}`, 'Tasks → task timeline, task rows on the inbox row grammar', 'issues 1 · 2 · 6'), html: macWindow(t, {title: 'Task Timeline', sidebar: tasksSidebar(t, 'rig'), detail: taskTimeline(t)}), note: 'Task row = status as the keyword line (accent when an open Inbox item is bound to the task, faint when finished or superseded, ink otherwise) with AGENT trailing · name in serif · project · issue · current phase · round, time trailing · the one guidance link. The four-phase sentence and the hold text leave the row for the page’s hold callout and Milestones; the current phase stays, in the context line’s slot.', diamonds: [['6.1', 'Row budget (R31): the per-phase sentence and hold text off the row, current phase and round kept in the context line (drawn) — or the full phase sentence kept as SURFACES.md lists it. Changes what the row says, so it is a SURFACES.md line.']]}),
  frame({width: 1180, label: `Mac ${t.name} — empty detail, operational summary`, caption: cap(`macOS · ${t.name}`, 'Inbox with nothing selected — operational summary, top-leading', 'issue 6'), html: macWindow(t, {title: 'Inbox', sidebar: inboxSidebar(t, ''), detail: opSummary(t), minHeight: 640}), note: 'The summary and every card now share one x, one max width (640) and one bordered card, so selecting an item changes the content, not the shape. The task and run timelines take the same card.', diamonds: []}),
  frame({width: 1180, label: `Mac ${t.name} — empty scopes`, caption: cap(`macOS · ${t.name}`, 'Inbox and Tasks, project filter with no items — system glyphs return', 'issue 5'), html: `<div style="display: flex; gap: 24px;">${macWindow(t, {title: 'Inbox', sidebar: emptyInboxSidebar(t), detail: emptyState(t, 'archivebox', 'No open items', 'Nothing in this project needs you.', '', 'archivebox'), detailStyle: 'padding: 0; align-items: stretch;', minHeight: 560, width: 578})}${macWindow(t, {title: 'Tasks', sidebar: emptyTasksSidebar(t), detail: emptyState(t, 'checklist.checked', 'No active tasks', 'Nothing is running in this project.', '', 'checklist.checked'), detailStyle: 'padding: 0; align-items: stretch;', minHeight: 560, width: 578})}</div>`, note: 'R13 revised: the SF Symbol named under each glyph: <code>archivebox</code> for Inbox (everything filed; chosen 8 Oct from <a href="Tray Glyphs - 8 Oct 2026.dc.html">Tray Glyphs</a> 2c), <code>checklist.checked</code> for Tasks (the tab’s symbol with every item ticked), <code>lock.slash</code> for Revoked, faint ink at ~28pt, over the serif line and the dim line. The key mark stays on the menu-bar status item only.', diamonds: []}),
  frame({width: 1180, label: `Mac ${t.name} — banner stack`, caption: cap(`macOS · ${t.name}`, 'Freshness banners · Stopped · Mismatch over the summary — actions are buttons', 'issue 3'), html: macBanners(t), note: 'R14 revised: a notice states; it never carries a text action. Where a notice has an act, the act is an outlined button on its own line under the sentence (wax outline when the notice is wax).', diamonds: []}),
  frame({width: 1180, label: `Mac ${t.name} — revoked`, caption: cap(`macOS · ${t.name}`, 'Revoked — the detail pane empties around Pair Again', 'issues 3 · 5'), html: macRevoked(t), note: 'Revoked is the one state in which the pane has nothing else to offer, so Pair Again is the pane’s filled control; the banner only states. Cached items stay readable in the sidebar.', diamonds: [['6.8', 'Pair Again in the pane (drawn) or as a button in the banner alone.']]}),
  frame({width: 1180, label: `Mac ${t.name} — stop states`, caption: cap(`macOS · ${t.name}`, 'Task header stop states (7.5) — the notice states, Retry Sending Stop stays a button', 'issues 1 · 3'), html: `<div style="display: flex; gap: 24px; flex-wrap: wrap;">${stopExcerpt(t, 'requested')}${stopExcerpt(t, 'unconfirmed')}${stopExcerpt(t, 'failed')}${stopExcerpt(t, 'recorded')}</div>`, note: 'Undoes the 6 Oct move of Retry into the Unconfirmed notice. The control group holds Retry Sending Stop beside Refresh Task Status; More Actions keeps Stop Task.', diamonds: []}),
  frame({width: 1080, label: `Mac ${t.name} — New Task sheet`, caption: cap(`macOS · ${t.name}`, 'New Task (⌘N) — empty, then valid', 'issues 1 · 4'), html: `<div style="display: flex; gap: 40px; align-items: flex-start;">${newTaskSheet(t, false)}${newTaskSheet(t, true)}</div>`, note: 'Fill rule for sheets (R6): Submit fills only when the form is valid; an empty form has no forward action yet, so Submit takes the disabled recipe (faint ink on the rule border). The same two states in both modes — dusk below must match.', diamonds: []}),
  frame({width: 340, label: `Mac ${t.name} — menu-bar panel`, caption: cap(`macOS · ${t.name}`, 'Menu-bar panel — Stopped notice with Review to Resume as a button', 'issues 1 · 3'), html: menuPanel(t), diamonds: []}),
]; }});
