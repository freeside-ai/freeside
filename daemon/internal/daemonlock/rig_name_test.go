package daemonlock

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// rigNameID is one run-ID or owner-slice shape the rig patterns admit: a
// literal lead followed by a fixed count of lowercase hex digits.
type rigNameID struct {
	lead   string
	digits int
}

func (id rigNameID) sample() string {
	return id.lead + strings.Repeat("0123456789abcdef", 2)[:id.digits]
}

// rigNameShape pins, independently of the production definition, one prefix
// and ID shape with the roles each resource class admits for it. bare marks
// a container named by prefix and ID alone.
type rigNameShape struct {
	prefix     string
	id         rigNameID
	containers []string
	volumes    []string
	networks   []string
	bare       bool
}

var (
	rigProductionRunID  = rigNameID{lead: "c", digits: 31}
	rigConformanceRunID = rigNameID{lead: "conf-", digits: 16}
	rigReviewRunID      = rigNameID{lead: "review-", digits: 24}
	rigPreJobRunID      = rigNameID{digits: 16}
	rigOwnerSlice       = rigNameID{digits: 12}
)

// rigAdmittedNameShapes is the namespace the rig patterns admitted when this
// table was written. It is a copy on purpose: it proves a change to how the
// patterns are built leaves the admitted names exactly as they were.
var rigAdmittedNameShapes = []rigNameShape{
	{
		prefix: "freeside-handoff-", id: rigProductionRunID,
		containers: []string{
			"seeder", "observer", "ins-seed", "ins-check", "prompt-seed", "prompt-check",
			"inputs-seed", "inputs-check", "cfg-seed", "cfg-check", "projects-check",
			"sess-check", "writer-check", "cred-pre", "cred-post", "agent", "exporter",
		},
		volumes:  []string{"ws", "ins", "prompt", "inputs", "cfg", "projects", "session-env"},
		networks: []string{"egress"},
	},
	{
		prefix: "freeside-handoff-", id: rigConformanceRunID,
		containers: []string{"seeder", "observer", "ins-seed", "ins-check", "writer-check", "agent", "exporter"},
		volumes:    []string{"ws", "ins"},
		networks:   []string{"egress"},
	},
	{
		prefix: "freeside-handoff-", id: rigReviewRunID,
		containers: []string{"seeder", "observer"},
		volumes:    []string{"ws"},
	},
	{
		prefix: "freeside-review-", id: rigReviewRunID,
		containers: []string{
			"ws-obs", "workspace-observer", "agents-init", "agents-obs", "agents-observer",
			"snap-init", "snap-obs", "inputs-init", "inputs-obs", "codex", "claude",
		},
		volumes:  []string{"agents", "snap", "inputs"},
		networks: []string{"egress"},
	},
	{
		prefix: "freeside-ward-conf-", id: rigPreJobRunID,
		containers: []string{"prejob"},
	},
	{
		prefix: "freeside-ward-conf-", id: rigConformanceRunID,
		containers: []string{
			"liveness", "seed", "audit", "excl-writer", "excl-second",
			"net-live", "net", "inx-live", "inx",
		},
		volumes: []string{"cred", "liveness-ws", "excl-ws", "net-live-ws", "inx-ws"},
	},
	{prefix: "freeside-preflight-credential-", id: rigOwnerSlice, bare: true},
	{prefix: "freeside-lineup-probe-", id: rigOwnerSlice, bare: true, networks: []string{"egress"}},
}

type rigNameClass struct {
	name    string
	pattern *regexp.Regexp
	roles   func(rigNameShape) []string
	bare    func(rigNameShape) bool
}

func rigNameClasses() []rigNameClass {
	return []rigNameClass{
		{
			name: "container", pattern: rigContainerNamePattern,
			roles: func(s rigNameShape) []string { return s.containers },
			bare:  func(s rigNameShape) bool { return s.bare },
		},
		{
			name: "volume", pattern: rigVolumeNamePattern,
			roles: func(s rigNameShape) []string { return s.volumes },
			bare:  func(rigNameShape) bool { return false },
		},
		{
			name: "network", pattern: rigNetworkNamePattern,
			roles: func(s rigNameShape) []string { return s.networks },
			bare:  func(rigNameShape) bool { return false },
		},
	}
}

func rigShapeName(prefix, id, role string) string {
	if role == "" {
		return prefix + id
	}
	return prefix + id + "-" + role
}

// TestRigNamePatternsAdmitExactlyTheOwnedNamespace checks every combination of
// a known prefix, ID shape, and role (the empty role is a bare name) against
// each class: a name is admitted exactly when the pinned table lists that
// role for that prefix, ID shape, and class. A role from another shape or
// class, an unknown role, and a prefix paired with another prefix's ID all
// land on the refused side of the same loop.
func TestRigNamePatternsAdmitExactlyTheOwnedNamespace(t *testing.T) {
	t.Parallel()
	var prefixes []string
	var ids []rigNameID
	roles := []string{"", "unknown", "seeder-observer"}
	for _, shape := range rigAdmittedNameShapes {
		prefixes = append(prefixes, shape.prefix)
		ids = append(ids, shape.id)
		roles = append(roles, shape.containers...)
		roles = append(roles, shape.volumes...)
		roles = append(roles, shape.networks...)
	}
	prefixes, ids, roles = sortedUnique(prefixes), uniqueIDs(ids), sortedUnique(roles)

	for _, class := range rigNameClasses() {
		admitted := 0
		for _, prefix := range prefixes {
			for _, id := range ids {
				for _, role := range roles {
					name := rigShapeName(prefix, id.sample(), role)
					want := rigPinnedAdmits(class, prefix, id, role)
					if got := class.pattern.MatchString(name); got != want {
						t.Errorf("%s pattern MatchString(%q) = %t, want %t", class.name, name, got, want)
					}
					if want {
						admitted++
					}
				}
			}
		}
		if admitted == 0 {
			t.Errorf("%s pattern admits no pinned name", class.name)
		}
	}
}

// TestRigNamePatternsRefuseNearMisses mutates every admitted name: its ID one
// digit short or long, in uppercase or non-hex, or with text before or after.
func TestRigNamePatternsRefuseNearMisses(t *testing.T) {
	t.Parallel()
	for _, class := range rigNameClasses() {
		for _, shape := range rigAdmittedNameShapes {
			roles := slices.Clone(class.roles(shape))
			if class.bare(shape) {
				roles = append(roles, "")
			}
			for _, role := range roles {
				hex := strings.Repeat("0123456789abcdef", 3)[:shape.id.digits+1]
				valid := rigShapeName(shape.prefix, shape.id.sample(), role)
				if !class.pattern.MatchString(valid) {
					t.Fatalf("%s pattern refuses pinned name %q", class.name, valid)
				}
				for _, miss := range []string{
					rigShapeName(shape.prefix, shape.id.lead+hex[:shape.id.digits-1], role),
					rigShapeName(shape.prefix, shape.id.lead+hex, role),
					rigShapeName(shape.prefix, shape.id.lead+strings.ToUpper(hex[:shape.id.digits]), role),
					rigShapeName(shape.prefix, shape.id.lead+"g"+hex[1:shape.id.digits], role),
					valid + "\n", valid + "x", valid + "-", " " + valid, "x" + valid,
					strings.ToUpper(valid[:1]) + valid[1:],
				} {
					if class.pattern.MatchString(miss) {
						t.Errorf("%s pattern admits near miss %q", class.name, miss)
					}
				}
			}
		}
	}
}

func rigPinnedAdmits(class rigNameClass, prefix string, id rigNameID, role string) bool {
	for _, shape := range rigAdmittedNameShapes {
		if shape.prefix != prefix || shape.id != id {
			continue
		}
		if role == "" {
			return class.bare(shape)
		}
		return slices.Contains(class.roles(shape), role)
	}
	return false
}

func sortedUnique(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)
	return slices.Compact(values)
}

func uniqueIDs(ids []rigNameID) []rigNameID {
	var unique []rigNameID
	for _, id := range ids {
		if !slices.Contains(unique, id) {
			unique = append(unique, id)
		}
	}
	return unique
}
