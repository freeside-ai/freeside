# Authenticate Retained Feedback History

Return to agent produced valid execution history that the projection reader
rejected because it did not recognize the existing feedback intent. This
work repairs that reader without changing the feedback wire format or
authorizing a new retry. Prompt delivery is a separate repair in #1235.

## Retained Intent Authority

Moved the existing operator-feedback intent shape and strict decoder into
the domain package so the producer and timeline reader share its validation.
The reader still binds the queue key, invocation, run and stage. Canonical
encoding, deterministic identities, required fields and paired commit hashes
remain enforced. Broadly accepting an unknown intent kind would hide actual
corruption, so the new case accepts only this existing, validated protocol.

A generated comparison of the old and new decoders agreed on acceptance and
decoded values across 20,000 valid and malformed inputs. The corrected
GetRun and GetRunTimeline readers also reconstructed the retained failed
feedback through a read-only connection, without changing the database.

Review confirmed that accepting feedback dispatch intents also let a forged
`run_submitted` milestone treat a continuation as initial submission authority.
The submission case now rejects feedback after authenticating its reserved
intent. A regression test failed before that restriction and passes with it.
Rejecting all pre-attempt feedback holds would instead hide legitimate work:
the producer creates the stage and outbox before admission, and dispatch can
record a hold while no attempt exists. The held-feedback regression keeps
that projection visible while preserving the submission restriction.

The owner allowed this contract repair to follow #1213's merged code while
that issue remains open for live acceptance. This is a narrow serialization
exception for #1234, not a waiver of #1213's evidence requirements or a change
to the standing contract policy.

Revisit when another execution-intent kind is introduced; its producer and
authenticated readers must share the same identity contract.
