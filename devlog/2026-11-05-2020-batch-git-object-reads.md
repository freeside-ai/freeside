# Batch Git Object Reads

Issue #2669 adds `gitrun.ObjectReader`, a batch reader over a pinned
checkout's object store, and moves `agenttree.ReadCommit` onto it. Several
daemon read paths started one `git cat-file` per object; on macOS each
process start costs about 6 ms quiet and 10 to 60 ms under load, and
`ReadCommit` alone made 396 of 725 git calls in a sampled cmd/freesided test
run. The reader carries untrusted object bytes into Tier A gates (agent-tree
admission now; the importer and verifier in #2680 and #2681), so it is Tier A
and joins the AGENTS.md path list.

## Decisions

- **Two long-lived `cat-file` processes over one process per object.**
  Chose `cat-file --batch-check` for size and type questions and
  `cat-file --batch` for content, each started on first use, so a size check
  never makes git stream an object. Per-object processes were the cost being
  removed.
- **Two processes over `--batch-command`.** `--batch-command` would serve
  both questions from one process, but it needs git 2.36, and the daemon
  declares no git floor. Two processes cost one extra start per reader.
- **Git's own plumbing over a Go git library.** A library would remove the
  process entirely but would replace git's object parsing, replace-object
  handling, and config hardening (`gitrun.Baseline`,
  `GIT_NO_REPLACE_OBJECTS=1`) with a second implementation the Tier A gates
  would then have to trust. The reader inherits all of that from the pinned
  runner.
- **Full hex object IDs only.** The reader refuses anything but a full
  lowercase hex ID of the pinned object format, before starting or writing
  to git, so a request names exactly one object, `<rev>:<path>` and other
  revision syntax never reach git's name resolution, and git's echoed ID is
  checkable. An unpinned runner's reader refuses every call, since git would
  otherwise discover a repository from the scratch directory.
- **Every error is final.** A malformed header, a missing object, an echo
  of another ID, a short body, a bad delimiter, git's death, a refused ID,
  content the caller left unread, and cancellation of the session or of one
  call all stop both processes and make every later call return the first
  error. A reader that continued after any of these could pair a response
  with the wrong object. Callers that want to skip a large object ask `Info`
  first instead of opening it.
- **Cancellation kills the process group.** Each call watches its own
  context with `context.AfterFunc`, and an `Open` call's watch lasts until
  its content and delimiter are read, so a cancelled call cannot hang on the
  pipe. The session context governs the processes themselves. Close kills
  rather than waiting for git to exit: every response already returned was
  checked, so the exit status adds nothing.
- **Tag peeling is now refused (a deliberate tightening).** `cat-file blob
  <oid>` peeled a tag to its blob, so an agent-tree entry with blob mode that
  named a tag object read the tagged blob. `--batch` does not peel, and
  `ReadCommit` now requires the object to be a blob of the size `ls-tree -l`
  listed, so that entry fails with `ErrMalformed`. No tool writes such a tree;
  only a hand-built object can, and admission should not follow it.

## Refute-First Findings

- **Old and new `ReadCommit` agree.** A scratch comparison ran the old
  per-object implementation and the new one over 129 sources: 120 generated
  commits (random files under each listed path and outside them, empty and
  boundary-size files, symlinks, executables, and gitlinks) and the refusal
  cases (a branch name, an abbreviation, a tag, a tree, an absent ID, and
  malformed names). Both returned identical files and error classes for
  every source (48 read, 71 malformed, 10 not a commit). The tag in a blob
  entry is the only known difference; it has its own test.
- **Size mismatch between listing and object is unreachable with a correct
  git.** `ls-tree -l` and `cat-file --batch` read the same content-addressed
  object, so the sizes cannot differ. The check stays as a cheap consistency
  guard, without a lying-git test.
- **The old peel also skipped the size cap (confirmed, closed).** A
  fresh-context review found that `ls-tree -l` lists a tag entry at the
  tag's own size, so the old peeled read never compared the tagged blob's
  length with the listed size and a tag could carry a blob past
  `MaxAgentFragmentBytes`. The type and size checks now refuse it.
- **A cancel after git answered went untested (confirmed, fixed).** Every
  cancellation test cancelled while git still owed bytes. A test now cancels
  after the content is read in full and before the delimiter check; `Info`
  shares the same watch and check, but no fake git can time its answer
  between the two without a test-only seam.
- **A session cancel racing a finished response returned success
  (confirmed, fixed).** Calls now check the session context as well as the
  call's after git answers.
- **The pending delimiter read runs under the previous call's context
  (allowed).** Git writes the delimiter with the content, so the read cannot
  stall between them.
- **A failed `StdoutPipe` leaked the stdin pipe (confirmed, fixed).** Only
  descriptor exhaustion reaches it; the stdin pipe is now closed there.

## Revisit When

- The daemon sets a git floor of 2.36 or later: one `--batch-command`
  process can then replace the two.
