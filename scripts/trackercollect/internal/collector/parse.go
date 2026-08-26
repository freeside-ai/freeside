package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	claimMarker           = "<!-- freeside-work-claim:v1 -->"
	releaseMarker         = "<!-- freeside-work-release:v1 -->"
	reservationMarker     = "<!-- freeside-planning-reservation:v1 -->"
	claimMarkerPattern    = regexp.MustCompile(`(?m)^[ \t]*<!-- freeside-work-claim:v1 -->[ \t]*$`)
	releaseMarkerPattern  = regexp.MustCompile(`(?m)^[ \t]*<!-- freeside-work-release:v1 -->[ \t]*$`)
	reserveMarkerPattern  = regexp.MustCompile(`(?m)^[ \t]*<!-- freeside-planning-reservation:v1 -->[ \t]*$`)
	repositoryPartPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	waveTitlePattern      = regexp.MustCompile(`^Wave [0-9]+ \([^)]*\) tracking$`)
	checkboxPattern       = regexp.MustCompile(`(?m)^[ \t]*(?:-|[0-9]+\.) \[([ xX])\][ \t]+#([0-9]+)(?:[ \t]+.*)?$`)
	headingPattern        = regexp.MustCompile(`^(#{1,6})[ \t]+(.+?)[ \t]*$`)
	scopeLinePattern      = regexp.MustCompile(`(?im)^Scope:[ \t]*.*$`)
	claimPattern          = regexp.MustCompile(`(?m)^Claim:[ \t]*(\S+)[ \t]*$`)
	releasePattern        = regexp.MustCompile(`(?m)^Release:[ \t]*(\S+)[ \t]*$`)
	releasesIDPattern     = regexp.MustCompile(`(?m)^Releases-claim:[ \t]*([0-9]+)[ \t]*$`)
	planPattern           = regexp.MustCompile(`(?m)^Plan:[ \t]*#([0-9]+)[ \t]*$`)
	stackedPattern        = regexp.MustCompile(`(?im)^.*stacked-on[ \t]*:?[ \t]*(?:(PR)[ \t]+)?#([0-9]+).*$`)
	identityPattern       = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)
	backtickPattern       = regexp.MustCompile("`([^`]+)`")
	pathPattern           = regexp.MustCompile(`(?:[A-Za-z0-9_.-]+/)+(?:[A-Za-z0-9_.-]+)?|[A-Za-z0-9_.-]+\.[A-Za-z0-9_.-]+`)
)

func ParseRepository(value string) (Repository, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 3 {
		return Repository{}, fmt.Errorf("expected HOST/OWNER/NAME")
	}
	for _, part := range parts {
		if !repositoryPartPattern.MatchString(part) {
			return Repository{}, fmt.Errorf("invalid repository component %q", part)
		}
	}
	return Repository{Host: parts[0], Owner: parts[1], Name: parts[2]}, nil
}

func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func ParseCheckboxEntries(body string) []TrackerEntry {
	entries, _ := parseCheckboxEntries(body)
	return entries
}

func parseCheckboxEntries(body string) ([]TrackerEntry, []string) {
	evidence := markdownEvidence(body)
	matches := checkboxPattern.FindAllStringSubmatchIndex(evidence, -1)
	entries := make([]TrackerEntry, 0, len(matches))
	var invalidNumbers []string
	for _, match := range matches {
		rawNumber := evidence[match[4]:match[5]]
		parsed, err := strconv.ParseInt(rawNumber, 10, 32)
		number := int(parsed)
		if err != nil || number <= 0 {
			invalidNumbers = append(invalidNumbers, rawNumber)
			continue
		}
		entries = append(entries, TrackerEntry{
			UnitNumber: number,
			Checked:    strings.EqualFold(evidence[match[2]:match[3]], "x"),
			Line:       body[match[0]:match[1]],
		})
	}
	return entries, invalidNumbers
}

func extractSections(body, wanted string) []string {
	rawLines := strings.Split(body, "\n")
	evidenceLines := strings.Split(markdownEvidence(body), "\n")
	var sections []string
	for start := 0; start < len(evidenceLines); start++ {
		match := headingPattern.FindStringSubmatch(evidenceLines[start])
		if match == nil || !strings.EqualFold(strings.TrimSpace(match[2]), wanted) {
			continue
		}
		level := len(match[1])
		end := len(evidenceLines)
		for i := start + 1; i < len(evidenceLines); i++ {
			next := headingPattern.FindStringSubmatch(evidenceLines[i])
			if next != nil && len(next[1]) <= level {
				end = i
				break
			}
		}
		sections = append(sections, strings.TrimSpace(strings.Join(rawLines[start:end], "\n")))
		start = end - 1
	}
	return sections
}

func extractSection(body, wanted string) string {
	sections := extractSections(body, wanted)
	if len(sections) > 0 {
		return sections[0]
	}
	return ""
}

func extractScopeLines(body string) []string {
	evidence := markdownEvidence(body)
	matches := scopeLinePattern.FindAllStringIndex(evidence, -1)
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		result = append(result, strings.TrimSpace(body[match[0]:match[1]]))
	}
	return result
}

func markdownEvidence(body string) string {
	lines := strings.SplitAfter(body, "\n")
	var result strings.Builder
	inFence := false
	fenceCharacter := byte(0)
	fenceLength := 0
	for _, line := range lines {
		character, length, closing, isFence := markdownFence(line, inFence, fenceCharacter, fenceLength)
		indentedCode := !inFence && !isFence && hasCodeIndent(line)
		if inFence || isFence || indentedCode {
			result.WriteString(blankMarkdownLine(line))
		} else {
			result.WriteString(line)
		}
		if !inFence && isFence {
			inFence, fenceCharacter, fenceLength = true, character, length
		} else if inFence && closing {
			inFence, fenceCharacter, fenceLength = false, 0, 0
		}
	}
	return maskHTMLComments(result.String())
}

func maskHTMLComments(body string) string {
	lines := strings.SplitAfter(body, "\n")
	var result strings.Builder
	inComment := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		if !inComment && (trimmed == claimMarker || trimmed == releaseMarker || trimmed == reservationMarker) {
			result.WriteString(line)
			continue
		}
		for index := 0; index < len(line); {
			if inComment {
				if strings.HasPrefix(line[index:], "-->") {
					result.WriteString("   ")
					index += 3
					inComment = false
					continue
				}
				if line[index] == '\n' || line[index] == '\r' {
					result.WriteByte(line[index])
				} else {
					result.WriteByte(' ')
				}
				index++
				continue
			}
			if strings.HasPrefix(line[index:], "<!--") {
				result.WriteString("    ")
				index += 4
				inComment = true
				continue
			}
			result.WriteByte(line[index])
			index++
		}
	}
	return result.String()
}

func markdownFence(line string, inFence bool, fenceCharacter byte, fenceLength int) (byte, int, bool, bool) {
	trimmed := strings.TrimRight(line, "\r\n")
	leading := len(trimmed) - len(strings.TrimLeft(trimmed, " \t"))
	if leading > 3 || leading >= len(trimmed) {
		return 0, 0, false, false
	}
	character := trimmed[leading]
	if character != '`' && character != '~' {
		return 0, 0, false, false
	}
	length := 0
	for leading+length < len(trimmed) && trimmed[leading+length] == character {
		length++
	}
	if length < 3 {
		return 0, 0, false, false
	}
	if !inFence {
		return character, length, false, true
	}
	closing := character == fenceCharacter && length >= fenceLength && strings.TrimSpace(trimmed[leading+length:]) == ""
	return character, length, closing, true
}

func hasCodeIndent(line string) bool {
	return strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
}

func blankMarkdownLine(line string) string {
	bytes := []byte(line)
	for i := range bytes {
		if bytes[i] != '\n' && bytes[i] != '\r' {
			bytes[i] = ' '
		}
	}
	return string(bytes)
}

func parseDeclaredPaths(section string) []string {
	seen := make(map[string]bool)
	var paths []string
	lines := strings.Split(markdownEvidence(section), "\n")
	for _, line := range lines[minimum(1, len(lines)):] {
		candidates := backtickPattern.FindAllStringSubmatch(line, -1)
		if len(candidates) == 0 {
			for _, value := range pathPattern.FindAllString(line, -1) {
				candidates = append(candidates, []string{"", value})
			}
		}
		for _, candidate := range candidates {
			path := strings.TrimRight(strings.TrimSpace(candidate[1]), ",.;:")
			if path == "" || strings.ContainsAny(path, " \t") || strings.HasPrefix(path, "#") {
				continue
			}
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

type stackedReference struct {
	Number int
	Kind   string
}

func parseStackedReferences(section string) ([]stackedReference, []string) {
	seen := make(map[string]bool)
	var refs []stackedReference
	var invalidNumbers []string
	for _, match := range stackedPattern.FindAllStringSubmatch(markdownEvidence(section), -1) {
		parsed, err := strconv.ParseInt(match[2], 10, 32)
		number := int(parsed)
		if err != nil || number <= 0 {
			invalidNumbers = append(invalidNumbers, match[2])
			continue
		}
		kind := "issue"
		if strings.EqualFold(match[1], "PR") {
			kind = "pull_request"
		}
		key := fmt.Sprintf("%s:%d", kind, number)
		if !seen[key] {
			seen[key] = true
			refs = append(refs, stackedReference{Number: number, Kind: kind})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Number == refs[j].Number {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].Number < refs[j].Number
	})
	return refs, invalidNumbers
}

func parseRepositoryIdentity(raw json.RawMessage) (RepositoryIdentity, error) {
	if len(raw) == 0 {
		return RepositoryIdentity{}, fmt.Errorf("headRepository field is absent")
	}
	if string(raw) == "null" {
		return RepositoryIdentity{State: "explicit-null"}, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return RepositoryIdentity{}, fmt.Errorf("headRepository is malformed: %w", err)
	}
	nameRaw, ok := object["nameWithOwner"]
	if !ok {
		return RepositoryIdentity{}, fmt.Errorf("headRepository.nameWithOwner field is absent")
	}
	var value string
	if err := json.Unmarshal(nameRaw, &value); err != nil || !identityPattern.MatchString(value) {
		return RepositoryIdentity{}, fmt.Errorf("headRepository.nameWithOwner is malformed")
	}
	return RepositoryIdentity{State: "present", NameWithOwner: &value}, nil
}

func identityMatches(identity RepositoryIdentity, canonical string) bool {
	return identity.State == "present" && identity.NameWithOwner != nil && strings.EqualFold(*identity.NameWithOwner, canonical)
}

func minimum(a, b int) int {
	if a < b {
		return a
	}
	return b
}
