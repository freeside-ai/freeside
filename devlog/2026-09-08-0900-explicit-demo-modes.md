# Require Explicit Demo Modes

The owner chose real setup screens over implicit mock defaults because sample
data and simulated execution can make an unconfigured installation look ready
for work. The app therefore asks for a daemon address when no deployment is
configured, and the daemon defaults to execution disabled. Both mock app modes
and the fake execution driver remain available through explicit options.

Chose an execution-disabled daemon over refusing to start without a production
driver. A fresh Mac installation still needs pairing, health, stored state, and
backups before an operator has configured agent credentials and project images.
It runs no workflow engine or fake scheduler, and an inbox notice makes the
missing execution setup visible. Configuring a driver resolves that notice.

Defaulting to the production driver was also rejected: selecting a provider
does not supply the required credentials, images, repository, or execution
policy. A driver that only returns errors would still let the workflow engine
process existing state and misreport missing setup as execution failures.

The earlier mock-first client bootstrap made sense while the client was being
built against the in-process contract mock. That testing capability remains;
it no longer determines a person's first launch. See
`2026-07-15-1638-inbox-decision-mock.md` for the original mock design.

Revisit the setup-only daemon when guided onboarding can configure a real
execution path. Do not restore an implicit demo fallback.

Independent refutation traced credential isolation across selected servers,
disabled startup, and recovery from the setup notice. It found no implicit
execution or credential reuse. It did find that a mistyped server address
trapped users at pairing; the connection form now keeps the address and offers
a return path from pairing. Address validation rejects embedded credentials,
query strings, fragments, unsupported schemes, and invalid ports.
