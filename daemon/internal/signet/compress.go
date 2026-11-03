package signet

import (
	"compress/gzip"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// compressJSON gzips application/json responses for clients whose
// Accept-Encoding allows it (#2523). A full sync downloads the whole
// bootstrap snapshot, and JSON of that shape shrinks several times under
// gzip. The decision is made from the response's Content-Type when the
// status is written, so every writeJSON route is covered and the
// octet-stream attachment route (already-compressed images) is not.
//
// The wrapper does not expose http.Flusher or Unwrap: a flush through the
// underlying writer would bypass the gzip buffer, so a future streaming route
// must teach this writer to flush first.
func compressJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &compressWriter{ResponseWriter: w, request: r}
		defer cw.close()
		next.ServeHTTP(cw, r)
	})
}

type compressWriter struct {
	http.ResponseWriter
	request *http.Request
	decided bool
	gzip    *gzip.Writer
}

func (w *compressWriter) WriteHeader(status int) {
	if !w.decided {
		w.decided = true
		w.decide(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *compressWriter) Write(p []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.gzip != nil {
		return w.gzip.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// decide runs before the status is passed on, because headers set after
// WriteHeader are dropped. Every JSON response varies on Accept-Encoding,
// compressed or not, so a cache never serves one client's encoding to
// another.
func (w *compressWriter) decide(status int) {
	mediaType, _, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return
	}
	w.Header().Add("Vary", "Accept-Encoding")
	if status == http.StatusNoContent || status == http.StatusNotModified ||
		w.request.Method == http.MethodHead ||
		!acceptsGzip(w.request.Header.Values("Accept-Encoding")) {
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Del("Content-Length")
	w.gzip = gzip.NewWriter(w.ResponseWriter)
}

// close ends the gzip stream when one was opened. Its error is ignored, as
// writeJSON ignores its encode error: once the status is written, the
// connection is the failure surface.
func (w *compressWriter) close() {
	if w.gzip != nil {
		_ = w.gzip.Close()
	}
}

// acceptsGzip reports whether Accept-Encoding header lines allow gzip
// (RFC 9110 §12.5.3). A gzip entry decides; without one, a * entry decides.
// Either allows unless its q is 0, and a q that does not parse refuses. No
// header at all means the client did not ask, so the response stays plain.
func acceptsGzip(values []string) bool {
	starSeen, starAllowed := false, false
	for _, value := range values {
		for _, entry := range strings.Split(value, ",") {
			coding, params, _ := strings.Cut(entry, ";")
			coding = strings.TrimSpace(coding)
			switch {
			case strings.EqualFold(coding, "gzip"):
				return qualityAllows(params)
			case coding == "*" && !starSeen:
				starSeen, starAllowed = true, qualityAllows(params)
			}
		}
	}
	return starAllowed
}

func qualityAllows(params string) bool {
	for _, param := range strings.Split(params, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(param), "=")
		if strings.EqualFold(strings.TrimSpace(name), "q") {
			q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			return err == nil && q > 0
		}
	}
	return true
}
