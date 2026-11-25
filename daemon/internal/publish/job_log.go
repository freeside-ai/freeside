package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Job log reads (issue #3598): the tail of a failed required-check job's
// log, which a check_failure cycle hands its remediator. GitHub answers the
// authenticated request with a redirect to a signed URL on another host.
// The token never follows it: the redirect is read by hand, its target must
// be an https URL on a named host, and it is fetched once with no credential
// and no redirect of its own. The log is the candidate's output, so it is
// untrusted data; the caller screens it before storing any of it.
const (
	// MaxJobLogTailBytes bounds the tail ReadJobLogTail keeps. The caller
	// screens the whole tail before cutting the excerpt, so a credential
	// that straddles the excerpt's start is still replaced whole.
	MaxJobLogTailBytes = 4 << 20
	// maxJobLogBytes bounds how much of one log is read at all.
	maxJobLogBytes = 256 << 20
)

// ErrJobLogUnavailable reports that GitHub keeps no log for the job: it
// answered 404 or 410, as it does once a log expires or for a job that never
// started. Retrying does not bring the log back.
var ErrJobLogUnavailable = errors.New("job log unavailable")

// ErrJobLogTooLong reports a log that ignored the tail request and ran past
// maxJobLogBytes. The log is the candidate's output, so a retry would read
// the same log again; the caller records it as unreadable instead.
var ErrJobLogTooLong = errors.New("job log too long")

// ReadJobLogTail reads the last MaxJobLogTailBytes of a GitHub Actions job's
// log in repo. It asks the storage host for only that tail; a host that sends
// the whole log is read to its end, up to maxJobLogBytes. A log GitHub no
// longer keeps is ErrJobLogUnavailable, a log past maxJobLogBytes is
// ErrJobLogTooLong, and an empty log is an empty tail. Any other failure is
// an error the caller retries on a later pass.
func (r *Reconciler) ReadJobLogTail(ctx context.Context, repo string, jobID int64) ([]byte, error) {
	ref, err := parseRepo(repo)
	if err != nil {
		return nil, err
	}
	if jobID < 1 {
		return nil, fmt.Errorf("job log: job id %d", jobID)
	}
	path := "/repos/" + ref.path() + "/actions/jobs/" + strconv.FormatInt(jobID, 10) + "/logs"
	resp, err := r.forge.do(ctx, http.MethodGet, ref, path, "", nil)
	if err != nil {
		return nil, fmt.Errorf("job log: %w", err)
	}
	location := resp.Header.Get("Location")
	drainAndClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect:
	case http.StatusNotFound, http.StatusGone:
		return nil, ErrJobLogUnavailable
	default:
		return nil, fmt.Errorf("job log: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}
	target, err := r.jobLogTarget(location)
	if err != nil {
		return nil, err
	}
	// A fresh request: no header of the authenticated one is carried over.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("job log: build download request: %w", err)
	}
	req.Header.Set("Range", "bytes=-"+strconv.Itoa(MaxJobLogTailBytes))
	download, err := r.forge.client.Do(req)
	if err != nil {
		// The signed URL is a bearer credential for the log; an error
		// that quoted it would leak it into logs.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("job log: download from %s: %w", target.Host, err)
	}
	defer drainAndClose(download.Body)
	switch download.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusRequestedRangeNotSatisfiable:
		// A suffix range of an empty log asks for bytes that don't exist.
		return []byte{}, nil
	case http.StatusNotFound, http.StatusGone:
		return nil, ErrJobLogUnavailable
	default:
		return nil, fmt.Errorf("job log: download from %s answered %d", target.Host, download.StatusCode)
	}
	return readTail(download.Body, MaxJobLogTailBytes, maxJobLogBytes)
}

// jobLogTarget parses the redirect's Location and admits only an absolute
// https URL with no user info whose host is a name, not an address or a
// local name. The Location is untrusted: following it to an internal
// address would turn the read into a request on the daemon's network.
func (r *Reconciler) jobLogTarget(location string) (*url.URL, error) {
	target, err := url.Parse(location)
	if err != nil || location == "" {
		return nil, errors.New("job log: redirect carries no usable location")
	}
	if target.Scheme != "https" || target.User != nil || target.Hostname() == "" {
		return nil, fmt.Errorf("job log: redirect to a %q URL on host %q is refused", target.Scheme, target.Hostname())
	}
	allowed := publicLogHost
	if r.logHostAllowed != nil {
		allowed = r.logHostAllowed
	}
	if !allowed(target.Hostname()) {
		return nil, fmt.Errorf("job log: redirect to host %q is refused", target.Hostname())
	}
	return target, nil
}

// publicLogHost reports whether host is a DNS name outside the local
// namespace: not an IP literal, not localhost, and not a single label.
func publicLogHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	return strings.Contains(host, ".")
}

// readTail reads body to its end, at most limit bytes in all, and returns
// its last keep bytes.
func readTail(body io.Reader, keep, limit int) ([]byte, error) {
	tail := make([]byte, 0, keep)
	buf := make([]byte, 64<<10)
	total := 0
	for {
		n, err := body.Read(buf)
		total += n
		if total > limit {
			return nil, fmt.Errorf("job log: longer than %d bytes: %w", limit, ErrJobLogTooLong)
		}
		tail = append(tail, buf[:n]...)
		if len(tail) > keep {
			tail = append(tail[:0], tail[len(tail)-keep:]...)
		}
		if errors.Is(err, io.EOF) {
			return tail, nil
		}
		if err != nil {
			return nil, fmt.Errorf("job log: read: %w", err)
		}
	}
}
