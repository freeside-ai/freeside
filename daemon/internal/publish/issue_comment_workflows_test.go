package publish

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestIssueCommentWorkflowsTriggerForms(t *testing.T) {
	t.Parallel()
	const jobs = `
jobs:
  respond:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`
	sources := []workflowSource{
		{Path: ".github/workflows/scalar.yml", Active: true, Content: []byte("on: issue_comment" + jobs)},
		{Path: ".github/workflows/list.yml", Active: true, Content: []byte("on: [push, issue_comment]" + jobs)},
		{Path: ".github/workflows/map.yml", Active: true, Content: []byte("on:\n  issue_comment:\n    types: [created]" + jobs)},
		{Path: ".github/workflows/beside.yml", Active: true, Content: []byte("on:\n  pull_request:\n  issue_comment:\n  issues:" + jobs)},
		{Path: ".github/workflows/ci.yml", Active: true, Content: []byte("on: [push, pull_request, issues]" + jobs)},
		{Path: ".github/workflows/disabled.yml", Active: false, Content: []byte("on: issue_comment" + jobs)},
	}
	listed, err := issueCommentWorkflows(sources, nil)
	if err != nil {
		t.Fatalf("issueCommentWorkflows: %v", err)
	}
	var paths []string
	for _, w := range listed {
		if err := w.Validate(); err != nil {
			t.Fatalf("listed entry %+v invalid: %v", w, err)
		}
		paths = append(paths, w.Path)
	}
	want := []string{
		".github/workflows/beside.yml", ".github/workflows/list.yml",
		".github/workflows/map.yml", ".github/workflows/scalar.yml",
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("listed %v, want %v", paths, want)
	}

	none, err := issueCommentWorkflows(sources[4:], nil)
	if err != nil {
		t.Fatalf("issueCommentWorkflows without listed workflows: %v", err)
	}
	if none != nil {
		t.Fatalf("no issue_comment workflow listed %#v, want nil", none)
	}
}

func TestIssueCommentWorkflowsFailClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sources []workflowSource
		actions []localActionSource
		want    string
	}{
		{
			name: "malformed listed workflow",
			sources: []workflowSource{{
				Path: ".github/workflows/bot.yml", Active: true,
				Content: []byte("on: issue_comment\njobs: [unclosed\n"),
			}},
			want: "bot.yml",
		},
		{
			name: "merge key in the trigger map",
			sources: []workflowSource{{Path: ".github/workflows/bot.yml", Active: true, Content: []byte(`x-triggers: &triggers
  issue_comment:
on:
  <<: *triggers
jobs:
  respond:
    runs-on: ubuntu-latest
    steps:
      - run: echo hi
`)}},
			want: "trigger map",
		},
		{
			name: "absent local action",
			sources: []workflowSource{{Path: ".github/workflows/bot.yml", Active: true, Content: []byte(`on: issue_comment
jobs:
  respond:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/missing
`)}},
			want: "absent",
		},
		{
			name: "absent local reusable workflow",
			sources: []workflowSource{{Path: ".github/workflows/bot.yml", Active: true, Content: []byte(`on: issue_comment
jobs:
  respond:
    uses: ./.github/workflows/missing.yml
`)}},
			want: "absent",
		},
		{
			name: "absent action behind a local action",
			sources: []workflowSource{{Path: ".github/workflows/bot.yml", Active: true, Content: []byte(`on: issue_comment
jobs:
  respond:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/outer
`)}},
			actions: []localActionSource{{Path: ".github/actions/outer/action.yml", Content: []byte(`runs:
  using: composite
  steps:
    - uses: ./.github/actions/missing
`)}},
			want: "absent",
		},
		{
			name: "non-composite local action",
			sources: []workflowSource{{Path: ".github/workflows/bot.yml", Active: true, Content: []byte(`on: issue_comment
jobs:
  respond:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/node
`)}},
			actions: []localActionSource{{Path: ".github/actions/node/action.yml", Content: []byte(`runs:
  using: node20
  main: index.js
`)}},
			want: "unsupported",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			listed, err := issueCommentWorkflows(tt.sources, tt.actions)
			if err == nil {
				t.Fatalf("issueCommentWorkflows listed %+v, want an error", listed)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

// issueCommentClosureFixture is a listed workflow (bot.yml) that calls a local reusable
// workflow and uses a local action directly; the reusable workflow uses a
// second action, and the direct action nests a third. ci.yml and the z action
// are outside the closure.
func issueCommentClosureFixture() (map[string]string, map[string]string) {
	workflows := map[string]string{
		".github/workflows/bot.yml": `on: issue_comment
jobs:
  respond:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: ./.github/actions/a
  shared:
    uses: ./.github/workflows/shared.yml
`,
		".github/workflows/shared.yml": `on: workflow_call
jobs:
  inner:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/b
`,
		".github/workflows/ci.yml": `on: pull_request
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/z
`,
	}
	actions := map[string]string{
		".github/actions/a/action.yml": "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/c\n",
		".github/actions/b/action.yml": "runs:\n  using: composite\n  steps:\n    - run: echo b\n      shell: bash\n",
		".github/actions/c/action.yml": "runs:\n  using: composite\n  steps:\n    - run: ${{ github.action_path }}/run.sh\n      shell: bash\n",
		".github/actions/c/run.sh":     "echo c\n",
		".github/actions/z/action.yml": "runs:\n  using: composite\n  steps:\n    - run: echo z\n      shell: bash\n",
	}
	return workflows, actions
}

func closureDigestOf(t *testing.T, workflows, actions map[string]string) domain.Digest {
	t.Helper()
	var sources []workflowSource
	for _, p := range slices.Sorted(maps.Keys(workflows)) {
		sources = append(sources, workflowSource{Path: p, Content: []byte(workflows[p]), Active: true})
	}
	var actionSources []localActionSource
	for _, p := range slices.Sorted(maps.Keys(actions)) {
		actionSources = append(actionSources, localActionSource{Path: p, Content: []byte(actions[p])})
	}
	listed, err := issueCommentWorkflows(sources, actionSources)
	if err != nil {
		t.Fatalf("issueCommentWorkflows: %v", err)
	}
	if len(listed) != 1 || listed[0].Path != ".github/workflows/bot.yml" {
		t.Fatalf("listed %+v, want only bot.yml", listed)
	}
	return listed[0].Digest
}

func TestIssueCommentWorkflowsClosureDigest(t *testing.T) {
	t.Parallel()
	baseWorkflows, baseActions := issueCommentClosureFixture()
	base := closureDigestOf(t, baseWorkflows, baseActions)

	tests := []struct {
		name    string
		file    string
		changes bool
	}{
		{"the workflow file", ".github/workflows/bot.yml", true},
		{"a local reusable workflow it calls", ".github/workflows/shared.yml", true},
		{"a local action it uses directly", ".github/actions/a/action.yml", true},
		{"a local action its reusable workflow uses", ".github/actions/b/action.yml", true},
		{"a local action nested in another", ".github/actions/c/action.yml", true},
		{"a script in a used action's directory", ".github/actions/c/run.sh", true},
		{"an unrelated workflow", ".github/workflows/ci.yml", false},
		{"an unrelated action", ".github/actions/z/action.yml", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workflows, actions := issueCommentClosureFixture()
			if _, ok := workflows[tt.file]; ok {
				workflows[tt.file] += "# edited\n"
			} else {
				actions[tt.file] += "# edited\n"
			}
			got := closureDigestOf(t, workflows, actions)
			if changed := got != base; changed != tt.changes {
				t.Fatalf("editing %s: digest changed = %t, want %t", tt.file, changed, tt.changes)
			}
		})
	}
}

// An action nested in another action's directory is still followed: the
// outer action's directory sweep adds the nested metadata file, and the
// action that one uses must still enter the closure.
func TestIssueCommentWorkflowsClosureFollowsNestedAction(t *testing.T) {
	t.Parallel()
	workflows := map[string]string{".github/workflows/bot.yml": `on: issue_comment
jobs:
  respond:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/a
`}
	actions := func(c string) map[string]string {
		return map[string]string{
			".github/actions/a/action.yml":     "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/a/sub\n",
			".github/actions/a/sub/action.yml": "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/c\n",
			".github/actions/c/action.yml":     c,
		}
	}
	base := closureDigestOf(t, workflows, actions("runs:\n  using: composite\n  steps:\n    - run: echo one\n      shell: bash\n"))
	edited := closureDigestOf(t, workflows, actions("runs:\n  using: composite\n  steps:\n    - run: echo two\n      shell: bash\n"))
	if edited == base {
		t.Fatal("editing an action used by a nested action left the closure digest unchanged")
	}
}

// A cycle among local actions terminates: the closure is a set of files, so a
// file already in it is not followed again.
func TestIssueCommentWorkflowsClosureCycleTerminates(t *testing.T) {
	t.Parallel()
	workflows := map[string]string{".github/workflows/bot.yml": `on: issue_comment
jobs:
  respond:
    runs-on: ubuntu-latest
    steps:
      - uses: ./.github/actions/a
`}
	actions := map[string]string{
		".github/actions/a/action.yml": "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/b\n",
		".github/actions/b/action.yml": "runs:\n  using: composite\n  steps:\n    - uses: ./.github/actions/a\n",
	}
	if closureDigestOf(t, workflows, actions) == "" {
		t.Fatal("cyclic closure produced an empty digest")
	}
}
