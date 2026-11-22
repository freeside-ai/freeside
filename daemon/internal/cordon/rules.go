package cordon

import (
	_ "embed"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

//go:embed rules.sb
var ruleTemplate string

// renderMarker is the line in rules.sb where the list-valued blocks go.
const renderMarker = ";; @rendered\n"

// RenderRules returns the rule file a launch under cfg runs: the embedded
// rules with the configuration's rename guards and list-valued inputs
// rendered in at the marker, in a fixed order. The per-launch paths stay -D
// parameters (sandboxParams), so two launches under one configuration run
// identical bytes and RuleDigest names them.
func RenderRules(cfg Config) []byte {
	head, tail, ok := strings.Cut(ruleTemplate, renderMarker)
	if !ok || strings.Contains(tail, renderMarker) {
		panic("cordon: rules.sb must hold the render marker exactly once")
	}
	var b strings.Builder
	b.WriteString(head)
	b.WriteString(";; The directories above every denied path, and each configured root\n")
	b.WriteString(";; itself (renameGuards). A rename names only its two paths, so renaming\n")
	b.WriteString(";; one of these would bring a denied path out under another name. A literal\n")
	b.WriteString(";; denies the directory's own rename, mode, and ACL; its entries stay open.\n")
	b.WriteString("(deny file-write*")
	for _, dir := range renameGuards(cfg) {
		fmt.Fprintf(&b, "\n  (literal %s)", sbString(dir))
	}
	b.WriteString(")\n\n")
	b.WriteString(";; The configured protected roots (Config.ProtectedRoots), sorted. Metadata\n")
	b.WriteString(";; stays readable; contents, writes, and Unix-socket connects are denied.\n")
	for _, root := range cfg.ProtectedRoots {
		quoted := sbString(root)
		fmt.Fprintf(&b, "(deny file-read-data file-read-xattr file-write*\n  (subpath %s))\n", quoted)
		fmt.Fprintf(&b, "(deny network-outbound\n  (remote unix-socket (subpath %s)))\n", quoted)
	}
	if len(cfg.ReservedPorts) > 0 {
		b.WriteString("\n;; The ports verification reserves (Config.ReservedPorts), sorted. A filter\n")
		b.WriteString(";; takes one port, so a list can't be a parameter.\n(deny network-bind")
		for _, port := range cfg.ReservedPorts {
			fmt.Fprintf(&b, "\n  (local ip \"localhost:%d\")", port)
		}
		b.WriteString(")\n")
	}
	b.WriteString(tail)
	return []byte(b.String())
}

// homePath finds each HOME-relative path rules.sb denies.
var homePath = regexp.MustCompile(`\(param "HOME(?:_RE)?"\) "(/[^"]+)"`)

// renameGuards lists, sorted, every directory above a path the rules deny
// (rules.sb's HOME-relative paths and the configured paths), up to but not
// including /, and the configured paths themselves, which are the bases of
// the regex denials. The HOME-relative paths are read from rules.sb, so a
// denial added there is guarded without a second list to keep in step.
func renameGuards(cfg Config) []string {
	guarded := map[string]bool{}
	above := func(path string) {
		for dir := filepath.Dir(path); dir != "/"; dir = filepath.Dir(dir) {
			guarded[dir] = true
		}
	}
	for _, m := range homePath.FindAllStringSubmatch(ruleTemplate, -1) {
		above(cfg.Home + m[1])
	}
	for _, path := range append([]string{cfg.Home, cfg.OwnerClaudeConfig, cfg.Projects, cfg.UserTemp, cfg.UserCache}, cfg.ProtectedRoots...) {
		guarded[path] = true
		above(path)
	}
	return slices.Sorted(maps.Keys(guarded))
}

// RuleDigest names the rule file a launch under cfg runs.
func RuleDigest(cfg Config) string { return contentaddr.Sum(RenderRules(cfg)) }

// sbString quotes s as a Seatbelt string literal. Seatbelt's reader takes
// backslash escapes, so a backslash or a double quote in a path is escaped.
func sbString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// sandboxParams are the -D parameters a launch's rule file reads: the
// configuration's single-valued inputs and the launch's two paths. A value
// that goes into a regex is quoted, so a home such as /Users/first.last
// matches only itself.
func sandboxParams(cfg Config, runRoot, tag string) []string {
	params := []struct{ name, value string }{
		{"HOME", cfg.Home},
		{"HOME_RE", regexp.QuoteMeta(cfg.Home)},
		{"OWNER_CLAUDE_CONFIG", cfg.OwnerClaudeConfig},
		{"PROJECTS_RE", regexp.QuoteMeta(cfg.Projects)},
		{"USER_TEMP_RE", regexp.QuoteMeta(cfg.UserTemp)},
		{"USER_CACHE_RE", regexp.QuoteMeta(cfg.UserCache)},
		{"RUN_ROOT", runRoot},
		{"LAUNCH_TAG", tag},
	}
	args := make([]string, 0, 2*len(params))
	for _, p := range params {
		args = append(args, "-D", p.name+"="+p.value)
	}
	return args
}
