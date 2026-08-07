# Project-Image Clones Mirror the Network Git Baseline (#551)

## Decisions

- **Project-image source clones carry the full publish transport baseline.**
  Chose the eleven-option `publish/gitnet.go` transport baseline over only the
  five options shared by the hardened Git runners because project-image source
  clones receive external objects over the network. In particular,
  `credential.helper=` prevents ambient helpers from receiving the token and
  `transfer.fsckObjects=true` rejects malformed objects before they enter the
  daemon-owned object store.
- **Redirect refusal is unconditional.** Chose loud failure for public as well
  as authenticated renamed-repository redirects because repository resolution,
  not transport, owns identity drift. Authenticated clones also retain the
  stronger property that their authorization header never rides a redirect.
- **The baseline remains a literal local copy until #566.** Chose duplication
  over exporting `publish.transportConfig` because this work unit is a narrow
  hardening fix and #566 owns cross-runner consolidation. Keeping the order
  identical makes that later change a behavior-preserving refactor.

## Refute-First Verification

An independent read-only review tried to disprove exact baseline equality,
unconditional application, credential isolation, and test robustness. It found
no divergence: all eleven options and their order match
`transportConfig("https")`; the prefix is built outside the authentication
branch; the token remains environment-only; and the unauthenticated test pins
the complete prefix. A mechanical extraction comparison independently produced
no difference between the two option lists.

Rejected by verification: the new flags do not move the token into argv, and
the authenticated failure path still suppresses command output that could
contain the credential. Accepted by decision: malformed legacy Git objects and
renamed public repositories can now fail loudly rather than being accepted or
redirected.

Follow-up: #566

## Revisit When

Revisit the local duplication when #566 consolidates the hardened Git runners.
