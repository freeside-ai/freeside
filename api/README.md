# api

The OpenAPI spec: the single source of truth for the daemon/client boundary (`docs/plan.md` §5.14).

- **Toolchain:** OpenAPI (spec only).
- **Scope boundary:** the spec and nothing else. Generated code lives with its consumers (`daemon/`, `app/`), never here. Any spec change is treated as a migration.
- **Status:** `openapi.yaml` holds the **provisional** schema for the §5.14 sync surface (bootstrap snapshot, attention/runs/conversations read surfaces, ClientCommand submission, digest-addressed attachment upload), provisional until exercised by real clients (plan §11 Wave 0; decision record in `docs/history/decisions.md`). No server implementation exists yet. Schemas mirror `daemon/internal/domain` field-for-field; entity examples are lifted from that package's golden files so the linter proves they validate.
- **Validate:** from the repo root:

  ```sh
  go run github.com/daveshanley/vacuum@v0.29.9 lint -r api/vacuum.ruleset.yaml --details --fail-severity warn api/openapi.yaml
  ```

  `api/vacuum.ruleset.yaml` documents the two deliberately disabled rules. CI runs the same command (`.github/workflows/api-ci.yml`); keep the pinned version in this command, the workflow, and AGENTS.md in step.
