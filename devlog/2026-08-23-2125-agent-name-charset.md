# Agent Name Charset

Work unit: #904.

## Decision

Chose at most 246 bytes of lowercase ASCII letters and digits with `-` and `_`
allowed only at interior positions for agent names. This reserves the lineup's
`@` delimiter, avoids case-folding and path-separator ambiguity in the
control-plane tree, and leaves room for the nine-byte `.attended` suffix in a
255-byte filename component. Names remain outside the agent digest. Rejected
allowing every character except `@`, because that would leave filenames and
the `.attended` mark ambiguous; rejected changing the `name@digest` lineup
format, which is outside this unit's contract.

Repeated interior separators remain valid by decision: the contract constrains
the first and last characters and the allowed interior byte set, not separator
grouping.

## Trust-Boundary Verification

The initial refute-first pass found no defect. Automated review then identified
one correctness gap, and an independent refute pass confirmed it.

- Accepted by verification: the charset gate admitted 247-byte names even
  though the required `<agent-name>.attended` sibling would exceed the common
  255-byte filename-component limit. The gate now caps names at 246 bytes, and
  boundary tests exercise source, reconstruction, strict decode, and lineup
  parsing.

- Rejected by verification: byte-class bypasses. An exhaustive test covers all
  256 byte values at edge and interior positions; uppercase, punctuation,
  whitespace, controls, non-ASCII bytes, and `@` fail closed.
- Rejected by verification: source, reconstruction, and policy-parser bypasses.
  `AgentSource.Validate`, `AgentDefinition.Validate` (including strict decode),
  and `ParseLineupSelection` apply the same gate before accepting a name.
- Rejected by verification: typed-error and digest regressions. Empty names keep
  `ErrEmptyField`, invalid nonempty names use `ErrInvalidAgentName`, malformed
  lineup structure or digests keep `ErrInvalidLineupKey`, and names remain
  outside canonical agent bytes.

## Revisit When

The lineup value format changes, agent names enter another tree-backed
interface with stricter filename needs, a supported filesystem imposes a
component limit below 255 bytes, or the control-plane tree permits a name
vocabulary this ASCII rule cannot express.
