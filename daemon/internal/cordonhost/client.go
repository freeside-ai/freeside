package cordonhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// ClientConfig is a client's configuration.
type ClientConfig struct {
	// Socket is the launcher's Unix socket.
	Socket string
	// Build is the client's build; a launcher of any other is refused.
	Build string
	// Seeder is the daemon's own seeder. The client seeds each launch's
	// base with it, on the daemon's host, and streams the result.
	Seeder cordon.Seeder
	// Recorder keeps each conformance record the launcher's suite writes.
	Recorder cordon.ConformanceRecorder
	// ScratchRoot is a private directory the client owns: it seeds under
	// seeds/ and keeps a failed run's transcript and stderr under runs/.
	ScratchRoot string
	// ExportRoot is where the client receives each clean run's export, at
	// <ExportRoot>/<runID>/export. The stage driver accepts a cordoned
	// run's released export only at <CordonRunsRoot>/<runID>/export, so the
	// composition passes the driver's CordonRunsRoot here.
	ExportRoot string
}

// Hello is what a launcher says of itself before any request.
type Hello struct {
	Build               string
	ConfigurationDigest domain.Digest
	Account             Account
	// Facts is nil when the launcher could not read them, which refuses
	// every launch on it.
	Facts *cordon.HostFacts
}

// Client is the backend seam over a launcher: it has *cordon.Backend's
// operations and its suite's two passes, and each runs on the launcher
// over a connection of its own.
//
// Tier A: it sends a launch's token, once, after the hello, and receives
// the run's end report, which it checks before the gate reads it, and a
// clean run's export, which it receives as untrusted input (receipt.go).
type Client struct {
	cfg ClientConfig

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	detached bool
}

// NewClient checks cfg. It connects to nothing until an operation runs.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Socket == "" || cfg.Build == "" || cfg.Seeder == nil || cfg.Recorder == nil ||
		!filepath.IsAbs(cfg.ScratchRoot) || !filepath.IsAbs(cfg.ExportRoot) {
		return nil, errors.New("cordonhost: a client needs a socket, a build, a seeder, a recorder, and absolute scratch and export roots")
	}
	return &Client{cfg: cfg, conns: map[net.Conn]struct{}{}}, nil
}

// clientConn is one operation's connection, past its hello.
type clientConn struct {
	c     *Client
	conn  net.Conn
	hello Hello
	// writeMu keeps a launch's cancel frame out of the middle of its seed
	// stream and of a record's answer.
	writeMu sync.Mutex
}

func (cc *clientConn) write(f frame) error {
	cc.writeMu.Lock()
	defer cc.writeMu.Unlock()
	return writeFrame(cc.conn, f)
}

// dial connects and exchanges hellos: the client's build goes first, and
// nothing else crosses until the launcher's hello names the same build.
func (c *Client) dial(ctx context.Context) (*clientConn, error) {
	c.mu.Lock()
	detached := c.detached
	c.mu.Unlock()
	if detached {
		return nil, cordon.ErrDetached
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.cfg.Socket)
	if err != nil {
		return nil, fmt.Errorf("cordonhost: connect to the launcher: %w", err)
	}
	c.mu.Lock()
	if c.detached {
		c.mu.Unlock()
		_ = conn.Close()
		return nil, cordon.ErrDetached
	}
	c.conns[conn] = struct{}{}
	c.mu.Unlock()
	cc := &clientConn{c: c, conn: conn}
	if err := cc.exchangeHellos(); err != nil {
		cc.close()
		return nil, c.lost(err)
	}
	return cc, nil
}

func (cc *clientConn) exchangeHellos() error {
	if err := writeFrame(cc.conn, frame{Kind: kindHello, Hello: &helloBody{Build: cc.c.cfg.Build}}); err != nil {
		return err
	}
	f, err := readFrame(cc.conn, true)
	if err != nil {
		return err
	}
	if f.Kind == kindReply {
		if f.Reply == nil || f.Reply.Error == nil {
			return fmt.Errorf("%w: a reply where the launcher's hello belongs", ErrProtocol)
		}
		return decodeError(f.Reply.Error, "")
	}
	h := f.Hello
	if h == nil {
		return fmt.Errorf("%w: a hello with no body", ErrProtocol)
	}
	if h.Build != cc.c.cfg.Build {
		return fmt.Errorf("%w: the launcher is %q, the client %q", ErrBuildMismatch, boundText(h.Build), cc.c.cfg.Build)
	}
	if !contentaddr.Valid(string(h.ConfigurationDigest)) || h.Account == nil || h.Account.Name == "" || !shortText(h.Account.Name) {
		return fmt.Errorf("%w: a malformed launcher hello", ErrProtocol)
	}
	cc.hello = Hello{Build: h.Build, ConfigurationDigest: h.ConfigurationDigest, Account: *h.Account}
	if h.Facts != nil {
		cc.hello.Facts = &cordon.HostFacts{
			ScreenRecording: h.Facts.ScreenRecording, Accessibility: h.Facts.Accessibility, DesktopSession: h.Facts.DesktopSession,
		}
	}
	return nil
}

func (cc *clientConn) close() {
	cc.c.mu.Lock()
	delete(cc.c.conns, cc.conn)
	cc.c.mu.Unlock()
	_ = cc.conn.Close()
}

// lost is the error for a connection that failed: ErrDetached once the
// client detached, err otherwise.
func (c *Client) lost(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.detached {
		return fmt.Errorf("%w: %w", cordon.ErrDetached, err)
	}
	return err
}

// await reads the launcher's frames until its reply, keeping each record
// its suite sends on the way.
func (cc *clientConn) await(ctx context.Context) (*replyBody, error) {
	for {
		f, err := readFrame(cc.conn, false)
		if err != nil {
			return nil, err
		}
		switch f.Kind {
		case kindReply:
			return f.Reply, nil
		case kindRecord:
			answer := recordedBody{}
			if err := cc.keep(ctx, f.Record); err != nil {
				answer.Error = boundText(err.Error())
			}
			if err := cc.write(frame{Kind: kindRecorded, Recorded: &answer}); err != nil {
				return nil, err
			}
		case kindHello, kindLaunch, kindCancel, kindRecorded, kindStarted, kindRequestCancellation, kindRecover,
			kindReleaseFinished, kindExport, kindFull, kindProbeBeforeJob:
			return nil, fmt.Errorf("%w: the launcher sent a %s frame", ErrProtocol, f.Kind)
		}
	}
}

// keep validates a record the launcher's suite wrote and hands it to the
// client's recorder. It must be the cordon's, bound to the digest this
// connection's hello named.
func (cc *clientConn) keep(ctx context.Context, body *recordBody) error {
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: body.Backend, Outcome: body.Outcome, ConfigurationDigest: body.ConfigurationDigest,
		Capabilities: body.Capabilities, ProvedAt: body.ProvedAt,
	})
	if err != nil {
		return fmt.Errorf("the launcher's record: %w", err)
	}
	if record.Backend != domain.BackendCordonedHostProcess {
		return fmt.Errorf("the launcher's record is for %s, not the cordon", record.Backend)
	}
	if record.ConfigurationDigest != cc.hello.ConfigurationDigest {
		return fmt.Errorf("the launcher's record is bound to %s, not the %s its hello named",
			record.ConfigurationDigest, cc.hello.ConfigurationDigest)
	}
	return cc.c.cfg.Recorder.RecordBackendConformance(context.WithoutCancel(ctx), record)
}

// call runs one request that isn't a launch: the connection closes when
// ctx ends, which ends the operation on the launcher.
func (c *Client) call(ctx context.Context, req frame) (*replyBody, error) {
	return c.callThen(ctx, req, nil)
}

// callThen is call with then run on the reply before the connection
// closes, to read a stream that follows it.
func (c *Client) callThen(ctx context.Context, req frame, then func(io.Reader, *replyBody) error) (*replyBody, error) {
	cc, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer cc.close()
	stop := context.AfterFunc(ctx, func() { _ = cc.conn.Close() })
	defer stop()
	if err := cc.write(req); err != nil {
		return nil, c.ended(ctx, err)
	}
	reply, err := cc.await(ctx)
	if err != nil {
		return nil, c.ended(ctx, err)
	}
	if then != nil {
		if err := then(cc.conn, reply); err != nil {
			return nil, c.ended(ctx, err)
		}
	}
	return reply, nil
}

func (c *Client) ended(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return c.lost(err)
}

// expectEmpty checks a reply that carries only a possible refusal.
func expectEmpty(reply *replyBody, runID string) error {
	if reply.Error != nil {
		return decodeError(reply.Error, runID)
	}
	if reply.End != nil || reply.Started != nil || reply.Export != nil {
		return fmt.Errorf("%w: an answer where none belongs", ErrProtocol)
	}
	return nil
}

// Hello connects and returns the launcher's hello.
func (c *Client) Hello(ctx context.Context) (Hello, error) {
	cc, err := c.dial(ctx)
	if err != nil {
		return Hello{}, err
	}
	cc.close()
	return cc.hello, nil
}

// Launch runs req on the launcher. The client seeds req's base with its
// own seeder, sends the request with its token, and streams the seed. As
// with the backend, canceling ctx cancels the run for good; a connection
// that closes mid-launch returns an error wrapping cordon.ErrDetached and
// leaves the run running, for Recover to adopt.
func (c *Client) Launch(ctx context.Context, req cordon.LaunchRequest) (cordon.LaunchResult, error) {
	if !cordon.ValidRunID(req.RunID) {
		return cordon.LaunchResult{}, fmt.Errorf("cordonhost: run id %q is not one plain path component", req.RunID)
	}
	if req.Seeder != nil {
		return cordon.LaunchResult{}, errors.New("cordonhost: a launch on a host seeds from the client's seeder")
	}
	cc, err := c.dial(ctx)
	if err != nil {
		return cordon.LaunchResult{}, err
	}
	defer cc.close()
	seed, err := c.seed(ctx, req)
	if err != nil {
		return cordon.LaunchResult{}, err
	}
	defer func() { _ = os.RemoveAll(seed) }()
	// The launcher cancels the run only on this frame, never on a
	// connection that closes; a detached client sends nothing.
	stop := context.AfterFunc(ctx, func() {
		c.mu.Lock()
		detached := c.detached
		c.mu.Unlock()
		if !detached {
			_ = cc.write(frame{Kind: kindCancel})
		}
	})
	defer stop()
	err = cc.send(req, seed)
	if err != nil {
		// End the stream, so a launcher still reading the seed sees it
		// end short and refuses instead of waiting for more.
		cc.closeWrite()
	}
	// A launcher that refused before it read the whole seed has replied;
	// its reply says why.
	reply, readErr := cc.await(ctx)
	if readErr != nil {
		if err != nil {
			return cordon.LaunchResult{}, c.lost(err)
		}
		return cordon.LaunchResult{}, c.launchLost(readErr)
	}
	if reply.Error != nil {
		return cordon.LaunchResult{}, decodeError(reply.Error, req.RunID)
	}
	if err != nil {
		return cordon.LaunchResult{}, c.lost(err)
	}
	end, err := checkEnd(reply, false)
	if err != nil {
		return cordon.LaunchResult{}, err
	}
	res := cordon.LaunchResult{ExitCode: end.ExitCode, EndedBy: end.EndedBy}
	res.TranscriptPath, res.StderrPath, err = c.keepFailure(req.RunID, end)
	return res, err
}

// send writes the launch frame, with the token's one crossing, and the
// seed stream after it, together under the write lock.
func (cc *clientConn) send(req cordon.LaunchRequest, seed string) error {
	cc.writeMu.Lock()
	defer cc.writeMu.Unlock()
	token := req.Token.Reveal()
	defer clear(token)
	if err := writeFrame(cc.conn, frame{Kind: kindLaunch, Launch: &launchBody{
		RunID: req.RunID, Repository: req.Repository, BaseRef: req.BaseRef, BaseSHA: req.BaseSHA,
		Prompt: req.Prompt, Args: req.Args, Token: token, RequireProof: req.RequireProof,
	}}); err != nil {
		return err
	}
	return writeTree(cc.conn, seed)
}

// closeWrite ends what the client sends on the connection, or closes it
// where that can't be done alone.
func (cc *clientConn) closeWrite() {
	if w, ok := cc.conn.(interface{ CloseWrite() error }); ok && w.CloseWrite() == nil {
		return
	}
	_ = cc.conn.Close()
}

// launchLost is a launch's connection closing before its reply: the run
// keeps running under its supervisor, as a detached backend's does.
func (c *Client) launchLost(err error) error {
	return fmt.Errorf("%w: the launcher's connection closed mid-launch; the run keeps running: %w", cordon.ErrDetached, err)
}

// seed fills the client's seed directory for req with the daemon's own
// seeder, removing what an earlier attempt at the same run left.
func (c *Client) seed(ctx context.Context, req cordon.LaunchRequest) (string, error) {
	seeds := filepath.Join(c.cfg.ScratchRoot, "seeds")
	if err := os.MkdirAll(seeds, 0o700); err != nil {
		return "", fmt.Errorf("cordonhost: seed directory: %w", err)
	}
	dir := filepath.Join(seeds, req.RunID)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("cordonhost: remove an earlier seed: %w", err)
	}
	if err := c.cfg.Seeder.FetchBaseWorktree(ctx, req.Repository, req.BaseRef, req.BaseSHA, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("cordonhost: seed %s: %w", req.RunID, err)
	}
	return dir, nil
}

// checkEnd checks a launcher's report of how a run ended before anything
// reads it: a known end with a plausible exit status, a launch's never
// lost, an export only with a recovery's clean, exported end, and a
// failure's transcript only with a failed end, within its bound.
func checkEnd(reply *replyBody, recovery bool) (*endBody, error) {
	end := reply.End
	if end == nil || reply.Started != nil {
		return nil, fmt.Errorf("%w: a reply with no end", ErrProtocol)
	}
	if !slices.Contains(cordon.AllEndedBy, end.EndedBy) || end.ExitCode < -1 || end.ExitCode > 255 || !shortText(end.Reason) {
		return nil, fmt.Errorf("%w: an end of %q with exit status %d", ErrProtocol, end.EndedBy, end.ExitCode)
	}
	switch {
	case recovery && !slices.Contains(cordon.AllRecoveryOutcomes, end.Outcome):
		return nil, fmt.Errorf("%w: a recovery outcome of %q", ErrProtocol, end.Outcome)
	case !recovery && (end.Outcome != "" || end.EndedBy == cordon.EndedByLost):
		return nil, fmt.Errorf("%w: a launch that ended %q with outcome %q", ErrProtocol, end.EndedBy, end.Outcome)
	}
	exported := recovery && end.Outcome == cordon.RecoveryExported
	if exported != (reply.Export != nil) || (exported && (end.EndedBy != cordon.EndedByExited || end.ExitCode != 0)) {
		return nil, fmt.Errorf("%w: an export with an end of %q, exit status %d, outcome %q", ErrProtocol, end.EndedBy, end.ExitCode, end.Outcome)
	}
	failed := end.EndedBy == cordon.EndedByStalled || (end.EndedBy == cordon.EndedByExited && end.ExitCode != 0)
	if recovery && end.Outcome == cordon.RecoveryFailed && !failed {
		return nil, fmt.Errorf("%w: a failed recovery that ended %q with exit status %d", ErrProtocol, end.EndedBy, end.ExitCode)
	}
	carries := end.Transcript != nil || end.Stderr != nil
	if carries && (!failed || (recovery && end.Outcome != cordon.RecoveryFailed)) {
		return nil, fmt.Errorf("%w: a transcript with an end that did not fail", ErrProtocol)
	}
	for _, body := range []*failureBody{end.Transcript, end.Stderr} {
		if body != nil && len(body.Bytes) > ward.MaxFailureTranscriptBytes {
			return nil, fmt.Errorf("%w: a transcript over %d bytes", ErrProtocol, ward.MaxFailureTranscriptBytes)
		}
	}
	return end, nil
}

// keepFailure writes a failed end's transcript and stderr under
// runs/<runID> in the scratch root and returns their paths, as a local
// launch's result names its own. A body the launcher didn't send leaves
// its file absent, which the gate reads as unavailable. An end that did
// not fail names no paths.
func (c *Client) keepFailure(runID string, end *endBody) (string, string, error) {
	failed := end.EndedBy == cordon.EndedByStalled || (end.EndedBy == cordon.EndedByExited && end.ExitCode != 0)
	if !failed || end.Outcome == cordon.RecoveryCanceled || end.Outcome == cordon.RecoveryLost {
		return "", "", nil
	}
	dir := filepath.Join(c.cfg.ScratchRoot, "runs", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("cordonhost: keep %s's transcript: %w", runID, err)
	}
	paths := [2]string{filepath.Join(dir, "transcript"), filepath.Join(dir, "stderr")}
	for i, body := range []*failureBody{end.Transcript, end.Stderr} {
		if err := keepFile(paths[i], body); err != nil {
			return "", "", fmt.Errorf("cordonhost: keep %s's transcript: %w", runID, err)
		}
	}
	return paths[0], paths[1], nil
}

func keepFile(path string, body *failureBody) error {
	if body == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keep-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(body.Bytes)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// Export returns runID's export, received from the launcher into the
// client's export root as untrusted input (receipt.go). An export the
// client already received is returned from its receipt without a
// connection; a receipt cut short is removed and the launcher asked again.
// The launcher's refusals return as the backend's own errors.
func (c *Client) Export(ctx context.Context, req cordon.ExportRequest) (cordon.ExportResult, error) {
	if !cordon.ValidRunID(req.RunID) {
		return cordon.ExportResult{}, fmt.Errorf("cordonhost: run id %q is not one plain path component", req.RunID)
	}
	if res, ok, err := c.received(req.RunID); err != nil || ok {
		return res, err
	}
	_, res, err := c.fetch(ctx, frame{Kind: kindExport, Export: exportRequestOf(req)}, req.RunID, func(reply *replyBody) error {
		if reply.Error != nil {
			return decodeError(reply.Error, req.RunID)
		}
		if reply.End != nil || reply.Started != nil || reply.Export == nil {
			return fmt.Errorf("%w: an export reply with no export", ErrProtocol)
		}
		return nil
	})
	if err != nil {
		return cordon.ExportResult{}, err
	}
	return *res, nil
}

func exportRequestOf(req cordon.ExportRequest) *exportRequestBody {
	return &exportRequestBody{RunID: req.RunID, Transcript: req.Transcript, Sources: req.Sources}
}

// AuthenticateReleasedExport accepts dir only as runID's export in the
// client's export root, its members matching the receipt record the client
// wrote when it received them. It asks the launcher nothing.
func (c *Client) AuthenticateReleasedExport(ctx context.Context, runID, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !cordon.ValidRunID(runID) {
		return fmt.Errorf("%w: run id %q is not one plain path component", cordon.ErrExportUnauthenticated, runID)
	}
	l := c.receipt(runID)
	if dir != l.export {
		return fmt.Errorf("%w: %s is not %s's export directory", cordon.ErrExportUnauthenticated, dir, runID)
	}
	rec, err := readReceiptRecord(l.record, runID)
	if err != nil {
		return fmt.Errorf("%w: %w", cordon.ErrExportUnauthenticated, err)
	}
	return cordon.VerifyExportDigests(dir, rec.digests())
}

// Started reports whether runID's directory exists on the launcher.
func (c *Client) Started(ctx context.Context, runID string) (bool, error) {
	if !cordon.ValidRunID(runID) {
		return false, fmt.Errorf("cordonhost: run id %q is not one plain path component", runID)
	}
	reply, err := c.call(ctx, frame{Kind: kindStarted, RunID: runID})
	if err != nil {
		return false, err
	}
	if reply.Error != nil {
		return false, decodeError(reply.Error, runID)
	}
	if reply.Started == nil || reply.End != nil || reply.Export != nil {
		return false, fmt.Errorf("%w: a Started reply with no answer", ErrProtocol)
	}
	return *reply.Started, nil
}

// RequestCancellation makes runID end canceled and never export.
func (c *Client) RequestCancellation(ctx context.Context, runID string) error {
	if !cordon.ValidRunID(runID) {
		return fmt.Errorf("cordonhost: run id %q is not one plain path component", runID)
	}
	reply, err := c.call(ctx, frame{Kind: kindRequestCancellation, RunID: runID})
	if err != nil {
		return err
	}
	return expectEmpty(reply, runID)
}

// Recover finds how runID ended on the launcher, waiting while it runs.
// Canceling ctx stops the wait and leaves the run running. A run that
// exited cleanly returns its export, received as Export receives it, and
// an export the client already received returns from its receipt without
// a connection.
func (c *Client) Recover(ctx context.Context, req cordon.RecoverRequest) (cordon.RecoveryResult, error) {
	if !cordon.ValidRunID(req.RunID) {
		return cordon.RecoveryResult{}, fmt.Errorf("cordonhost: run id %q is not one plain path component", req.RunID)
	}
	if req.Export.RunID != req.RunID {
		return cordon.RecoveryResult{}, fmt.Errorf("cordonhost: recovering %s with %s's export", req.RunID, req.Export.RunID)
	}
	kept, ok, err := c.received(req.RunID)
	if err != nil {
		return cordon.RecoveryResult{}, err
	}
	if ok {
		return cordon.RecoveryResult{Outcome: cordon.RecoveryExported, EndedBy: cordon.EndedByExited, Export: kept}, nil
	}
	var end *endBody
	_, exp, err := c.fetch(ctx, frame{Kind: kindRecover, Recover: exportRequestOf(req.Export)}, req.RunID, func(reply *replyBody) error {
		if reply.Error != nil {
			return decodeError(reply.Error, req.RunID)
		}
		var err error
		end, err = checkEnd(reply, true)
		return err
	})
	if err != nil {
		return cordon.RecoveryResult{}, err
	}
	res := cordon.RecoveryResult{Outcome: end.Outcome, EndedBy: end.EndedBy, ExitCode: end.ExitCode, Reason: end.Reason}
	if exp != nil {
		res.Export = *exp
	}
	res.TranscriptPath, res.StderrPath, err = c.keepFailure(req.RunID, end)
	return res, err
}

// ReleaseFinished releases runIDs on the launcher, which removes each
// finished run's directory, and then the transcripts and exports the
// client kept for them.
func (c *Client) ReleaseFinished(ctx context.Context, runIDs []string) error {
	if len(runIDs) == 0 {
		return nil
	}
	for _, id := range runIDs {
		if !cordon.ValidRunID(id) {
			return fmt.Errorf("cordonhost: run id %q is not one plain path component", id)
		}
	}
	reply, err := c.call(ctx, frame{Kind: kindReleaseFinished, RunIDs: runIDs})
	if err != nil {
		return err
	}
	if err := expectEmpty(reply, ""); err != nil {
		return err
	}
	var errs []error
	for _, id := range runIDs {
		errs = append(errs, os.RemoveAll(filepath.Join(c.cfg.ScratchRoot, "runs", id)), c.receipt(id).release())
	}
	return errors.Join(errs...)
}

// Full runs the launcher's whole conformance suite; its records reach the
// client's recorder on the way.
func (c *Client) Full(ctx context.Context) error {
	reply, err := c.call(ctx, frame{Kind: kindFull})
	if err != nil {
		return err
	}
	return expectEmpty(reply, "")
}

// ProbeBeforeJob runs the launcher's pre-job probe.
func (c *Client) ProbeBeforeJob(ctx context.Context) error {
	reply, err := c.call(ctx, frame{Kind: kindProbeBeforeJob})
	if err != nil {
		return err
	}
	return expectEmpty(reply, "")
}

// SendMessage refuses every message with ErrMessageUnsupported, without
// dialing: the launcher's backend streams its runs' input, but no frame
// carries a message to it yet (#3702).
func (c *Client) SendMessage(context.Context, string, int64, string, string) error {
	return ErrMessageUnsupported
}

// Detach closes every connection and refuses every later operation with
// cordon.ErrDetached, signaling nothing: the launches keep running on the
// launcher, for a later client to recover.
func (c *Client) Detach() {
	c.mu.Lock()
	c.detached = true
	conns := make([]net.Conn, 0, len(c.conns))
	for conn := range c.conns {
		conns = append(conns, conn)
	}
	c.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}
