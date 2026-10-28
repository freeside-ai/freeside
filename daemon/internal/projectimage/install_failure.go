package projectimage

import (
	"regexp"
	"strings"
)

// maxInstallFailureBytes bounds the installer lines an install failure
// carries, so they fit in the leading text the publication lane keeps as
// evidence.
const maxInstallFailureBytes = 512

const (
	npmIntegrityCode = "npm error code EINTEGRITY"
	npmUsageCode     = "npm error code EUSAGE"
	npmOutOfSync     = "npm error `npm ci` can only install packages when your package.json and package-lock.json " +
		"or npm-shrinkwrap.json are in sync."
	npmLogPointer = "npm error A complete log of this run can be found in"
)

var (
	// goChecksumMismatch is Go's line for a module download, or a module's
	// go.mod, whose hash differs from the one recorded for it. Go names the
	// two hashes on the lines after it.
	goChecksumMismatch = regexp.MustCompile(`^verifying \S+@\S+: checksum mismatch$`)
	// goProxyNotFound is Go's line for a module version the build's one proxy
	// says it does not serve, naming the version go.mod chose and, when a
	// replace chose it, the replacement. Go writes it after "go: ", or
	// indented as the last link of a "requires" chain when the version is a
	// dependency's requirement. It is anchored to that proxy's URL, so no
	// other host's answer matches.
	goProxyNotFound = regexp.MustCompile(`^(?:go: |\t)\S+@\S+(?: \(replaced by \S+@\S+\))?: ` +
		goProxyNotFoundSuffix + `$`)
	goProxyNotFoundEnd = regexp.MustCompile(goProxyNotFoundSuffix + `$`)
	// goModRefused is Go's line before the errors it found in the main
	// module's go.mod, each on a line of its own that names go.mod and the
	// line it is on.
	goModRefused   = regexp.MustCompile(`^go: (?:errors parsing|error loading) \S*go\.mod:$`)
	goModLineError = regexp.MustCompile(`^\S*go\.mod:\d+: `)
)

// goProxyNotFoundSuffix is how Go ends an error for a module version the
// build's one proxy does not serve.
var goProxyNotFoundSuffix = `reading https://` + regexp.QuoteMeta(goModuleProxyHost) +
	`/\S+: (?:404 Not Found|410 Gone)`

// buildKitLinePrefix is the step number and elapsed time BuildKit's plain
// progress puts before a RUN step's output line ("#7 4.623 "), or the elapsed
// time alone in the failed step's closing excerpt ("4.623 ").
var buildKitLinePrefix = regexp.MustCompile(`^(?:#\d+ )?\d+\.\d+ `)

// installFailure reads a failed image build's output for a dependency
// install failure only the candidate's own manifests can cause, and returns
// the installer's lines describing it. For npm it matches two codes from the
// npm the image's Node ships: EINTEGRITY, a tarball that doesn't match its lockfile integrity, and
// EUSAGE with npm ci's out-of-sync message, a lockfile that doesn't hold what
// package.json declares. Everything else, including registry, proxy, and
// network failures and codes such as E404 and ETARGET, returns false, so the
// build stays retryable without positive evidence.
//
// The match is on whole lines. npm prefixes every line of a multi-line
// message with its own level and title ("npm warn deprecated …"), so text the
// candidate controls, such as a lockfile's deprecation message, can't produce
// a bare "npm error code" line.
//
// For Go it matches what goInstallFailure does.
func installFailure(output []byte) (string, bool) {
	var lines []string
	for line := range strings.Lines(string(output)) {
		lines = append(lines, buildKitLinePrefix.ReplaceAllString(strings.TrimRight(line, "\r\n"), ""))
	}
	if detail, ok := goInstallFailure(lines); ok {
		return detail, true
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

// goInstallFailure matches the failures of the Go warm step only the
// candidate's go.mod and go.sum can cause, from the Go the image pins:
//
//   - A checksum mismatch between a downloaded module and its go.sum hash.
//     The match needs Go's following "downloaded:" and "go.sum:" lines, so
//     a mismatch with the checksum database, which no go.sum line caused,
//     stays a host fault.
//   - A 404 or 410 from the build's one module proxy for a module version
//     go.mod or a dependency requires, which the proxy does not serve. Go's
//     "requires" chain leads to it and its indented server response follows.
//   - Go's refusal of the main module's go.mod, for an argument the rebuild
//     gate leaves to Go, such as a godebug setting Go does not know. Go
//     resolves a version it reads as a query through the proxy while it
//     reads go.mod, so an error line naming any URL but such a 404 or 410 is
//     a fetch that failed, and the whole refusal stays a host fault.
//
// A connection, proxy, or name-resolution failure returns false. Go writes
// these lines itself, and every module path and version it names has passed
// Go's own syntax check, which admits no whitespace, so the candidate's
// manifests can't forge one. A go.mod error line quotes the candidate's text,
// which can only make a refusal read as the host fault it is not, never the
// reverse.
func goInstallFailure(lines []string) (string, bool) {
	for index, line := range lines {
		switch {
		case goChecksumMismatch.MatchString(line):
			if index+2 < len(lines) && strings.HasPrefix(lines[index+1], "\tdownloaded: ") &&
				strings.HasPrefix(lines[index+2], "\tgo.sum: ") {
				return boundedLines(lines[index : index+3]), true
			}
		case goProxyNotFound.MatchString(line):
			start := goRequiresChainStart(lines, index)
			if start < 0 {
				continue
			}
			return boundedLines(append(lines[start:index+1:index+1], indentedLines(lines[index+1:])...)), true
		case goModRefused.MatchString(line):
			if detail, ok := goModRefusal(lines[index+1:]); ok {
				return boundedLines(append([]string{line}, detail...)), true
			}
		}
	}
	return "", false
}

// goRequiresChainStart returns the index of the line that starts the error
// ending at lines[end]: the line itself when Go wrote it after "go: ", or the
// "go: … requires" line heading the indented chain it ends. It returns -1
// when the indented line ends no such chain.
func goRequiresChainStart(lines []string, end int) int {
	start := end
	for start > 0 && strings.HasPrefix(lines[start], "\t") {
		start--
	}
	if !strings.HasPrefix(lines[start], "go: ") {
		return -1
	}
	for _, link := range lines[start:end] {
		if !strings.HasSuffix(link, " requires") {
			return -1
		}
	}
	return start
}

// goModRefusal returns the error lines Go wrote under its refusal of go.mod,
// with their indented continuations, and whether every one is the
// candidate's: no error line names a URL unless it ends in the pinned
// proxy's 404 or 410.
func goModRefusal(lines []string) ([]string, bool) {
	var detail []string
	for _, line := range lines {
		if !goModLineError.MatchString(line) && !strings.HasPrefix(line, "\t") {
			break
		}
		if !strings.HasPrefix(line, "\t") && strings.Contains(line, "://") &&
			!goProxyNotFoundEnd.MatchString(line) {
			return nil, false
		}
		detail = append(detail, line)
	}
	return detail, len(detail) != 0
}

// indentedLines returns the leading lines that start with a tab.
func indentedLines(lines []string) []string {
	var indented []string
	for _, line := range lines {
		if !strings.HasPrefix(line, "\t") {
			break
		}
		indented = append(indented, line)
	}
	return indented
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
