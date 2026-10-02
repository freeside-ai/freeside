package agenttree

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

// Patch renders the change from one revision's files to another as a patch
// `git apply` accepts at the checkout root. The daemon never applies it:
// adoption prints it, and a human reviews and commits it, so the tree only
// ever changes through review. A changed file is replaced whole, which keeps
// the patch a function of the two revisions alone.
func Patch(old, next Files) ([]byte, error) {
	paths := sortedPaths(old)
	for _, path := range sortedPaths(next) {
		if _, ok := old[path]; !ok {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	var out bytes.Buffer
	for _, path := range paths {
		before, had := old[path]
		after, has := next[path]
		if had && has && bytes.Equal(before, after) {
			continue
		}
		removed, err := patchLines(path, before)
		if err != nil {
			return nil, err
		}
		added, err := patchLines(path, after)
		if err != nil {
			return nil, err
		}
		full := Root + "/" + path
		fmt.Fprintf(&out, "diff --git a/%s b/%s\n", full, full)
		from, to := "a/"+full, "b/"+full
		switch {
		case !had:
			out.WriteString("new file mode 100644\n")
			from = "/dev/null"
		case !has:
			out.WriteString("deleted file mode 100644\n")
			to = "/dev/null"
		}
		if len(removed)+len(added) == 0 {
			// An empty file created or deleted has a header and no hunk.
			continue
		}
		fmt.Fprintf(&out, "--- %s\n+++ %s\n@@ -%s +%s @@\n", from, to, hunkRange(removed), hunkRange(added))
		for _, line := range removed {
			out.WriteString("-" + line + "\n")
		}
		for _, line := range added {
			out.WriteString("+" + line + "\n")
		}
	}
	return out.Bytes(), nil
}

func patchLines(path string, body []byte) ([]string, error) {
	if len(body) == 0 {
		return nil, nil
	}
	if body[len(body)-1] != '\n' {
		return nil, fmt.Errorf("%s does not end with a newline: %w", path, ErrMalformed)
	}
	return strings.Split(string(body[:len(body)-1]), "\n"), nil
}

func hunkRange(lines []string) string {
	if len(lines) == 0 {
		return "0,0"
	}
	return fmt.Sprintf("1,%d", len(lines))
}
