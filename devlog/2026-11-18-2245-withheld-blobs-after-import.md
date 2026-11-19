# Require No Withheld Blob After Import

The owner chose, during the #2382 exit run, to fix this in-run (#3427).

## Decision

Chose to skip a blob the export withheld wherever the daemon persists,
materializes, or backs up a replay's repository blobs, over keeping the
refusal and stopping the ignored file from reaching the export. The importer
reads no content for a withheld entry: it decides from the manifest and the
base alone. An unchanged file is elided, a base-ignored addition is excluded,
and changed content is the blocking `blob_omitted` finding. The publication
replay reruns that same import and must reproduce the recorded head with
only advisory findings, so the replay still binds the decision. A stored
entry with no digest still refuses.

## Why

The refusal in `persistRepositoryBlobs` assumed a successful import could
not have accepted a withheld blob. That stopped being true once the importer
excluded base-ignored additions (452a4d4b5, #3399), whose blobs export
withholds first. On the run, the #2968 writer ran the app's Swift tests in
the ward, which left an ignored test bundle under `app/.build/`. The import
accepted the export, then persistence refused it. The stage driver treats
that refusal as retryable, so the run sat at `import_pending` for the
32-minute recovery grace and then failed. `materializeReplay` and the backup
digest set made the same assumption and would have failed next.

## Rejected

- **Keeping the refusal and filtering debris before export:** the export
  can't know what the importer will exclude, because only the exact trusted
  base's ignore rules decide it, and export's budget order is only a hint.
- **Persisting only the blobs the import's result names:** the replay reruns
  the importer over the whole manifest, which reads every stored blob,
  including excluded ones, so it needs those blobs too.
- **Raising export's caps:** any cap leaves the same wedge for the next
  larger build output.

## Tests

`TestPersistRepositoryBlobsSkipsWithheldBlobs` (stage),
`TestProductionPublishesPastAWithheldIgnoredBlob` (integration, real export
and import through publication), and
`TestPublicationBackupLeavesOutWithheldBlobs` (engine). Each fails without
its part of the change.

## Follow-Up

A stage recovery that fails the same way stays silent for its 30-minute
grace before the operator sees anything (#3428).
