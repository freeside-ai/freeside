# Ready Summary Source Views

## Decision

Chose a deterministic view of the existing claim over a second summarizer or
new claim schema. The client recognizes only the advisory Change, Remaining
concerns, and Details headings. It keeps the entire concerns section visible,
even when that defeats the writing target. Duplicate, unknown, empty, or
fenced heading structures use the legacy fallback rather than guessing.

A legacy excerpt carries an explicit warning that omitted concerns are
unknown. Keyword matching cannot establish that a report is safe or complete.
The full original remains below evidence, with its existing producer and
digest. That digest never labels the excerpt as a separately bound artifact.
Artifact-only claims keep the existing attachment handling; preview creation
never fetches protected bytes.

The prompt word target and headings remain advice. They do not change claim
admission, retained bytes, provenance, or the summary's unverified register.
The producer must keep all uncertainty and dissent visible regardless of the
word target, and cite the sources of claimed verification.

## Revisit When

Revisit when the claim contract supplies a separately bound concise report,
or observed summaries show that this narrow writing convention routinely
fails to preserve useful change and concern text.

## Refute-First Finding

Independent review confirmed that trimming indentation before recognizing a
heading could treat an indented code example as Details and hide subsequent
concerns. Indented heading candidates now force the warned legacy fallback;
the regression includes a material concern after the code example.
