package collector

import (
	"fmt"
	"strings"
	"testing"
)

func dependenciesSection(lines ...string) string {
	return "### Dependencies\n\n" + strings.Join(lines, "\n")
}

func TestDependencyLinesParseTheSpellingsOpenIssuesUse(t *testing.T) {
	// Each line is a spelling an open issue used on 2026-10-05.
	cases := []struct{ line, relationship, targets string }{
		{"- `starts-after` #1709: reuses the resolver that PR adds", "starts-after", "#1709"},
		{"- `starts-after`: #502 (the state is introduced there)", "starts-after", "#502"},
		{"- starts-after #1630", "starts-after", "#1630"},
		{"- starts-after #1209 (PR #1339), which deleted the run row.", "starts-after", "#1209"},
		{"- `starts-after` #1207, because PR #1325 changes the same helper.", "starts-after", "#1207"},
		{"starts-after: #1563", "starts-after", "#1563"},
		{"`starts-after` #1107.", "starts-after", "#1107"},
		{"`starts-after: #1134`", "starts-after", "#1134"},
		{"`starts-after #1134`: use the product decision", "starts-after", "#1134"},
		{"- `exclusive-with: #853` while both units remain open", "exclusive-with", "#853"},
		{"- `exclusive-with`: #1033. Same card and same budget test.", "exclusive-with", "#1033"},
		{"- `exclusive-with` #1146. Both edit the race step.", "exclusive-with", "#1146"},
		{"- starts-after: #990 / PR #1085.", "starts-after", "#990, PR #1085"},
		{"merges-after: PR #823 (salvaged form)", "merges-after", "PR #823"},
		{"- merges-after #1328 (no code dependency).", "merges-after", "#1328"},
		{"* stacked-on #91", "stacked-on", "#91"},
		{"1. `starts-after` #10, #11 and #12, and #13 & #14", "starts-after", "#10, #11, #12, #13, #14"},
		{"- Starts-After #7", "starts-after", "#7"},
		{fmt.Sprintf("- starts-after #%d", MaxGraphQLInt), "starts-after", fmt.Sprintf("#%d", MaxGraphQLInt)},
	}
	for _, tc := range cases {
		entries := parseDependencies(dependenciesSection(tc.line))
		if len(entries) != 1 {
			t.Fatalf("%q: entries = %#v", tc.line, entries)
		}
		entry := entries[0]
		if entry.kind != DependencyTyped || entry.relationship != tc.relationship || dependencyTargetList(entry.targets) != tc.targets || entry.line != tc.line {
			t.Errorf("%q: kind=%s relationship=%s targets=%s line=%q", tc.line, entry.kind, entry.relationship, dependencyTargetList(entry.targets), entry.line)
		}
	}
}

func TestDependencyNoneLinesAreRecognized(t *testing.T) {
	for _, line := range []string{
		"none", "None.", "- none.", "**None**", "_None._",
		"None. Discovered during #1129.",
		"- None (`starts-after`, `merges-after`, `stacked-on`, `exclusive-with` empty).",
		"none (schedule when the prompt lands)",
		"- `merges-after`: none.",
		"- exclusive-with none",
	} {
		entries := parseDependencies(dependenciesSection(line))
		if len(entries) != 1 || entries[0].kind != DependencyNone || len(entries[0].targets) != 0 || entries[0].line != line {
			t.Errorf("%q: entries = %#v", line, entries)
		}
	}
}

func TestDependencyLinesThatAreNotTypedStayUnknownWordForWord(t *testing.T) {
	cases := []struct{ line, reason string }{
		// An untyped issue reference.
		{"Refs #12, #13.", reasonUntyped},
		{"- #869 starts-after this unit", reasonUntyped},
		{"- Dependents: #1629 and #981.", reasonUntyped},
		// Prose, including prose that mentions a keyword past the line's lead.
		{"- Builds on merged #302 and #334.", reasonUntyped},
		{"- No `starts-after`, `merges-after`, `stacked-on`, or `exclusive-with`.", reasonUntyped},
		{"- Contract regime: exclusive with every other open `kind:contract` unit.", reasonUntyped},
		{"This unit is exclusive-with #5 in practice.", reasonUntyped},
		{"- Contract assessment, #1600 (spine, 2026-10-05, at `8d834525`): independent.", reasonUntyped},
		{"nonexistent prerequisite", reasonUntyped},
		{"- **starts-after** #5", reasonUntyped},
		// A relationship keyword with no target.
		{"- `exclusive-with`: every other open `kind:contract` unit while active", reasonNoTarget},
		{"- exclusive-with every other open `kind:contract` unit", reasonNoTarget},
		{"- starts-after the store migration (#12)", reasonNoTarget},
		{"starts-after", reasonNoTarget},
		{"- starts-after #12abc", reasonNoTarget},
		// A target the forge cannot hold.
		{"- starts-after #0", reasonTargetOutRange},
		{fmt.Sprintf("- starts-after #5, #%d", MaxGraphQLInt+1), reasonTargetOutRange},
		{"- exclusive-with #99999999999999999999", reasonTargetOutRange},
	}
	for _, tc := range cases {
		entries := parseDependencies(dependenciesSection(tc.line))
		if len(entries) != 1 {
			t.Fatalf("%q: entries = %#v", tc.line, entries)
		}
		entry := entries[0]
		if entry.kind != dependencyUnknown || entry.reason != tc.reason || entry.line != tc.line || len(entry.targets) != 0 {
			t.Errorf("%q: kind=%s reason=%q line=%q targets=%v", tc.line, entry.kind, entry.reason, entry.line, entry.targets)
		}
	}
}

func TestDependencyTargetsStopAtTheRationale(t *testing.T) {
	entries := parseDependencies(dependenciesSection("- `starts-after` #10 and the migration in #11, then #12"))
	if len(entries) != 1 || dependencyTargetList(entries[0].targets) != "#10" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestDependencyIndentedLinesContinueTheEntryAbove(t *testing.T) {
	entries := parseDependencies(dependenciesSection(
		"- `starts-after` #10: the resolver",
		"  it reuses lands there.",
		"",
		"  A second paragraph of the same item.",
		"- none of this is typed",
		"  - exclusive-with #20",
		"wrapped prose with no indent",
	))
	if len(entries) != 4 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0].kind != DependencyTyped || entries[0].line != "- `starts-after` #10: the resolver\nit reuses lands there.\nA second paragraph of the same item." {
		t.Errorf("continued entry = %#v", entries[0])
	}
	if entries[1].kind != DependencyNone {
		t.Errorf("entry 1 = %#v", entries[1])
	}
	// An indented line that leads with a keyword is its own declaration: a
	// continuation never hides one.
	if entries[2].kind != DependencyTyped || !entries[2].declaresExclusiveWith(20) {
		t.Errorf("indented declaration = %#v", entries[2])
	}
	if entries[3].kind != dependencyUnknown || entries[3].line != "wrapped prose with no indent" {
		t.Errorf("unindented line = %#v", entries[3])
	}
}

func TestDependencyCodeCommentsAndCarriageReturnsReadAsElsewhere(t *testing.T) {
	section := "### Dependencies\r\n\r\n" +
		"- `starts-after` #10\r\n" +
		"<!-- exclusive-with #30 -->\r\n" +
		"```text\r\n- exclusive-with #31\r\n```\r\n" +
		"- none of the above <!-- exclusive-with #32 -->\r\n"
	entries := parseDependencies(section)
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0].kind != DependencyTyped || dependencyTargetList(entries[0].targets) != "#10" || entries[0].line != "- `starts-after` #10" {
		t.Errorf("entry 0 = %#v", entries[0])
	}
	// The line is kept as written; the comment inside it is not evidence.
	if entries[1].kind != DependencyNone || !strings.Contains(entries[1].line, "#32") || entries[1].mentionsExclusiveWith() || entries[1].mentionsIssue(32) {
		t.Errorf("entry 1 = %#v", entries[1])
	}
}

func TestDependencyEntryMentions(t *testing.T) {
	parse := func(lines ...string) dependencyEntry {
		t.Helper()
		entries := parseDependencies(dependenciesSection(lines...))
		if len(entries) != 1 {
			t.Fatalf("entries = %#v", entries)
		}
		return entries[0]
	}
	declaration := parse("- `exclusive-with` #40 and #41: both edit the installer that #42 added")
	if !declaration.declaresExclusiveWith(40) || !declaration.declaresExclusiveWith(41) || declaration.declaresExclusiveWith(42) {
		t.Errorf("declaration = %#v", declaration)
	}
	if !declaration.mentionsIssue(42) || declaration.mentionsIssue(4) || declaration.mentionsIssue(420) {
		t.Errorf("mentions = %#v", declaration)
	}
	// A reference in the rationale is not a declared target.
	if got := strings.Join(declaration.undeclaredReferences(), ","); got != "#42" {
		t.Errorf("undeclared = %v", got)
	}
	// A pull request shares no number with an issue, so PR #40 never
	// declares exclusivity with issue #40.
	if parse("- exclusive-with PR #40").declaresExclusiveWith(40) {
		t.Error("a PR target declared exclusivity with an issue")
	}
	if parse("- starts-after #40").declaresExclusiveWith(40) {
		t.Error("starts-after declared exclusivity")
	}
	for line, want := range map[string]string{
		"- `starts-after` #40; exclusive-with #41 while both are open": "#40,#41",
		"- `exclusive-with` #40. Also `exclusive-with`: #41":           "#41",
		"- none. In practice exclusive-with PR #41":                    "#41",
		"- `exclusive-with` #40\n  and exclusive-with #041 too":        "#041",
		"- `exclusive-with` #40, and exclusive-with #040 again":        "",
		"- No `starts-after` or `exclusive-with`. See #41.":            "#41",
		// Targets behind a separator the grammar does not know.
		"- exclusive-with #40 #41":                                "#41",
		"- exclusive-with #40; #41":                               "#41",
		"- exclusive-with #40 (and #41)":                          "#41",
		"- exclusive-with #40, `#41`":                             "#41",
		"- exclusive-with #40 or #41, or #41 again":               "#41",
		"- exclusive-with #40,\n  #41":                            "#41",
		"- `starts-after` #5\n  Also **exclusive-with** unit #41": "#5,#41",
		"- None (`starts-after`, `exclusive-with` empty).":        "",
	} {
		entry := parse(strings.Split(line, "\n")...)
		if got := strings.Join(entry.undeclaredReferences(), ","); got != want {
			t.Errorf("%q: undeclared = %q, want %q", line, got, want)
		}
		if !entry.mentionsExclusiveWith() {
			t.Errorf("%q: exclusive-with mention not seen", line)
		}
	}
	// Without the keyword no reference is a possible partner.
	if got := parse("- starts-after #40, see #41").undeclaredReferences(); len(got) != 0 {
		t.Errorf("undeclared without the keyword = %v", got)
	}
}

func TestDependencyDeeplyIndentedLinesAreReadAsTheForgeRendersThem(t *testing.T) {
	// The forge renders a line indented four spaces or a tab under a list
	// item as text, so it can hold a declaration.
	entries := parseDependencies(dependenciesSection(
		"- starts-after #5",
		"    - exclusive-with #10",
		"\t- exclusive-with #11",
		"1. none",
		"    wrapped text naming #12",
		"    ```",
		"    - exclusive-with #13",
		"    ```",
		"    <!-- exclusive-with #14 -->",
	))
	if len(entries) != 4 {
		t.Fatalf("entries = %#v", entries)
	}
	if !entries[1].declaresExclusiveWith(10) || entries[1].line != "- exclusive-with #10" || !entries[2].declaresExclusiveWith(11) {
		t.Errorf("nested declarations = %#v %#v", entries[1], entries[2])
	}
	if entries[3].kind != DependencyNone || entries[3].line != "1. none\nwrapped text naming #12" || !entries[3].mentionsIssue(12) {
		t.Errorf("continued entry = %#v", entries[3])
	}
	// A fence or comment inside the indentation still masks.
	for _, entry := range entries {
		if entry.mentionsIssue(13) || entry.mentionsIssue(14) {
			t.Errorf("masked text read as evidence: %#v", entry)
		}
	}
	alone := parseDependencies(dependenciesSection("    indented prose with no item above"))
	if len(alone) != 1 || alone[0].kind != dependencyUnknown || alone[0].line != "indented prose with no item above" {
		t.Errorf("entries = %#v", alone)
	}
}
