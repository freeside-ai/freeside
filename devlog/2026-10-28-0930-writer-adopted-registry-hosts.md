# Replay Adopted Registry Hosts At Writer Start

For #2406, chose to record the effective registry set in the writer's durable
intent instead of reading the live adoption set on every recovery. Ward binds
the hosts into its journaled spec digest. Adding a host during an existing run
must not change that run's recovery spec.

The record carries the declared hosts plus those adopted for the run's
repository at start. Admission still requires a valid declared set, whether
policy or a capability manifest selects `provider_registry`. Adopted hosts
widen an existing opt-in; they do not enable registry egress alone. Old intents
without the field retain their declared-only allowance.

Every handoff reconstruction checks the recorded set against the immutable
policy and current adoptions: every declared host must remain, and every extra
host must be adopted for this repository. The full start spec, including the
repository ID, is authenticated against the stored admission before a start
reads adoptions. An unavailable adoption store leaves the operation retryable;
an unsupported effective set at start commits a failed outcome, like an
unsupported prompt.

This relies on adoptions being append-only. Revisit recovery if adoptions
become revocable: revocation would make an in-flight run fail its next check.

## Refute-First Findings

- **Confirmed and fixed:** the terminal reconstruction gate requires an
  independently stored outcome. New registry refusals authenticate while
  still seeding, then record the outcome before saving terminal state.
- **Disproved by regression checks:** a recorded set cannot drop a declared
  host or add a host adopted only for another repository. Missing adoption
  composition also refuses extra recorded hosts.
- **Disproved by regression checks:** a later adoption does not widen an
  existing run at recovery; the original recorded hosts reach ward again.
- **Disproved by regression checks:** a store failure at start leaves no
  intent or result, while malformed adopted hosts produce a failed outcome.
- **Checked:** `storeAdmissionAuthority.admission` compares the full
  `StartSpecFromAdmission` with the supplied spec, so a changed repository ID
  does not authenticate. Registry shape is checked when decoding the intent.
