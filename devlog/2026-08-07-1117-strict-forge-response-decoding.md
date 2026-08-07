# Strict Forge Response Decoding

Issue #546 changes the App-registration credential response boundary, so it
carries the credential-leak and returned-object refute-first record.

## Decision

Chose one 16 MiB bound inside `decodeResponse` over the former 64 KiB
App-bot-only bound or endpoint-specific limits. GitHub pages up to 100 comments
whose individual bodies may be large, so 64 KiB can reject legitimate traffic;
16 MiB remains a finite resource guard against an interposed proxy without
turning the guard into response-schema validation.

Chose tracking the shared `io.LimitedReader`'s remaining allowance after
draining at most 16 MiB plus one byte over checking `json.Decoder.InputOffset`.
`InputOffset` describes the decoder's parsed position, not every byte already
read into its buffer, and a syntax error before the end of an oversized body can
stop parsing before the offset crosses the bound. The limited-reader allowance
measures bytes actually consumed from the response and makes every oversized
body return the same content-free error, including malformed and trailing-data
bodies. Exactly-at-bound responses remain accepted because the reader retains
the one-byte probe allowance when the source reaches EOF.

## Refute-First Findings

- **Confirmed and fixed:** the manifest conversion, public App lookup, and
  authenticated App lookup decoded directly, bypassing the package's
  trailing-data rule. All now use `decodeResponse`; the conversion regression
  test proves a trailing payload never reaches the save callback.
- **Confirmed and fixed:** the response-size guard applied only to the App-bot
  identity lookup. The shared decoder now reads no more than 16 MiB plus one
  byte.
- **Confirmed and fixed (refute-first review, P2):** the first package
  guarantee recognized only the literal `json.NewDecoder(resp.Body)` shape, so
  a response-body variable, an `io.LimitReader` wrapper, an import alias, or a
  whole-body `io.ReadAll` could introduce a new decode path without failing the
  test. A follow-up review also showed that guarding the reader but not
  `json.Unmarshal` left an equivalent staged-body bypass. The guarantee now
  resolves import aliases and allowlists every existing production
  `json.NewDecoder`, `json.Unmarshal`, and `io.ReadAll` call by file, function,
  and exact call count. A new decode or whole-reader entry point requires a
  visible allowlist review; only `decodeResponse` is approved for forge
  responses.
- **Rejected by verification:** an oversized response cannot disclose its body
  through the new refusal. The size error is static and a sentinel test proves
  it carries no response bytes.
- **Rejected by verification:** the one-byte probe does not reject a body
  exactly at the bound. Boundary tests cover exactly 16 MiB and one byte over.

Revisit when GitHub introduces a legitimate response shape near 16 MiB, or a
streamed endpoint whose safe contract cannot buffer one bounded JSON document.
