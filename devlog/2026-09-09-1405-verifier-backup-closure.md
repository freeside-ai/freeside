# Verify Workflow Payloads At The Backup Boundary

The live exit verifier could not reconstruct encrypted checkpoints after
feedback successor publication. Its payload-reader registry omitted five
workflow kinds already supported by the daemon. Checkpoint inspection rejects
an incomplete artifact closure before reporting any healthy dimension, so the
admission gate reported that the checkpoint was not encrypted. That message
did not establish that encryption had been removed.

Chose the daemon's existing validating extractors over bypassing backup health
or trusting recorded admission facts. A recognized workflow kind still needs
canonical payload validation and the correct queue key. Encryption proves
checkpoint authenticity, not that this verifier understands every retained
record. Unknown and malformed records must continue to fail closed.

The regression constructs real encrypted checkpoints for all five omitted
kinds, then opens them through the final verifier's existing-file, read-only
path beside a writer. Each valid case reproduced the admission failure before
registration. Adversarial fixtures seal malformed payloads, mismatched keys,
and an unsupported kind under a test-only permissive producer, then require
the actual verifier to reject them. Production readers remain unchanged.

Independent refutation found no reachable defect. It disproved a possible
validation bypass by tracing each registered function and the closure-gap
path: all sixteen bindings match the daemon, rejected payloads still close
admission, and the permissive fixture producer is separate from the verifier.

Revisit the separate composition maps when another workflow kind is added;
its durable-payload checkpoint must work in both the daemon and the final
verifier. This repair does not establish live exit acceptance, which requires
the supported merged verifier against the preserved live result.
