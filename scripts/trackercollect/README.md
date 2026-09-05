# Trackercollect

`trackercollect` gathers advisory, stamped GitHub evidence for Freeside's
post-merge tracker reconciliation. It writes `snapshot.json` and `report.md`;
it never writes to the forge and does not decide wave state, path overlap,
startability, or mergeability.

The runtime requires Go 1.26.6 and an authenticated `gh` CLI with access to the
target repository. Run it from this module:

```sh
go build -o /tmp/trackercollect .
/tmp/trackercollect --repo github.com/freeside-ai/freeside --pr 910 --out /tmp/trackercollect-910
```

The pull request must already be merged. The command exits 0 for a complete
collection, 2 after writing artifacts that contain one or more `AMBIGUOUS`
entries, and 1 for a hard failure. A non-merged pull request fails before the
output artifacts are written. A prompt-backed direct unit has no forge issue
that proves its origin, so pass `--direct` to assert that provenance. Without
the flag, zero attributed closing issues produce an `AMBIGUOUS unit-origin`
entry; using the flag when the forge attributes a closing issue is a hard
failure.

Run the built binary when its exact exit code is required. `go run` reports a
child exit status but returns its own nonzero status, so it cannot preserve the
collector's distinction between exit 1 and exit 2.

Every GraphQL connection has a finite page cap. Reaching it preserves the
partial evidence, records an `AMBIGUOUS` truncation entry, and returns exit 2.
Before using a snapshot for a tracker edit, follow the inventory-aware
freshness recheck in `docs/coordination.md`.

