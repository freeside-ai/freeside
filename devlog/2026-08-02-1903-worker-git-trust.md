# Worker Git Trust Boundary

## Decision

Chose a command-scope Git protected-config exception for exactly
`/workspace`, alongside the fixed local author identity, because the enabled
Claude launcher deliberately gives repository contents to UID 1001 while
keeping the sticky workspace root owned by root. Git therefore refuses the
repository as dubious before it reads the seeded local config. The fixed
`GIT_CONFIG_COUNT` entry reaches Claude and every tool it spawns without
trusting any other repository path or changing the daemon-authored published
history.

Rejected a repository-local `safe.directory` entry because the ownership gate
runs before Git trusts local config. Rejected changing `/workspace` to UID 1001
because ownership of the sticky directory would let the writer remove
root-owned control entries. Rejected `safe.directory=*` as broader than the
one daemon-created workspace, and rejected an image-baked system config because
the requirement belongs to the launch topology rather than either provider
base.

Work unit: #469, PR #476.

## Verification Finding

The original worker-environment audit established the missing identity only
statically. Live reproduction first confirmed `git commit` failed for missing
identity in both shipped agent images. After adding the identity, reproducing
the production ownership split under UID 1001 exposed Git's independent
`detected dubious ownership` refusal. The permanent live regression now runs
the checkpoint commit through ward with that root-owned/UID-1001 topology and
the exact protected-config exception, and passed in both shipped agent images.
In the same unprivileged process, Git still refused a second root-owned
repository outside `/workspace`, confirming the exception did not broaden to
other paths.

## Revisit When

The workspace target, privilege-drop UID, sticky control-directory topology,
or Git's protected-config semantics change, or another enabled provider uses a
different ownership boundary.
