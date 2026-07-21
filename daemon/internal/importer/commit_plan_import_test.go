package importer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func writeWorkspaceFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

//nolint:staticcheck // Existing import-test call sites keep the primary result and error adjacent.
func runCommitPlanImport(t *testing.T, baseFiles, workspaceFiles map[string]string, plan []byte, mode domain.CommitPlanMode, mutate func(*Options, string)) (Result, error, string, string) {
	t.Helper()
	repo, base := initBaseRepo(t, baseFiles)
	workspace := writeWorkspaceFiles(t, workspaceFiles)
	if plan != nil {
		if err := os.WriteFile(filepath.Join(workspace, export.CommitPlanFilename), plan, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handoff := exportWorkspace(t, workspace)
	clone := cloneAtBase(t, repo)
	opts := testImportOptions(base)
	opts.Policy.CommitPlan = mode
	opts.ImportRef = "refs/freeside/imports/plan-test"
	if mutate != nil {
		mutate(&opts, clone)
	}
	res, err := Import(t.Context(), handoff, clone, opts)
	return res, err, clone, base
}

func noticeIs(result Result, want domain.CommitPlanNoticeReason) bool {
	return result.CommitPlanNotice != nil && *result.CommitPlanNotice == want
}

func TestCommitPlanValidMultiGroup(t *testing.T) {
	plan := []byte(`{"version":"freeside.commit-plan/v1","groups":[` +
		`{"name":"modify","message":"Update a","paths":["a.txt"]},` +
		`{"name":"add","message":"Add b","paths":["b.txt"]},` +
		`{"name":"remove","message":"Remove old","remainder":true}]}`)
	res, err, clone, base := runCommitPlanImport(t,
		map[string]string{"a.txt": "old\n", "del.txt": "bye\n", "keep.txt": "same\n"},
		map[string]string{"a.txt": "new\n", "b.txt": "added\n", "keep.txt": "same\n"},
		plan, domain.CommitPlanPlanPreferred, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" || res.CommitPlanNotice != nil {
		t.Fatalf("valid plan result = %+v", res)
	}
	if got := rungit(t, clone, "rev-list", "--count", base+".."+res.CommitSHA); got != "3" {
		t.Fatalf("commit count = %s, want 3", got)
	}
	if got := rungit(t, clone, "log", "--reverse", "--format=%s", base+".."+res.CommitSHA); got != "Update a\nAdd b\nRemove old" {
		t.Fatalf("subjects = %q", got)
	}
	if got := rungit(t, clone, "log", "--format=%B", base+".."+res.CommitSHA); strings.Count(got, agentProposedTrailer) != 3 {
		t.Fatalf("agent trailer count = %d, log=%q", strings.Count(got, agentProposedTrailer), got)
	}
	if got := rungit(t, clone, "ls-tree", "-r", "--name-only", res.CommitSHA); got != "a.txt\nb.txt\nkeep.txt" {
		t.Fatalf("final tree = %q", got)
	}
}

func TestCommitPlanSingleGroupMatchesSingleCommitTree(t *testing.T) {
	base := map[string]string{"a.txt": "old\n", "delete.txt": "bye\n"}
	workspace := map[string]string{"a.txt": "new\n", "add.txt": "added\n"}
	plan := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"all","message":"Apply candidate","remainder":true}]}`)
	planned, err, plannedClone, plannedBase := runCommitPlanImport(t, base, workspace, plan, domain.CommitPlanPlanPreferred, nil)
	if err != nil {
		t.Fatal(err)
	}
	single, err, _, singleBase := runCommitPlanImport(t, base, workspace, nil, domain.CommitPlanSingleCommit, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plannedBase != singleBase || planned.TreeSHA != single.TreeSHA {
		t.Fatalf("single-group tree/base differ: planned=(%s,%s) single=(%s,%s)", plannedBase, planned.TreeSHA, singleBase, single.TreeSHA)
	}
	if got := rungit(t, plannedClone, "rev-list", "--count", plannedBase+".."+planned.CommitSHA); got != "1" {
		t.Fatalf("single-group plan authored %s commits", got)
	}
	if got := rungit(t, plannedClone, "log", "-1", "--format=%B", planned.CommitSHA); !strings.Contains(got, agentProposedTrailer) {
		t.Fatalf("planned message lacks provenance trailer: %q", got)
	}
}

func TestCommitPlanModesAndFallback(t *testing.T) {
	base := map[string]string{"a.txt": "old\n"}
	workspace := map[string]string{"a.txt": "new\n"}
	t.Run("single absent unchanged", func(t *testing.T) {
		res, err, clone, _ := runCommitPlanImport(t, base, workspace, nil, domain.CommitPlanSingleCommit, nil)
		if err != nil || res.CommitSHA == "" || res.CommitPlanNotice != nil {
			t.Fatalf("result=%+v err=%v", res, err)
		}
		if got := rungit(t, clone, "log", "-1", "--format=%B", res.CommitSHA); got != DefaultCommitMessage {
			t.Fatalf("message = %q", got)
		}
	})
	t.Run("single present malformed is not decoded", func(t *testing.T) {
		res, err, _, _ := runCommitPlanImport(t, base, workspace, []byte(`{"unterminated"`), domain.CommitPlanSingleCommit, nil)
		if err != nil || res.CommitSHA == "" || len(res.Findings) != 0 || !noticeIs(res, domain.CommitPlanNoticePresentButNotHonored) {
			t.Fatalf("result=%+v err=%v", res, err)
		}
	})
	t.Run("preferred absent fallback", func(t *testing.T) {
		res, err, clone, _ := runCommitPlanImport(t, base, workspace, nil, domain.CommitPlanPlanPreferred, nil)
		if err != nil || res.CommitSHA == "" || !noticeIs(res, domain.CommitPlanNoticeAbsent) {
			t.Fatalf("result=%+v err=%v", res, err)
		}
		if got := rungit(t, clone, "log", "-1", "--format=%B", res.CommitSHA); strings.Contains(got, agentProposedTrailer) {
			t.Fatalf("fallback carries agent trailer: %q", got)
		}
	})
	t.Run("preferred structural fallback", func(t *testing.T) {
		res, err, _, _ := runCommitPlanImport(t, base, workspace,
			[]byte(`{"version":"freeside.commit-plan/v1","groups":[]}`), domain.CommitPlanPlanPreferred, nil)
		if err != nil || res.CommitSHA == "" || !noticeIs(res, domain.CommitPlanNoticeStructural) {
			t.Fatalf("result=%+v err=%v", res, err)
		}
	})
	t.Run("preferred screening fallback", func(t *testing.T) {
		res, err, _, _ := runCommitPlanImport(t, base, workspace,
			[]byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"Fixes #1","remainder":true}]}`),
			domain.CommitPlanPlanPreferred, nil)
		if err != nil || res.CommitSHA == "" || !noticeIs(res, domain.CommitPlanNoticeScreening) {
			t.Fatalf("result=%+v err=%v", res, err)
		}
	})
}

func TestCommitPlanDecodedSecretDominatesFallback(t *testing.T) {
	escaped := `ghp_\u0041` + strings.Repeat("A", 35)
	for _, raw := range []string{
		`{"version":"freeside.commit-plan/v1","groups":[],"unknown":"` + escaped + `"}`,
		`{"version":"freeside.commit-plan/v1","groups":[],"` + escaped + `":"value"}`,
		`{"version":"freeside.commit-plan/v1","groups":[],"number":1e999,"unknown":"` + escaped + `"}`,
	} {
		res, err, _, _ := runCommitPlanImport(t,
			map[string]string{"a.txt": "old\n"}, map[string]string{"a.txt": "new\n"}, []byte(raw),
			domain.CommitPlanPlanPreferred, nil)
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if res.CommitSHA != "" || res.CommitPlanNotice != nil || len(res.Findings) == 0 || res.Findings[0].Kind != FindingCommitPlanSecret {
			t.Fatalf("secret plan result = %+v", res)
		}
	}
}

func TestCommitPlanEmptyRemainderMessageHandling(t *testing.T) {
	base := map[string]string{"a.txt": "old\n"}
	workspace := map[string]string{"a.txt": "new\n"}
	plan := []byte(`{"version":"freeside.commit-plan/v1","groups":[` +
		`{"name":"all","message":"Update a","paths":["a.txt"]},` +
		`{"name":"empty","message":"[skip ci]","remainder":true}]}`)
	res, err, clone, baseSHA := runCommitPlanImport(t, base, workspace, plan, domain.CommitPlanPlanPreferred, nil)
	if err != nil || res.CommitSHA == "" || res.CommitPlanNotice != nil {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if got := rungit(t, clone, "rev-list", "--count", baseSHA+".."+res.CommitSHA); got != "1" {
		t.Fatalf("empty remainder authored a commit: %s", got)
	}

	escaped := `ghp_\u0041` + strings.Repeat("A", 35)
	secretPlan := []byte(`{"version":"freeside.commit-plan/v1","groups":[` +
		`{"name":"all","message":"Update a","paths":["a.txt"]},` +
		`{"name":"empty","message":"` + escaped + `","remainder":true}]}`)
	res, err, _, _ = runCommitPlanImport(t, base, workspace, secretPlan, domain.CommitPlanPlanPreferred, nil)
	if err != nil || res.CommitSHA != "" || len(res.Findings) == 0 || res.Findings[0].Kind != FindingCommitPlanSecret {
		t.Fatalf("empty-remainder secret result=%+v err=%v", res, err)
	}
}

func TestCommitPlanZeroChangeDispatch(t *testing.T) {
	base := map[string]string{"a.txt": "same\n"}
	workspace := map[string]string{"a.txt": "same\n"}
	for name, plan := range map[string][]byte{
		"valid":      []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"ok","remainder":true}]}`),
		"malformed":  []byte(`{"bad"`),
		"structural": []byte(`{"version":"freeside.commit-plan/v1","groups":[]}`),
		"screening":  []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"Fixes #1","remainder":true}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			res, err, _, _ := runCommitPlanImport(t, base, workspace, plan, domain.CommitPlanPlanPreferred, nil)
			if err != nil || res.CommitSHA == "" || !noticeIs(res, domain.CommitPlanNoticePresentButNotHonored) {
				t.Fatalf("result=%+v err=%v", res, err)
			}
		})
	}
	res, err, _, _ := runCommitPlanImport(t, base, workspace, nil, domain.CommitPlanPlanPreferred, nil)
	if err != nil || res.CommitSHA == "" || res.CommitPlanNotice != nil {
		t.Fatalf("absent zero-change result=%+v err=%v", res, err)
	}
	escaped := `ghp_\u0041` + strings.Repeat("A", 35)
	res, err, _, _ = runCommitPlanImport(t, base, workspace,
		[]byte(`{"version":"freeside.commit-plan/v1","groups":[],"unknown":"`+escaped+`"}`),
		domain.CommitPlanPlanPreferred, nil)
	if err != nil || res.CommitSHA != "" || len(res.Findings) == 0 || res.Findings[0].Kind != FindingCommitPlanSecret {
		t.Fatalf("zero-change secret result=%+v err=%v", res, err)
	}
}

func TestCommitPlanBaseNamespacePreflight(t *testing.T) {
	for _, mode := range []domain.CommitPlanMode{domain.CommitPlanSingleCommit, domain.CommitPlanPlanPreferred} {
		for _, collision := range []string{
			export.CommitPlanFilename,
			export.CommitPlanFilename + "/child",
			".FREESIDE-COMMIT-PLAN.JSON",
			".freeſide-commit-plan.json/child",
		} {
			t.Run(string(mode)+"/"+collision, func(t *testing.T) {
				base := map[string]string{"keep.txt": "same\n", collision: "tracked\n"}
				_, err, _, _ := runCommitPlanImport(t, base, map[string]string{"keep.txt": "same\n"}, nil, mode, nil)
				if !errors.Is(err, ErrCommitPlanCollision) {
					t.Fatalf("Import = %v, want collision", err)
				}
			})
		}
		t.Run(string(mode)+"/near-prefix", func(t *testing.T) {
			near := export.CommitPlanFilename + ".bak"
			res, err, _, _ := runCommitPlanImport(t, map[string]string{near: "same\n"}, map[string]string{near: "same\n"}, nil, mode, nil)
			if err != nil || res.CommitSHA == "" {
				t.Fatalf("near-prefix result=%+v err=%v", res, err)
			}
		})
	}
}

func TestImportRejectsForgedCommitPlanNamespaceEntry(t *testing.T) {
	for _, mode := range []domain.CommitPlanMode{domain.CommitPlanSingleCommit, domain.CommitPlanPlanPreferred} {
		for _, path := range []string{
			export.CommitPlanFilename,
			export.CommitPlanFilename + "/child",
			".FREESIDE-COMMIT-PLAN.JSON",
			".freeſide-commit-plan.json/child",
		} {
			t.Run(string(mode)+"/"+path, func(t *testing.T) {
				repo, base := initBaseRepo(t, map[string]string{})
				handoff := buildHandoff(t, []blobSpec{{path: path, content: "poison\n"}})
				opts := testImportOptions(base)
				opts.Policy.CommitPlan = mode
				result, err := Import(t.Context(), handoff, cloneAtBase(t, repo), opts)
				if !errors.Is(err, ErrCommitPlanCollision) || result.CommitSHA != "" {
					t.Fatalf("Import = %+v, %v; want reserved-namespace rejection", result, err)
				}
			})
		}
	}
}

func TestCommitPlanConstructAllSwapOnce(t *testing.T) {
	plan := []byte(`{"version":"freeside.commit-plan/v1","groups":[` +
		`{"name":"a","message":"Add a","paths":["a.txt"]},` +
		`{"name":"b","message":"Add b","paths":["b.txt"]}]}`)
	injected := errors.New("injected construction failure")
	res, err, clone, _ := runCommitPlanImport(t, map[string]string{}, map[string]string{"a.txt": "a", "b.txt": "b"}, plan,
		domain.CommitPlanPlanPreferred, func(opts *Options, _ string) {
			opts.constructionHook = func(group int) error {
				if group == 1 {
					return injected
				}
				return nil
			}
		})
	if !errors.Is(err, injected) || res.CommitSHA != "" {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if got := rungit(t, clone, "for-each-ref", "--format=%(objectname)", "refs/freeside/imports/plan-test"); got != "" {
		t.Fatalf("import ref moved after partial construction: %s", got)
	}

	_, err, clone, base := runCommitPlanImport(t, map[string]string{}, map[string]string{"a.txt": "a", "b.txt": "b"}, plan,
		domain.CommitPlanPlanPreferred, func(opts *Options, clone string) {
			opts.beforeRefUpdate = func() error {
				rungit(t, clone, "update-ref", opts.ImportRef, opts.BaseSHA)
				return nil
			}
		})
	if err == nil {
		t.Fatal("CAS race succeeded")
	}
	if got := rungit(t, clone, "rev-parse", "refs/freeside/imports/plan-test"); got != base {
		t.Fatalf("CAS overwrote competing ref: got %s want %s", got, base)
	}
}

func TestCommitPlanOperationalValidationFailureDoesNotFallback(t *testing.T) {
	plan := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"Update a","remainder":true}]}`)
	injected := errors.New("injected validation I/O failure")
	res, err, _, _ := runCommitPlanImport(t,
		map[string]string{"a.txt": "old"}, map[string]string{"a.txt": "new"}, plan,
		domain.CommitPlanPlanPreferred, func(opts *Options, _ string) {
			opts.planValidationHook = func() error { return injected }
		})
	if !errors.Is(err, injected) || res.CommitSHA != "" || res.CommitPlanNotice != nil {
		t.Fatalf("operational failure fell back: result=%+v err=%v", res, err)
	}
}

func TestCommitPlanSecretPastMessageCapStillBlocks(t *testing.T) {
	escaped := `ghp_\u0041` + strings.Repeat("A", 35)
	message := strings.Repeat("x", 64) + " " + escaped
	plan := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"` + message + `","remainder":true}]}`)
	res, err, _, _ := runCommitPlanImport(t,
		map[string]string{"a.txt": "old"}, map[string]string{"a.txt": "new"}, plan,
		domain.CommitPlanPlanPreferred, func(opts *Options, _ string) { opts.Policy.MaxCommitMessageBytes = 8 })
	if err != nil || res.CommitSHA != "" || len(res.Findings) == 0 || res.Findings[0].Kind != FindingCommitPlanSecret {
		t.Fatalf("over-cap secret result=%+v err=%v", res, err)
	}
}

func TestCommitPlanShapeAndOrdering(t *testing.T) {
	changes := []plannedChange{
		{path: "a.txt", kind: ChangeAdded, mode: "100644", oid: strings.Repeat("1", 40)},
		{path: "b.txt", kind: ChangeAdded, mode: "100644", oid: strings.Repeat("2", 40)},
	}
	pol := Policy{}.withDefaults()
	cases := map[string]string{
		"empty groups":          `{"version":"freeside.commit-plan/v1","groups":[]}`,
		"empty group name":      `{"version":"freeside.commit-plan/v1","groups":[{"name":" ","message":"m","remainder":true}]}`,
		"empty paths":           `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":[]}]}`,
		"unknown path":          `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":["x.txt"]}]}`,
		"duplicate within":      `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":["a.txt","a.txt"]},{"name":"r","message":"m","remainder":true}]}`,
		"duplicate across":      `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":["a.txt"]},{"name":"y","message":"m","paths":["a.txt"]},{"name":"r","message":"m","remainder":true}]}`,
		"incomplete":            `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":["a.txt"]}]}`,
		"both discriminator":    `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":["a.txt"],"remainder":true}]}`,
		"remainder false":       `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","remainder":false}]}`,
		"remainder null":        `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","remainder":null}]}`,
		"paths null":            `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":null}]}`,
		"neither discriminator": `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m"}]}`,
		"remainder not last":    `{"version":"freeside.commit-plan/v1","groups":[{"name":"r","message":"m","remainder":true},{"name":"x","message":"m","paths":["a.txt"]}]}`,
		"two remainders":        `{"version":"freeside.commit-plan/v1","groups":[{"name":"r1","message":"m","remainder":true},{"name":"r2","message":"m","remainder":true}]}`,
		"reserved plan path":    `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":[".freeside-commit-plan.json"]}]}`,
		"reserved plan child":   `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":[".freeside-commit-plan.json/child"]}]}`,
		"reserved plan case":    `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":[".FREESIDE-COMMIT-PLAN.JSON"]}]}`,
		"reserved plan unicode": `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":[".freeſide-commit-plan.json/child"]}]}`,
		"reserved evidence":     `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":[".freeside-evidence/x"]}]}`,
		"git metadata":          `{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":["x/.git/y"]}]}`,
		"version not first":     `{"groups":[],"version":"freeside.commit-plan/v1"}`,
		"unknown version":       `{"version":"freeside.commit-plan/v2","groups":[]}`,
		"unknown field":         `{"version":"freeside.commit-plan/v1","groups":[],"extra":true}`,
		"trailing value":        `{"version":"freeside.commit-plan/v1","groups":[]} {}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeAndResolveCommitPlan([]byte(raw), changes, nil, pol); !errors.Is(err, errPlanStructural) {
				t.Fatalf("decode = %v, want structural", err)
			}
		})
	}
	invalidUTF8 := append([]byte(`{"version":"freeside.commit-plan/v1","groups":[],"x":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	if _, err := decodeAndResolveCommitPlan(invalidUTF8, changes, nil, pol); !errors.Is(err, errPlanStructural) {
		t.Fatalf("invalid UTF-8 = %v", err)
	}
	longPath := strings.Repeat("x", int(pol.MaxPathBytes)+1)
	longPlan := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":["` + longPath + `"]}]}`)
	if _, err := decodeAndResolveCommitPlan(longPlan, changes, nil, pol); !errors.Is(err, errPlanStructural) {
		t.Fatalf("overlong path = %v", err)
	}
	deepPath := strings.Repeat("x/", pol.MaxPathDepth) + "x"
	deepPlan := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":["` + deepPath + `"]}]}`)
	if _, err := decodeAndResolveCommitPlan(deepPlan, changes, nil, pol); !errors.Is(err, errPlanStructural) {
		t.Fatalf("over-deep path = %v", err)
	}

	remainderOnly := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"all","message":"m","remainder":true}]}`)
	if groups, err := decodeAndResolveCommitPlan(remainderOnly, changes, nil, pol); err != nil || len(groups) != 1 || len(groups[0].changes) != len(changes) {
		t.Fatalf("remainder-only plan = %+v, %v", groups, err)
	}
	deletion := []plannedChange{{path: "gone.txt", kind: ChangeDeleted}}
	deletionPlan := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"delete","message":"m","paths":["gone.txt"]}]}`)
	if groups, err := decodeAndResolveCommitPlan(deletionPlan, deletion, map[string]treeEntry{"gone.txt": {mode: "100644", oid: strings.Repeat("f", 40)}}, pol); err != nil || len(groups) != 1 || len(groups[0].changes) != 1 {
		t.Fatalf("deletion-only group = %+v, %v", groups, err)
	}

	base := map[string]treeEntry{"Name": {mode: "100644", oid: strings.Repeat("a", 40)}}
	rename := []plannedChange{
		{path: "Name", kind: ChangeDeleted},
		{path: "name", kind: ChangeAdded, mode: "100644", oid: strings.Repeat("b", 40)},
	}
	bad := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"add","message":"m","paths":["name"]},{"name":"delete","message":"m","paths":["Name"]}]}`)
	if _, err := decodeAndResolveCommitPlan(bad, rename, base, pol); !errors.Is(err, errPlanStructural) {
		t.Fatalf("colliding order = %v", err)
	}
	good := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"delete","message":"m","paths":["Name"]},{"name":"add","message":"m","paths":["name"]}]}`)
	if _, err := decodeAndResolveCommitPlan(good, rename, base, pol); err != nil {
		t.Fatalf("non-colliding order = %v", err)
	}

	near := export.CommitPlanFilename + ".bak"
	nearChanges := []plannedChange{{path: near, kind: ChangeAdded, mode: "100644", oid: strings.Repeat("c", 40)}}
	nearPlan := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"one","message":"m","paths":["` + near + `"]}]}`)
	groups, err := decodeAndResolveCommitPlan(nearPlan, nearChanges, nil, pol)
	if err != nil || len(groups) != 1 || len(groups[0].changes) != 1 {
		t.Fatalf("near-prefix single-group plan = %+v, %v", groups, err)
	}

	fileDirBase := map[string]treeEntry{"node": {mode: "100644", oid: strings.Repeat("d", 40)}}
	fileDirChanges := []plannedChange{
		{path: "node", kind: ChangeDeleted},
		{path: "node/child", kind: ChangeAdded, mode: "100644", oid: strings.Repeat("e", 40)},
	}
	fileDirBad := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"add","message":"m","paths":["node/child"]},{"name":"delete","message":"m","paths":["node"]}]}`)
	if _, err := decodeAndResolveCommitPlan(fileDirBad, fileDirChanges, fileDirBase, pol); !errors.Is(err, errPlanStructural) {
		t.Fatalf("file-to-directory colliding order = %v", err)
	}
	fileDirGood := []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"delete","message":"m","paths":["node"]},{"name":"add","message":"m","paths":["node/child"]}]}`)
	if _, err := decodeAndResolveCommitPlan(fileDirGood, fileDirChanges, fileDirBase, pol); err != nil {
		t.Fatalf("file-to-directory valid order = %v", err)
	}

	for _, tc := range []struct {
		name       string
		descendant string
		parent     string
		badPlan    string
		goodPlan   string
	}{
		{
			name:       "nfd descendant before nfc parent",
			descendant: "e\u0301/child",
			parent:     "\u00e9",
			badPlan:    `{"version":"freeside.commit-plan/v1","groups":[{"name":"add","message":"m","paths":["\u00e9"]},{"name":"delete","message":"m","paths":["e\u0301/child"]}]}`,
			goodPlan:   `{"version":"freeside.commit-plan/v1","groups":[{"name":"delete","message":"m","paths":["e\u0301/child"]},{"name":"add","message":"m","paths":["\u00e9"]}]}`,
		},
		{
			name:       "nfc descendant before nfd parent",
			descendant: "\u00e9/child",
			parent:     "e\u0301",
			badPlan:    `{"version":"freeside.commit-plan/v1","groups":[{"name":"add","message":"m","paths":["e\u0301"]},{"name":"delete","message":"m","paths":["\u00e9/child"]}]}`,
			goodPlan:   `{"version":"freeside.commit-plan/v1","groups":[{"name":"delete","message":"m","paths":["\u00e9/child"]},{"name":"add","message":"m","paths":["e\u0301"]}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := map[string]treeEntry{tc.descendant: {mode: "100644", oid: strings.Repeat("f", 40)}}
			changes := []plannedChange{
				{path: tc.descendant, kind: ChangeDeleted},
				{path: tc.parent, kind: ChangeAdded, mode: "100644", oid: strings.Repeat("a", 40)},
			}
			if _, err := decodeAndResolveCommitPlan([]byte(tc.badPlan), changes, base, pol); !errors.Is(err, errPlanStructural) {
				t.Fatalf("colliding normalization order = %v", err)
			}
			if _, err := decodeAndResolveCommitPlan([]byte(tc.goodPlan), changes, base, pol); err != nil {
				t.Fatalf("non-colliding normalization order = %v", err)
			}
		})
	}
}

func TestGitHub1MessageScreening(t *testing.T) {
	pol := Policy{}.withDefaults()
	rejected := []string{
		"", " \n\t", "bad\x00", "bad\r", "bad\x1b", "bad\x7f", "bad\u202e", "bad\u0085", "bad\u200b",
		"subject [skip ci]", "subject [ci skip]", "body [no ci] here", "[skip actions]", "[actions skip]",
		"subject\n\nskip-checks: true", "subject\n\nSigned-off-by: Agent <agent@example.test>",
		"subject\n\nCo-authored-by: Agent <agent@example.test>", "subject\n\nReviewed-by: Agent",
		"subject\n\n" + agentProposedTrailer,
	}
	for _, keyword := range []string{"close", "closes", "closed", "fix", "fixes", "fixed", "resolve", "resolves", "resolved"} {
		for _, colon := range []string{"", ":"} {
			for _, ref := range []string{"#1", "owner/repo#1", "https://github.com/owner/repo/issues/1"} {
				rejected = append(rejected, strings.ToUpper(keyword)+colon+" "+ref)
			}
		}
	}
	for _, message := range rejected {
		if kind, err := screenCommitMessage(message, pol); err == nil || kind == "" {
			t.Errorf("message %q passed screening", message)
		}
	}
	pol.MaxCommitMessageBytes = 4
	if _, err := screenCommitMessage("12345", pol); err == nil {
		t.Error("over-cap message passed")
	}
	if _, err := screenCommitMessage("ordinary subject\n\nordinary body", Policy{}.withDefaults()); err != nil {
		t.Fatalf("ordinary message rejected: %v", err)
	}
	groups := []resolvedCommitGroup{
		{message: "Fixes #1", changes: []plannedChange{{path: "a"}}},
		{message: "Fixes #2", changes: []plannedChange{{path: "b"}}},
	}
	findings := screenCommitMessages(groups, Policy{}.withDefaults())
	if len(findings) != 2 || findings[0].GroupOrdinal != 1 || findings[1].GroupOrdinal != 2 ||
		findings[0].Kind != messageFindingCloseDirective || findings[1].Kind != messageFindingCloseDirective {
		t.Fatalf("message findings = %+v", findings)
	}
	for _, message := range []string{"", "bad\x00", strings.Repeat("x", DefaultMaxCommitMessageBytes+1), "Fixes #1"} {
		emptyRemainder := []resolvedCommitGroup{{message: message}}
		if findings := screenCommitMessages(emptyRemainder, Policy{}.withDefaults()); len(findings) != 0 {
			t.Errorf("empty remainder message %q produced findings: %+v", message, findings)
		}
	}
	seenKinds := map[commitMessageFindingKind]bool{}
	for _, message := range []string{"", "bad\x00", strings.Repeat("x", DefaultMaxCommitMessageBytes+1), "Fixes #1", "[skip ci]", "Signed-off-by: x"} {
		kind, err := screenCommitMessage(message, Policy{}.withDefaults())
		if err == nil {
			t.Fatalf("mapping fixture %q passed", message)
		}
		seenKinds[kind] = true
	}
	if len(seenKinds) != len(allCommitMessageFindingKinds) {
		t.Fatalf("finding mapping covers %d kinds, registry has %d: %v", len(seenKinds), len(allCommitMessageFindingKinds), seenKinds)
	}
}

func TestCommitPlanPreboundsCaseFoldedDuplicateKeys(t *testing.T) {
	pol := Policy{}.withDefaults()
	pol.MaxCommitPlanGroups = 1
	raw := []byte(`{"version":"freeside.commit-plan/v1","groups":[],"Groups":[{},{}]}`)
	if _, err := decodeAndResolveCommitPlan(raw, nil, nil, pol); !errors.Is(err, errPlanStructural) {
		t.Fatalf("case-folded group overflow = %v", err)
	}
	pol = Policy{}.withDefaults()
	pol.MaxEntries = 1
	raw = []byte(`{"version":"freeside.commit-plan/v1","groups":[{"name":"x","message":"m","paths":[],"Paths":["a","b"]}]}`)
	if _, err := decodeAndResolveCommitPlan(raw, nil, nil, pol); !errors.Is(err, errPlanStructural) {
		t.Fatalf("case-folded path overflow = %v", err)
	}
}

func TestCommitPlanGroupCap(t *testing.T) {
	groups := strings.Repeat(`{"name":"x","message":"m","remainder":true},`, DefaultMaxCommitPlanGroups+1)
	groups = strings.TrimSuffix(groups, ",")
	raw := []byte(fmt.Sprintf(`{"version":"freeside.commit-plan/v1","groups":[%s]}`, groups))
	if _, err := decodeAndResolveCommitPlan(raw, nil, nil, Policy{}.withDefaults()); !errors.Is(err, errPlanStructural) {
		t.Fatalf("group cap = %v", err)
	}
}
