# Preserve User Prompt Semantics Across Transport Changes

Chose a Ward-owned prompt file and stdin delivery over raising the shell
argument limit or appending the stage prompt to system instructions. The
failed operator-feedback prompt exceeded the old transport's 31-KiB bound.
That bound protects shell argv; weakening it would not establish a reliable
delivery channel. Moving content into the system role would change what the
model is being asked to trust. A separate finite 1-MiB file transport preserves
the complete user input without either change.

The owner explicitly approved the named recovery chain: protected prompt
delivery, then same-run feedback retry through verification, review, and
expected-old-head publication. The owner also extended the narrow #1213
serialization exception through those repairs while retaining its live
acceptance. Each contract PR still requires human merge. This unit does not
implement retry or mutate the failed attempt. Follow-up: #1235.

Persist delivery mode before execution. Missing mode means the old argument
protocol, including its original refusal and handoff identity. Omitting new
fields from old JSON keeps existing journal hashes stable. A failed old intent
cannot become a new attempt merely because code changed.
Comparing the original and revised argument launcher over 1,000 generated
inputs, including quoting, UTF-8 and preparation commands, produced identical
command arrays.

Use controlled rootfs copy into a networkless seeder because runtime copy
directly into a mounted volume is not reliable on the supported runtime. A
separate credential-free observer proves the actual file shape, root ownership,
mode and byte digest. Commit its fingerprint and digest with the existing
single prelaunch state mark, before writer creation. The writer receives a
read-only mount outside the workspace; its root shell opens stdin before
dropping privileges. Recovery requires the same digest and owned object.

Independent refutation confirmed that fresh ext4 volumes contain `lost+found`.
The first seeder left it behind, causing the exact-manifest observer to refuse
every normal launch. Reusing the existing guarded removal fixes this: only an
empty, non-symlink directory with mode 0700 and root ownership is removed.
Generated-script checks cover that shape and reject populated, linked,
wrong-mode and extra entries. Non-root host fixtures normalize ownership to
their test user; they do not establish container enforcement by themselves.

The optional pinned-CLI probe uses synthetic input and an isolated loopback
API. Refutation identified a gap between registering its resource and creating
the container: registration alone does not prevent the rig holder from
releasing ownership. The probe now retains the existing rig authorization
through cleanup. It also requires affirmative observation of isolation fields,
so absent runtime metadata cannot stand in for a network/forwarding proof.

The pinned Claude 2.1.220 probe consumed 70,003 synthetic UTF-8 bytes as an
exact user-message block and completed successfully. The unprivileged process
could not separately open the root-owned file. Runtime inspection, rather than
`logs --follow`, establishes completion because that command returned early
on the supported runtime. Auxiliary service requests receive HTTP 404 from
the isolated mock; only the exact inference input and successful final result
establish transport compatibility. This proof uses no real credential and is
separate from the target run's remaining recovery acceptance.

Revisit when a different provider needs another user-input channel, the
rendered prompt exceeds the file bound, or the supported runtime's copy and
mount behavior changes. Keep transport changes separate from retry authority.
