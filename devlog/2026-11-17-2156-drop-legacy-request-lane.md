# Drop the Legacy Production Request Lane

#3321 is family 3 of the #3266 legacy-format sweep. The owner chose to
remove the legacy production request formats and the replays that read
them, rather than keep readers for shapes no current writer produces.

## Decision

Chose to refuse three retired formats over keeping their readers:

- **Production request with no version.** The engine's decode of the
  `production_invocation_requested` marker now requires
  `freeside.production-invocation/v2`. The released v1 shape (no version,
  no publication) and the unversioned publication preview are refused as
  unreadable, not as an unsupported version, because no upgrade repairs
  them. The `Legacy` flag, the no-publication terminal path, and the
  `present` result of `ProductionInvocationPublication` went with them.
- **`freesided submit --run-id`.** The lookup-only replay of a run without
  specification intake state is gone, with the `LegacyRunID` control field
  and `engine.HasSpecificationIntakeState`.
- **`freeside-production-composition-v1` manifests and preflight's empty
  `-submission-id` mode.** Submission refuses a V1 manifest, and preflight
  outside `-launch` requires a saved submission identity.

**Kept:** the saved-submission replay (`acceptedManual`, the `retained*`
helpers) and the four `TestHistorical*` tests. They replay current saved
submissions, which current CLIs still write; only their `LegacyRunID`
steps went.

## Writers Confirmed Gone

- **Unversioned request.** The only production writer,
  `submitProductionRunTx`, has written v2 since 94654ba85 (2026-07-31).
- **V1 manifest.** Preflight has written v2 since 792edd0ad (2026-10-14).
- **Runs `--run-id` looked up.** Outside tests, an implementation run is
  created only by the specification approval path or a retry of its run;
  `SubmitProductionRun`, the entry without a specification grant, has no
  production caller. The lookup existed only for runs submitted before
  specification intake.

## Development Writers

The seed fixture and `freeside-signet-dev` still wrote the unversioned
request, and planning assumed only the version needed changing. They also
wrote stage and invocation IDs off the production lane (the engine derives
`implement-<run>` and `inv-implement-<run>`), so their marker sat at a key
the engine never reads: the engine never treated a seeded run as a
production run.

The owner chose to move both writers onto lane IDs and the v2 request over
keeping the legacy reader for them. Both now call
`engine.ProductionLaneIDs` and `engine.ProductionInvocationRequestPayload`,
the helpers the production writer uses, so they can't drift again. That
changes the IDs in `cmd/freesided/testdata/task_record.golden`, which
planning said must not move; the diff is only the lane IDs and the ids and
input digests derived from the invocation ID.

The seeded stages keep their display names (`implementation`), which the
app renders, rather than the lane's `implement`. So the engine would now
claim a seeded run by its marker but could not bind it to a stage. Chose
that over renaming the stages, because no supported flow reaches it:
`-seed-fixture` requires the driver disabled, and no engine runs without
one. A rename would change rendered app surfaces, which is outside this
unit.

## Refute-First Check

The decode is a deserializer trust boundary, so its old and new versions
ran side by side over 200,000 generated payloads (versions, identities,
publications, extra fields, trailing values, spacing, and kinds).
Confirmed: the new decoder accepts nothing the old refused, decodes
identically wherever both accept, and refuses newly only payloads with no
version, none of them as an unsupported version. Both refuse every other
input with the same upgrade classification.

## Old Databases and Backups

A database restored from a backup that holds an old marker quarantines
that run with the "could not be authenticated" notice rather than running
it, and the encrypted checkpoint's closure check refuses the marker, so the
checkpoint reads as unhealthy until the run is cleared. This is read from
the code (`productionQuarantineReason`, `BackupPayloadExtractors`), not
exercised.

**Mixed builds:** an old CLI that still sends `LegacyRunID` to a new daemon
is refused by the control socket's strict decode (unknown field). A
submission journal an old CLI saved with a `RunID` key still decodes,
because the journal reader ignores unknown keys.

## Instance Counts

On a copy of the dev instance's database: 0 production markers without a
version (0 production markers in total), and 0 composition manifests under
its state directory. `freesided check-database` passed. Planning expected a
nonzero dev count from the seed; the dev instance holds no production runs.
Prod is the owner's check under #3265.

## Removed Tests

- **`TestLegacyProductionRunFinishesWithoutPublicationAuthority`,
  `TestLegacyCompletedTerminalWithoutExportStillRequiresDriverProof`, and
  the legacy-marker case in `admissionauth`:** superseded by
  `TestDecodeProductionRequestRefusesUnversionedPayload`. The marker no
  longer decodes, so no later layer sees it.
- **`TestSubmitCommandReplaysMatchingPreSpecificationProductionRun`,
  `TestSubmitCommandRejectsCompositionRunOverride`,
  `TestSpecificationBackedLegacyMalformedPolicyDoesNotWriteBlob`, the
  `run-legacy` task-line and attachment cases, and the `LegacyRunID` steps
  of three `TestHistorical*` tests:** accepted: each covered only the
  removed `--run-id` lane. `TestSubmitUsageDocumentsResultLanes` asserts
  the flag is gone from the help.
- **`TestControlLegacySubmissionNearInputLimits`:** accepted: it sent a
  control request above 20 MiB through the legacy lookup, and no current
  submission reaches that size, because the specification request is capped
  at 1 MiB.
- **`TestPreflightLegacyEgressPolicyInspection` and the legacy-inspection
  tails in the source and work-unit preflight tests:** superseded by
  `TestPreflightRequiresSubmissionID`, which pins the refusal that replaces
  the mode.

Revisit when a writer of any of these shapes reappears, or prod's count
under #3265 is nonzero.
