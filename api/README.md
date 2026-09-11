# api

The OpenAPI spec: the single source of truth for the daemon/client boundary (`docs/plan.md` §5.14).

- **Toolchain:** OpenAPI (spec only).
- **Scope boundary:** the spec and nothing else. Generated code lives with its consumers (`daemon/`, `app/`), never here. Any spec change is treated as a migration.
- **Status:** `openapi.yaml` holds the **provisional** schema for the §5.14 sync surface (bootstrap snapshot, attention/runs/conversations read surfaces, ClientCommand submission, digest-addressed attachment upload), provisional until exercised by real clients (plan §11 Wave 0; decision record in `docs/history/decisions.md`). The daemon serves this surface (`daemon/internal/signet/http.go`) and the Swift client consumes the generated client under `app/Sources/FreesideAPI`. Schemas mirror `daemon/internal/domain` field-for-field; entity examples are lifted from that package's golden files so the linter proves they validate.
- **Contract digest:** `scripts/api-contract-digest.sh` computes the contract identity, `sha256:` plus the lowercase hex SHA-256 of `openapi.yaml`'s exact bytes, and `--write` writes it into two compiled-in constants (`daemon/internal/signet/contract_digest.go`, `app/Sources/FreesideAPI/ContractDigest.swift`). The daemon reports it on `GET /health` (`contract_digest`); the Mac client compares it with its own to diagnose a client/daemon build skew instead of a silent sync failure (#1265). `scripts/check.sh api digest` (which runs `--check`), the daemon's `TestContractDigestMatchesSpec`, and the app's `generate` drift check all fail when the spec changes without regenerating the constants; `app/scripts/generate-api-client.sh` runs `--write` so a client regeneration refreshes them together.
- **Validate:** from the repo root:

  ```sh
  go run github.com/daveshanley/vacuum@v0.29.9 lint -r api/vacuum.ruleset.yaml --details --fail-severity warn api/openapi.yaml
  ```

  `api/vacuum.ruleset.yaml` documents the two deliberately disabled rules. CI runs the same lint invocation (`.github/workflows/api-ci.yml`), but via a pinned prebuilt vacuum binary rather than `go run` (compiling it from source cost ~7min per run); keep the pinned **version** in step across this command, the workflow, and `scripts/check.sh`, and update the workflow's binary **sha256** whenever the version changes.
