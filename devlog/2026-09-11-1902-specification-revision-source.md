# Authenticate The Source Across Specification Revisions

Chose to bind the production attempt to the first authenticated specification
input rather than require the entire input vector to contain one artifact.
Request changes legitimately adds the prior specification and authenticated
feedback. Research and answered questions can also extend that vector. Its
length does not identify its source.

The production submission first verifies the specification terminal and its
transition chain. Those checks pin the initial source, reconstruct the allowed
input order, and authenticate feedback, publication and campaign identities.
The attempt gate still rejects an empty vector and compares the first
artifact's digest with the attempt's original source digest. Accepting extra
unauthenticated artifacts or changing the source-selection rule would be a
different change.

The revision fixture previously omitted the campaign identity, so it skipped
the attempt gate even when it covered several specification inputs. The
regression now uses a real campaign record and runs Request changes through
approval, production run creation and execution admission. It reproduces the
reported rejection before the fix. The existing initial-approval and forged
lineage/transition tests remain separate checks of the unchanged boundaries.

Independent refutation found no reachable bypass: terminal and transition
verification precede the changed gate in the same transaction, including the
accepted feedback command and reserved implementation identity. The regression
reaches the production attempt gate with a nonempty campaign and verifies
admission eligibility. It does not run an implementation driver or establish
live review acceptance.

Revisit this choice if the specification input contract stops placing the
original source first or production submission can bypass terminal verification.
