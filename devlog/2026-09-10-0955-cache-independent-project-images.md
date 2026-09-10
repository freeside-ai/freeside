# Prove Cache Independence Through The Trusted Recipe

Chose an execution proof over lockfile classification after a dependency-free
repository passed offline preparation and verification but failed onboarding
because its cache-masked installation also succeeded. The earlier builder note
(2026-07-27-1030-reusable-project-image-builder.md) assumed every accepted npm
project needed baked dependency material. That assumption does not hold for
this observed repository shape.

Keep the existing network-error negative probe for cache-dependent projects.
When masked installation succeeds, require the image-owned preparation helper
and every trusted recipe command to succeed with the cache masked and networking
disabled. Each command gets a fresh exact-commit workspace. Installation alone
cannot establish cache independence; a runtime failure or recipe failure still
prevents image publication. This uses npm's actual behavior for the selected
platform rather than inventing rules for optional, local or bundled packages.

Rejected adding dummy dependencies, accepting any masked exit code, and skipping
the cache proof for an apparently empty lockfile. None proves the actual trusted
recipe under the accepted image. The existing manifest, provenance, no-network,
script-disabled preparation and fresh-workspace boundaries remain mandatory.

The proof must also match the production verification room's `HOME` and locale.
Bind `HOME=/tmp/freeside-home` and `LC_ALL=C` in the backend for every workspace
run, including preparation and both proof branches. A recipe can observe these
values directly, so implicit image defaults could otherwise pass onboarding
and fail verification. Keep them fixed at the backend boundary rather than
adding caller-configurable environment fields to the proof request.

The generated launcher also needs an explicit mode after writing: the caller's
umask can reduce WriteFile's requested 0755 to 0700, while host-side image proof
correctly requires 0755. Set the mode on the builder's fixed, non-secret launcher
inside its private context; keep preparation and configuration private. Test a
restrictive mask in a subprocess so the test does not change another test's
process-wide permissions.

The owner assigned these live onboarding blockers and similar minor repairs in
#1284. Fixture acceptance is not live exit acceptance; the controlled challenge
still requires merged-code onboarding and real reviewer/adjudicator evidence.

Revisit when a supported package manager needs a different dependency material
boundary or preparation command. Keep the proof tied to execution rather than
assuming that successful installation implies successful verification.
