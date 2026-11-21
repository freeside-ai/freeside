# A Cordoned Candidate Is Verified Cordoned

Chose to run a cordoned candidate's macOS-host checks on the daemon's host,
as the owner's account under a verifier rule file, over keeping every
candidate's checks on the slot executor. The slot executor runs two jobs at
once and needs hidden accounts, a root helper, and a reinstall to change
either. Cordoned writers (revision 134) multiply the candidates, so
verification becomes the constraint (tracker #3418). Plan revision 135 adds
the class to §5.7 and the matching rules to §§5.4, 5.6, 5.15, 6, 10, 11, and
14 (#3548, tracker #3462).

## The Owner's Decisions

From 2026-11-20, recorded on #3464:

- **A cordoned implementer's candidate is verified cordoned,** and the
  verdict is recorded as cordoned.
- **A warded candidate keeps the slot executor** and never goes to the
  cordoned verifier.
- **Most slot work pauses for now.** The owner revisits the slot executor
  later; #3136 stays open for that.
- **Verification is not the Mac mini's first job.**

## What Changed

The cordoned-verifier spike (#3526, PR #3542) found that a job on the
owner's account can't get a verdict as trustworthy as a slot's, and
recommended keeping verification on the slot executor. Its findings stand.
The owner accepts the residual it names for cordoned candidates, because the
slot's wall was set when nothing untrusted ran as the owner:

- Under revision 134 the daemon, its verdict records, and the App key
  already rest on the writer's rule file.
- A cordoned candidate has already run as the owner, in the writer that
  wrote it.
- Review, the owner's approval, and the repository's own CI still stand
  between a verdict and a merge.

## Earlier Decisions This Revises

- **Revision 134 (#3464):** "Warded runs stay for clean verification, which
  this revision leaves where it was." The plan now verifies a cordoned
  candidate's macOS-host commands cordoned. Its Linux commands stay in the
  ward.
- **The macOS verifier executor**
  ([note](2026-10-22-1205-macos-verifier-executor.md)): host-side
  verification runs on slot accounts. That stays for warded candidates, for
  mixed or adopted heads, and for a cordoned candidate the operator sends
  there.
- **PR #3542's recommendation** to keep verification on the slot executor.

## Rejected

- **All verification on the slot executor.** The spike's recommendation. It
  keeps the stronger wall for candidates that already ran as the owner, at
  the cost of two jobs at a time and an executor that is costly to change.
- **A macOS VM per job on the Studio (#3136).** Unmeasured, and the kernel
  quota allows two macOS guests per host; the reference host can't hold two
  Xcode guests beside its wards
  ([macOS verifier note](2026-10-22-1205-macos-verifier-executor.md)). The
  owner revisits it with the slot executor.
- **Verification on the Mac mini.** The owner decided it is not that
  machine's first job.

## Defaults Chosen at Planning

Each is the owner's to change in review; the rest of the revision stands if
one changes.

- **Teardown may rest on the private `sandbox_check` call.** It was the only
  finder in the spike that caught every survivor: the job's process group
  missed a `setsid` child and one that rewrote its argv, `pgrep -f` and
  `lsof +D` on the job root missed the rewrite, and a per-job launchd
  service missed both. The plan states the promise (the daemon finds every
  process a job started, by a mark the job can't shed). Conformance plants
  the spike's four survivors (`nohup`, a double fork, a `setsid`, and a
  `setsid` that leaves the job root, closes its descriptors, and rewrites its
  argv) and must find each, so a macOS update that breaks the call fails
  conformance and closes the class. Alternative: forbid private calls, which
  leaves no finder that works and so no class.
- **A cordoned candidate may go to the slot executor by the operator's
  explicit setting only,** for a project or one task, and the verdict is then
  the slot executor's. It is the stronger wall, the record stays honest, and
  it keeps a way to land work when the cordoned proof lapses. Alternative:
  no setting, so the operator sets the writer warded instead.
- **A head with mixed writers is verified on the slot executor.** A
  candidate is cordoned only when Freeside runs wrote every one of its
  changes over the trusted base and each ran cordoned, so a hand-back head
  stays off the cordoned verifier even after a cordoned run writes on top. Content from the trusted base doesn't
  count as writing, so re-entry and the handoff check keep a cordoned
  candidate's class, and its before-capture at the base runs in the same
  class. A head a warded run helped write, or one Freeside adopted without
  writing it (a hand-back), goes to the slot executor. Alternative: follow
  the last writer only.
- **Cordoned jobs get an operator-set cap of their own per instance,**
  separate from the cap on cordoned writers. A job reserves no VM and takes
  no slot, and the spike's four jobs each ran about 1.8 times slower than
  one. Alternative: one cap shared with writers.
- **A cordoned job's artifacts keep verifier provenance** and publish under
  the same policy as any verifier artifact. The class shows through the
  producing invocation's admission, so the provenance shape gets no new
  field. §14 names what this gives up: a job can render what it can read,
  such as the pasteboard or another process's environment, into an image
  that text scanning can't inspect. Alternative: hold a cordoned job's
  images back from publication until a person approves them.
- **Capabilities are named per denial group,** as revision 134 named the
  writer's, not as one capability for the whole class as the slot executor
  has.
- **The writer-side read denial covers a job's workspace, not the cordoned
  verifier's toolchain and caches.** #3548's sixth criterion can be read as
  denying both reads and writes of the toolchain and caches. The spike's
  writer additions deny reads of the verification root (another task's
  candidate) and writes to the toolchain and caches; reading a toolchain
  exposes nothing. The plan follows the spike.

## Names

The contract unit registers what the plan names, so a later rename is
another revision.

- **Class:** `cordoned_host_verification`, beside
  `credential_free_host_verification` and `cordoned_host_process`.
- **Capabilities:** `supports_cordoned_verification_read_denial` (denial 1),
  `supports_cordoned_verification_confinement` (2, 3, and 5),
  `supports_cordoned_verification_host_protection` (4), and
  `supports_cordoned_verification_job_end_proof` (6).
- **Writer side:** `supports_cordoned_daemon_protection` keeps its name and
  proves more: no read or write of a job's own directories (workspace,
  home, and private temporary and cache directories), no write to the
  cordoned verifier's toolchain and caches or to the global preferences a
  job reads, and no bind of a job's reserved ports.

## Evidence

[`docs/spikes/cordoned-verifier.md`](../docs/spikes/cordoned-verifier.md)
(#3526, PR #3542), on the reference Mac Studio under macOS 27.2 and Xcode
27.2:

- **The whole `macos_host` recipe passed** under the final verifier file as
  the owner, with the owner's home unreadable: format, `app test`, the Mac
  and iOS builds, and convergence.
- **The denials the plan states as facts** each trace to a probe: home,
  other jobs' workspaces, and preference reads refused; writes confined but
  for the owner's temporary directory and Clang module cache; signal,
  `launchctl` verbs, and debugger attach refused; network only at the job's
  reserved ports, unix sockets outside the job refused, DNS empty, outbound
  off-host refused; the CoreSimulator services denied with the iOS build still
  passing; and sandbox membership finding every planted survivor.
- **What the spike didn't show,** written into the plan as requirements on
  the room's proof: that a job can't start a program of its choosing through
  launchd or Launch Services, that it can't reach the container runtime's
  service, and that its rule file can deny the window server while
  in-process renders still pass. If the last fails, the screen stays a
  residual. Nor did it probe inbound connections: the sandbox filters by
  port, not by address, so a job's listener on a reserved port takes
  connections from writers (whose additions deny binding the port, not
  connecting to it) and from off the host, with no packet filter in front.
  The plan names that as a residual rather than a denial.
- **Residuals no rule closes:** the shared temporary directory and module
  cache (write access shown; no poisoned module built), any process's
  environment readable through `KERN_PROCARGS2`, the pasteboard (denying the
  service fails two of the app's tests), window capture reaching the whole
  screen, helpers launchd starts for a build (`ibtoold`), which carry no
  sandbox mark, so the end-of-job proof can't find them and whether they
  outlive a job is unmeasured, no packet filter without root, and no uid to
  reap by.
- **Density:** four jobs ran the Mac build beside four cordoned writers with
  no failure, each job about 1.8 times one job's time and each writer about
  1.3 times.

The rule file's text, parameters, and path lists stay in the report, and
later in the daemon's package docs, not in the plan.

## What the Planner Updates

This revision gives other units more to carry; their contracts and tracker
#3462 are the planner's to edit, not this PR's:

- **#3466** (the writer rule file) and **#3470** (its proof) carry the
  writer-side additions above.
- **#3550** gates this class as it gates the writer class.
- **#3549** already moves verification scratch out of a writer's reach.
- **Units still to file:** the `kind:contract` unit (the class, its
  capabilities, and the isolation class on the verification record); the
  room, a second backend beside the slot room in `daemon/internal/macverify/`
  with its caches under the daemon's root; its conformance; selection by the
  writer's class, which needs #3472's record and carries the operator's
  setting; a replan of #472 around in-process renders; and image hosting for
  the EvidencePublisher.

## Refute-First Review

One fresh-context reviewer read the revision's diff and tried to refute it.
Each finding's outcome:

- **Confirmed, fixed: a hand-back head could verify cordoned.** "Every
  Freeside run that wrote it ran cordoned" holds for an adopted head with a
  cordoned run on top. The rule now requires Freeside runs to have written
  every change.
- **Confirmed, fixed: launchd helpers.** "Can't start anything through
  launchd" and "finds every process the job started" contradicted the
  spike's `ibtoold`, which launchd starts for a build outside any rule file.
  Both promises now cover what the job starts itself; the helpers and their
  possible survival are residuals.
- **Confirmed, fixed: inbound reach.** "No off-host connection" was
  measured outbound only, and a job's listener on a reserved port takes
  connections from writers and from off the host with no packet filter. The
  denial now says outbound; the rest is a residual.
- **Confirmed, fixed:** the §5.6 "same for both classes" sentence, the §11
  sentence that implied the slot executor until the class lands (the
  refusal rule governs that window), and the report link once #3542 merged.
- **Allowed, in the PR for the owner:** the writer-side toolchain read
  (Defaults Chosen at Planning) and the slot executor's "not built yet"
  claims, corrected in passing because the executor is built.
- **Confirmed, fixed in a second round:** the writer-side promise named
  "the cordoned verifier's toolchain and caches", which read as covering the
  owner's temporary directory and Clang module cache that the spike showed
  writers can write; it now names the verifier's own toolchain and caches
  and says the shared places stay writable. Two capability summaries ("can't
  read beyond itself", "can't reach another process") promised more than
  the spike showed; they now list denials 1 and 4. Denial 1 now says which
  of its rules the spike probed from a job and which come from #3463's
  writer file, denial 2 carves out the pasteboard, the inbound residual is
  marked unprobed, and the selection summaries name an uncordoned
  (granted) run beside a warded one.
- **Fixed in a third round, no blockers:** the writer-side denial names a
  job's home and private temporary and cache directories, not only its
  workspace; "nothing clean verification trusts" now excepts the two shared
  Xcode places; denial 1's heading lists what it denies; and the offline
  cache's fail-closed check and the planted-survivor conformance are
  written as requirements on the room's proof.
- **Declined:** qualifying "clean-room" in §2's goal 9. It states the goal,
  and §6 and §5.15 qualify it where verification is defined.

## Revisit When

- The owner returns to the slot executor or to #3136.
- A poisoned module, or another planted input a job reads from the shared
  per-user places, is shown in practice.
- A macOS release changes `sandbox_check` or the sandbox's inheritance.
- Image publication from cordoned jobs comes up for building.
