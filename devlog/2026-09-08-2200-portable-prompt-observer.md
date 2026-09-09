# Portable Prompt Observer

Chose `find -exec sh -c 'printf x' \;` over GNU `find -printf x` for the
protected-prompt observer because the pinned Alpine exporter uses BusyBox.
The observer must prove exactly one directory entry before it can attest the
prompt's type, ownership, modes and bytes. BusyBox rejects `-printf`, so the
old command exits before writing its proof and Ward correctly refuses writer
admission.

The missing-proof error comes from the shared state-proof reader; it does not
identify the prompt observer by itself. The original generated command failed
in the exact cached exporter image, with synthetic input and no network or
mounts. The corrected command in that same image produced the exact nonce and
input digest. The temporary containers were registered through the held-rig
API and removed after ownership checks. This proves command compatibility,
not the pending retained-run Retry or successor publication.

The Mac script test previously rewrote `find -printf` before executing it,
masking the production incompatibility. It now runs the same find expression
as production. Only test ownership, paths, BSD stat and checksum spelling are
adapted. The original implementation fails this regression; the corrected
implementation preserves rejection of unexpected prompt-volume contents.

The entry marker runs through the already-required `sh`. A direct
`find -exec printf` would add an external executable dependency that exporter
preflight does not guarantee: a shell builtin passes shell lookup but cannot
be launched by `find`. The script regression restricts observer PATH to its
external tools, without `printf`, and verifies both proof and extra-entry
refusal. No image probe or approval requirement changes.
An old/new comparison of 40 generated directory cases also preserved exact
entry markers, including shell-like and newline names.

Independent refutation checked empty, single and multiple entries, hidden and
unusual names, nested directories, dangling symlinks and FIFOs. No entry name
is passed to `printf`; each extra entry still emits a second marker and fails
the exact-`x` gate. The proof, ownership, mode and read-only mount checks remain
unchanged. No actionable review finding was identified.

Rejected adding GNU find to the exporter: the portable expression preserves
the existing single-entry test without changing images or approvals. Rejected
accepting a missing proof or weakening its checks: an unavailable observer
cannot authorize writer launch. General failure-cause propagation remains
the separate work tracked in #574.

Revisit when the pinned exporter's command set changes or a new observer
requires an extension that cannot be expressed portably. Live exit acceptance
remains on #1235 and #1001, separate from this implementation fix (#1255).
