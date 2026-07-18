# Reject verifier shell command strings

Work unit for #154 (gauntlet), following the shell facet deferred by
`2026-07-18-0403-verify-command-path-classification.md`. This changes
the trusted-recipe parsing boundary, so the returned-object
trust-boundary and refute-first rules apply.

## Decision

- **Reject recognized direct shell command-string forms at
  `ParseRecipe`.** Chose rejection over extracting an entrypoint from
  shell text because recipes are explicit argv and the process room
  intentionally performs no shell parse. Partial shell parsing would
  add a second, inevitably incomplete command-language model to a
  security boundary. A repository script remains expressible as a
  direct executable or as an interpreter plus a plain script path.
- Reject `-c` both alone and in a short-option cluster for the
  recognized direct Unix-shell basenames. Fish also has equivalent
  `-C`, `--command`, and `--init-command` forms; those fail closed too.
  The check deliberately scans the whole shell argv rather than
  interpreting shell-specific option ordering. This over-rejects a
  `-c`-shaped argument intended for a script invoked through a shell;
  the recipe author invokes that script directly instead.

## Rejected alternative

- **Extract the first command word from `-c` text.** Rejected because
  quoting, assignments, substitutions, compound commands, and
  shell-specific syntax make even “first entrypoint” extraction a
  general shell-parsing problem. Rejecting the form makes the recipe's
  explicit-argv contract honest and fail-closed.

## Accepted limitations

- Detection is limited to recognized direct shell executable
  basenames. An alias or indirect dispatcher such as `env sh -c` or
  `busybox sh -c` is not interpreted. Trusted recipe authors use a
  recognized shell name directly; no current or planned recipe uses a
  shell wrapper.
- Non-shell interpreters with code-string options (for example,
  `python -c`) remain opaque argv. They are outside #154's shell-runner
  contract and no current recipe uses one.
- A direct shell invocation that passes `-c` to a script is rejected
  even when the shell would treat it as a script operand. Invoking the
  repository script directly preserves that argument without creating
  an opaque shell command string.

## Refute-first verification findings

- A fresh-context refute pass **confirmed and fixed** one bypass in the
  first design: fish's `--command=...` and
  `-C`/`--init-command` variants passed while the validator named fish
  as a guarded shell. The adversarial parser corpus now covers these
  forms as well as bare/absolute shells and clustered `-c` options.
- **Rejected by verification:** the regression is non-vacuous at both
  boundaries. `ParseRecipe` returns `ErrRecipeInvalid`; end-to-end
  `Verify` runs no command for `sh -c "./scripts/verify.sh --fast"`.
  The genuine spaced xcodebuild destination still parses, the
  whitespace-exclusion `CommandPaths` behavior is unchanged, and both
  direct and prefix symlink-entrypoint tests pass.
- `go build ./...`, `go test ./...`, `go vet ./...`, and
  `golangci-lint run` pass after the refute fix. The first sandboxed
  full-test attempt could not bind loopback fixtures and polluted git
  fixture output with a macOS temp-directory warning; the permitted
  out-of-sandbox rerun passed all packages.

## Revisit when

- A trusted recipe needs an indirect shell dispatcher, an unrecognized
  shell, or a non-shell code-string interpreter. Revisit the recipe
  command model as a separate policy decision rather than incrementally
  parsing more command languages.
