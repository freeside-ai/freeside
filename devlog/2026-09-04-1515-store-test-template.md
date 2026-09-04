# Copy Migrated Databases for Test Fixtures

The owner-approved plan for [#1146](https://github.com/freeside-ai/freeside/issues/1146)
chooses a migrated file template for routine store fixtures. Every fixture
previously paid for the full migration history, even when its subject was an
unrelated store operation. Planning measurements on the ten-core Mac put a
fresh open at about 84 ms normally and 2.5 s under the race detector; opening
a migrated copy took about 3 ms and 64 ms respectively.

The template omits epoch seeding. Independent copies therefore receive
independent epochs through the normal open path, while existing files keep
their rows and epoch on reopen. Migration and open-contract tests retain
raw opens. Read-only validation proves the copied file has the complete
embedded migration history before a normal open could repair it. This also
checks the assumption that closing the last SQLite connection checkpoints
the WAL into the file we copy.

The shared helper must live in a non-`_test.go` file so other test packages
can import it. The production-open guard therefore recognizes that one raw
open and rejects imports of `storetest` from production files. Test support
does not create another production path around topic-key setup. The template
builder removes only its own unique temporary directory, never a caller path.

Reexecuted kill-test helpers cannot share the parent's in-memory template.
Their parents prepare and close the database before launching the child, so
the readiness deadline covers the durable write being tested rather than
another full migration. The child still commits the state before it is killed.

In-memory SQLite would change file and reopen semantics without addressing
the CPU cost of migrations. Serializing packages with `-p` would change
contention without making individual tests faster. A job matrix would also
break the race observer's contract of one `linux (race test)` job. These
alternatives remain outside this change.

The race command keeps the 40-minute timeout and disables test-result caching
so three post-merge runs measure the full suite under contention. A separate
PR can lower the timeout once those measurements exist; pre-merge local
timings cannot establish the shared runner's margin.

Revisit when a migration depends on `Options` or a seeded epoch, or SQLite's
close/checkpoint behavior changes.
