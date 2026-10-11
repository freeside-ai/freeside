# Agent Summary Presentation

Work unit: #1461. Extends
`devlog/2026-09-17-1415-ready-summary-source-views.md`, whose decision this
keeps and widens.

## Decision

Chose the card's body face for the agent summary over frame 4b's serif
statement, on the owner's direction of 2026-10-10 (#1461). The summary is
unverified agent prose on a card that asks for judgment, and in the Wave 8
exit run it was the largest text on the Mac card after the ask. It now draws
in the body face, sans regular, at the ladder's statement size (15pt on the
Mac, 16pt on iPhone) on every card type. The serif statement face keeps its
value, and other agent text keeps its presentation: the execution-failure
diagnostic, proposal text, dispute positions, question options, conversation
messages, and the reason a legacy specification approval shows under its ask.

The owner chose the size on 2026-10-10 after the first renders. Those drew
the summary at the body size, 13pt on the Mac and 13.5pt on iPhone, and it
read as too small beside the conversation's agent reply, which is serif at
15pt and 16pt. The summary's size is therefore unchanged from the serif
statement it replaces, on both platforms; what changes is its face. Sans at
15pt reads slightly larger than the serif at 15pt because of its taller
x-height.

Chose one bounded, formatted view on every card over the per-card split.
Before this, only the ready-for-review card used #1378's bounded view; every
other card printed the whole claim. All of them now show the 800-character
lead, the recognized concerns in full, and the complete original one
disclosure away, in place. The rules #1378 set are unchanged: no
client-written summary, no keyword inference that a report is safe, no silent
cut, and unknown concerns never read as no concerns.

Chose full Markdown block parsing through the specification reader's parser
over the inline-only reading the summary had. Inline-only kept the agent's
hard line breaks and printed list and heading markers literally, which broke
sentences mid-line and buried a concern in a paragraph (#1929). The parser
already neutralizes links, images, and HTML for agent-authored text, so the
summary inherits that boundary instead of growing a second one. A summary
heading draws semibold at the text's own size: an agent's heading is a
heavier line in its report, never a larger one.

An excerpt cut that lands inside a block is made in the parsed text, not the
source. Parsing a source prefix that ends inside a list item or an emphasis
span prints half-open markers. The cut falls after as many drawn characters
as the 800-character source prefix holds, between words. Inline markers are
not drawn, so it can fall later in the source than the prefix ends, and a
source whose drawn text fits the bound is complete. The card says
"incomplete" only when the drawn lead leaves text out, so the label follows
the blocks, not the source length.

Each block after the first also spends three characters of that bound, the
source a line break and the shortest list marker take. Counting drawn
characters alone let a report of very short blocks outrun the bound: 300
one-letter list items draw 300 characters, and a rule draws none, so every
row drew and the lead was called complete. Three per block is an estimate,
not the block's source length, which the parsed blocks do not carry. It
holds the lead to about 200 one-letter rows, where the source prefix ends.

Chose the source as written over the parser's output whenever that output
omits text the source holds. The block parser drops a link reference
definition, a link title, and an image's URL without a trace, and a concern
written as `- [risk]: flaky` is a reference definition. The check is that
every source character that cannot be a marker appears in the drawn text in
order; text the parser adds, such as a link's destination, passes. Markdown's
markers are all whitespace or ASCII punctuation, so every other character
counts: letters and digits, and also an emoji or a dash, since a link title
can be a warning sign alone. A dropped run of ASCII punctuation alone is not
told from the markers the parser consumes; telling them apart would take a
second Markdown parser. A summary that fails the check draws as literal text,
markers included, on the card and in the original report. Literal markers are
the lesser failure on a card that promises nothing is cut silently.

Chose to fix a trap in the shared parser in this unit, as its own commit
ahead of the one that shares the parser. Foundation shifts a lazy
continuation line's columns by its container's prefix, and its conversion of
a source position to a string range traps, where it should return nil, when
the shifted position lies past the end of the text. A report whose last line
continues a list item, a quote, or an indented paragraph without indentation
crashed the parser. The specification reader had the trap already; this unit
is what sends every card's summary through the parser, and a hard-wrapped
report is the input it exists to draw. The parser now converts positions in
a copy of the source with room past the end, so every position inside the
source converts exactly as before and one past the end has no range. With no
range, a block draws from the parser's own text; a link whose label cannot be
placed keeps its block's source. This goes beyond the issue's scope for
`SpecificationMarkdown.swift`, "only to share the Markdown block parsing and
drawing", and changes the reader for inputs that crashed it. The main agent
made the call during review; the owner can veto it by asking for the commit
as its own pull request.

Chose the writer's advisory headings for the specifier, `## Change` and
`## Remaining concerns`, over specifier-specific ones, so the client
recognizes one convention. "Change" is a stretch for a proposal; a second
lead heading would need a second recognizer path for no reader benefit.

## Rejected

- **The summary at the body size, 13pt on the Mac and 13.5pt on iPhone.** It
  was this unit's first rendering and the issue's criterion. The owner
  rejected it on seeing it: the summary read smaller than the agent's reply
  in the conversation below it.
- **An in-between size, 14pt and 14.5pt.** It would add a size the ladder
  does not have for one module, and the owner's read was that the statement
  size is the right one.
- **A second summarizer invocation.** It would add an unverified layer over
  an unverified layer and a new claim to bind; the deterministic view of the
  existing claim needs neither.
- **Validating summary length or headings in the daemon.** The word target and
  headings stay writing advice, as #1378 decided. The exit run's writer
  prompts already carried the target and the summary still ran long, so the
  client's bound is what holds, not the guidance.
- **Raising the specifier prompt's 4 KiB budget.** The new guidance fit by
  tightening: examples moved onto their list lines, blank lines around
  headings removed, and eight wordings shortened without dropping a rule or a
  pinned string. The file now holds 4,095 of its 4,096 bytes, so the next
  addition to it has no room and will force this question again.
- **The terser specifier line** "`## Change`: the proposal and why." It reads
  like the field bullets beside it and invites a heading with its text on the
  same line, which the client does not recognize as a section. "Under
  `## Change`, ..." cost 12 more bytes and puts the text beneath the heading.
- **Fixing the parser's handling of a bare address in this unit.** A
  paragraph that holds one comes back as raw source, as it does in the
  specification reader, which is out of scope here (#1377). The card draws
  raw source in the summary's face, not the reader's monospace, so such a
  paragraph still reads as prose with its markers showing. Follow-up: #1955.
- **Fixing the parser trap in a separate pull request.** It would be the
  cleaner scope, but it needs a work unit nobody assigned, and this unit
  could not merge before it without shipping the crash to every card.
- **Padding the summary's text before parsing it.** That stays inside the
  summary's files but masks the fault and leaves the specification reader
  crashing on the same input.

## Refute-First Finding

Independent review, run before the first push, confirmed three defects in
the first cut of the shared view: the parser's silent drops reached the
concerns and the original report; the incomplete label was decided on source
length while the cut was spent on drawn characters, so a complete lead could
be labeled incomplete; and a cut with no space left in its budget ended
inside a word. Each has a regression test that fails without its fix.

A second independent pass, run on the review fix to the dropped-text check,
found the parser trap above by running the parser on hard-wrapped lists. It
also found that the check compared a re-joined string, which merged a virama
with the consonant across a removed space and sent Telugu and Devanagari
reports to the source fallback; the check now compares characters. It left
two known limits: a non-ASCII symbol inside a link destination draws
percent-encoded, so that report falls back to its source, as one with a
non-ASCII letter there already did; and a dropped title that is only the
bullet a quoted list item draws can pass the check.

## Revisit When

Revisit when the summary at the statement size again reads as the loudest
text on a card; when the specifier prompt needs another rule and the budget
has to be raised or the prompt restructured; or when observed specifier
summaries show that `## Change` misleads the writer about what belongs in the
lead.
