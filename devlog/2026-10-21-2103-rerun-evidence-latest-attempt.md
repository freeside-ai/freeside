# Count Each Shard's Latest Attempt In The Coverage Gate

The daemon coverage gate now reads shard evidence from every attempt of a
run and counts each shard's latest attempt, instead of binding all evidence
to the current attempt. This revises one point of
`2026-10-19-1115-daemon-ci-sharding.md`, which bound the artifacts to one run
attempt. Issue: #2132.

**Changed assumption.** The one-attempt binding assumed every shard reruns
together. "Re-run failed jobs" reruns only the failed shards and the
aggregator, so the passed shards' evidence keeps its earlier attempt suffix.
In run 39676542762 attempt 2, the macOS aggregator saw one artifact and
failed, though every shard had passed; only a full rerun went green.

**Why the latest attempt is safe to count.** The aggregator's gate runs
before the download and requires every child job's current result to be
`success`. A successful shard job always uploads its own attempt's evidence
(`if: always()`, `if-no-files-found: error`, attempt-suffixed name), so each
gated shard's latest artifact is the gated execution's. A stale passing
artifact can't hide a shard that failed now, because the gate fails first.
`verify` still rejects two reports for one shard in one attempt and a
malformed attempt, and among the counted reports it rejects an attempt later
than the current one and evidence from another run. A superseded report isn't
read further, and broken latest evidence never falls back to an older
attempt.

**Rejected: attempt-free names with `actions/upload-artifact`
`overwrite: true`.** It needs no selection logic, but it deletes the failed
attempt's evidence, which is what you read when diagnosing the failure. It
also moves the decision about which attempt counts out of the tested
`verify` script into artifact-store behaviour.

Revisit when the gate stops running before the download, or a shard can
succeed without uploading its own attempt's evidence.
