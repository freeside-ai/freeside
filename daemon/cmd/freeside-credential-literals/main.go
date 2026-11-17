// Command freeside-credential-literals reports every line of the named files
// that matches a credential rule (internal/credentialpattern), so the
// repository can keep credential-shaped literals out of its tracked files.
//
// Freeside runs agents on its own repository. An agent that reads a tracked
// file echoes it into its transcript, which the export scan reads, and may
// quote it in a specification or a reply, which the importer screens. Both
// apply these rules, so one credential-shaped fixture in the tree can refuse
// a handoff that leaked nothing (#3305). Applying the same rules here, not a
// copy of their patterns, keeps the check and the gates from drifting.
//
// It reads NUL-separated paths on stdin, as `git ls-files -z` prints them:
//
//	git ls-files -z | freeside-credential-literals
//
// Each match prints as `path:line: rule-id` on stdout. The matched text is
// never printed: a real credential committed by mistake must not reach a CI
// log. Exit status: 0 when nothing matched, 1 when a line matched, 2 when a
// file couldn't be read.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/credentialpattern"
)

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}

func run(stdin io.Reader, stdout, stderr io.Writer) int {
	paths, err := readPaths(stdin)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "freeside-credential-literals:", err)
		return 2
	}
	rules := credentialpattern.All()
	matched := false
	for _, path := range paths {
		content, err := os.ReadFile(path) //nolint:gosec // G304: the caller names the files to scan
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "freeside-credential-literals:", err)
			return 2
		}
		for _, m := range scan(content, rules) {
			matched = true
			_, _ = fmt.Fprintf(stdout, "%s:%d: %s\n", path, m.line, m.ruleID)
		}
	}
	if matched {
		return 1
	}
	return 0
}

// readPaths splits NUL-separated paths, ignoring the terminator after the
// last one.
func readPaths(r io.Reader) ([]string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read paths: %w", err)
	}
	var paths []string
	for _, p := range bytes.Split(data, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	return paths, nil
}

type match struct {
	line   int
	ruleID string
}

// scan matches the whole content, as the export scan does, so a rule whose
// pattern can span a newline (the GCP key id's \s*) still matches. Each match
// reports the line it starts on, which is what a fix needs.
func scan(content []byte, rules []credentialpattern.Rule) []match {
	var found []match
	for _, rule := range rules {
		for _, loc := range rule.Pattern.FindAllIndex(content, -1) {
			found = append(found, match{line: 1 + bytes.Count(content[:loc[0]], []byte{'\n'}), ruleID: rule.ID})
		}
	}
	return found
}
