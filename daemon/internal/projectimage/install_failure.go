package projectimage

import (
	"regexp"
	"strings"
)

// maxInstallFailureBytes bounds the npm lines an install failure carries, so
// they fit in the leading text the publication lane keeps as evidence.
const maxInstallFailureBytes = 512

const (
	npmIntegrityCode = "npm error code EINTEGRITY"
	npmUsageCode     = "npm error code EUSAGE"
	npmOutOfSync     = "npm error `npm ci` can only install packages when your package.json and package-lock.json " +
		"or npm-shrinkwrap.json are in sync."
	npmLogPointer = "npm error A complete log of this run can be found in"
)

// buildKitLinePrefix is the step number and elapsed time BuildKit's plain
// progress puts before a RUN step's output line ("#7 4.623 "), or the elapsed
// time alone in the failed step's closing excerpt ("4.623 ").
var buildKitLinePrefix = regexp.MustCompile(`^(?:#\d+ )?\d+\.\d+ `)

// installFailure reads a failed image build's output for an npm install
// failure only the candidate's own manifests can cause, and returns npm's
// lines describing it. It matches two codes from the npm the image's Node
// ships: EINTEGRITY, a tarball that doesn't match its lockfile integrity, and
// EUSAGE with npm ci's out-of-sync message, a lockfile that doesn't hold what
// package.json declares. Everything else, including registry, proxy, and
// network failures and codes such as E404 and ETARGET, returns false, so the
// build stays retryable without positive evidence.
//
// The match is on whole lines. npm prefixes every line of a multi-line
// message with its own level and title ("npm warn deprecated …"), so text the
// candidate controls, such as a lockfile's deprecation message, can't produce
// a bare "npm error code" line.
func installFailure(output []byte) (string, bool) {
	var lines []string
	for line := range strings.Lines(string(output)) {
		lines = append(lines, buildKitLinePrefix.ReplaceAllString(strings.TrimRight(line, "\r\n"), ""))
	}
	for index, line := range lines {
		if line != npmIntegrityCode && line != npmUsageCode {
			continue
		}
		detail := npmErrorDetail(lines[index+1:])
		if line == npmIntegrityCode {
			return boundedLines(append([]string{line}, detail...)), true
		}
		if len(detail) == 0 || !strings.HasPrefix(detail[0], npmOutOfSync) {
			return "", false
		}
		// The usage text after the mismatched entries is npm ci's help.
		kept := []string{line, detail[0]}
		for _, entry := range detail[1:] {
			if strings.HasPrefix(entry, "npm error Missing: ") || strings.HasPrefix(entry, "npm error Invalid: ") {
				kept = append(kept, entry)
			}
		}
		return boundedLines(kept), true
	}
	return "", false
}

// npmErrorDetail returns the non-empty npm error lines that follow an error
// code, up to npm's log pointer or the first line npm didn't write.
func npmErrorDetail(lines []string) []string {
	var detail []string
	for _, line := range lines {
		if !strings.HasPrefix(line, "npm error") || strings.HasPrefix(line, npmLogPointer) {
			break
		}
		if strings.TrimSpace(line) != "npm error" {
			detail = append(detail, line)
		}
	}
	return detail
}

// boundedLines joins whole lines up to maxInstallFailureBytes; the first line,
// the error code, is always kept.
func boundedLines(lines []string) string {
	joined := lines[0]
	for _, line := range lines[1:] {
		if len(joined)+1+len(line) > maxInstallFailureBytes {
			break
		}
		joined += "\n" + line
	}
	return joined
}
