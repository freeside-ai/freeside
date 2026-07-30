# Anthropic Export Secret Rule (#384)

**Decision.** The importer recognizes Anthropic credentials with one
vendor-prefix rule, `sk-ant-[A-Za-z0-9_-]{16,}`, rather than separate rules
for the current `api` and `oat` token classes. The setup-token exposure that
motivates this backstop and Anthropic API keys share the vendor-issued prefix,
and the existing ward scanner already treats that full prefix family as
credential material. The importer intentionally uses the ward's exact pattern
without word boundaries: exported credentials remain dangerous when embedded
in another string, and the allowed `-` and `_` payload characters make a
boundary constraint an unsupported source of false negatives. The length
floor keeps redacted examples and short strings out.

Rejected alternatives: matching only `api03` and `oat01` would miss a new
vendor token class while the credential remains structurally recognizable;
matching the bare `sk-ant-` prefix would turn documentation mentions into
blocking findings.

Refute-first verification confirmed that adding word boundaries would diverge
from the ward and miss an otherwise matching token beside a word byte. The
positive fixtures now exercise that adjacency; negative fixtures confirmed
the length floor rejects redacted, short, and wrong-separator forms.

Revisit when Anthropic changes the issued prefix, character set, or minimum
token structure, or when the ward and importer scanners gain a shared rule
registry.
