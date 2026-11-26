package cordonhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// recordTimeout bounds the wait for a client to answer a record. The
// launcher's suite writes a record under its proof guard, so a client that
// stopped reading must not hold that guard for good.
const recordTimeout = 2 * time.Minute

// served is what a Server serves: a *cordon.Backend and the *cordon.Suite
// attached to it.
type served interface {
	Launch(context.Context, cordon.LaunchRequest) (cordon.LaunchResult, error)
	Started(context.Context, string) (bool, error)
	RequestCancellation(context.Context, string) error
	Recover(context.Context, cordon.RecoverRequest) (cordon.RecoveryResult, error)
	ReleaseFinished(context.Context, []string) error
	Detach()
	HostFacts() (cordon.HostFacts, error)
	ConfigurationDigest() domain.Digest
	Full(context.Context) error
	ProbeBeforeJob(context.Context) error
}

type backendAndSuite struct {
	*cordon.Backend
	*cordon.Suite
}

// ServerConfig is a launcher's configuration.
type ServerConfig struct {
	// Build is the launcher's build; a client of any other is refused.
	Build string
	// Backend has no forge seeder: every launch brings its seed. Suite is
	// attached to it and records through ConnectionRecorder.
	Backend *cordon.Backend
	Suite   *cordon.Suite
	Account Account
	// SeedRoot is where seeds are staged: a directory only the launcher
	// writes, under a protected root, on the runs root's file system so a
	// staged seed renames into its launch.
	SeedRoot string
	Logger   *slog.Logger
}

// Server serves one backend and its suite to clients on the same host.
//
// Tier A: it receives a launch's token and hands it to the backend alone,
// and reports a run's end only once the backend proved it. A client that
// disconnects leaves its launch running; only a cancel frame cancels it.
type Server struct {
	build    string
	backend  served
	account  Account
	seedRoot string
	logger   *slog.Logger
	awake    *awakeHolder
	// onFrame, when set, sees each frame's kind as the server reads it.
	// Only this package's tests set it.
	onFrame func(kind)

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	ln     net.Listener
	closed bool
	wg     sync.WaitGroup
}

// NewServer checks cfg and prepares its seed root, removing what an
// earlier launcher staged and never used.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Build == "" || cfg.Backend == nil || cfg.Suite == nil || cfg.Logger == nil {
		return nil, errors.New("cordonhost: a server needs a build, a backend, its suite, and a logger")
	}
	return newServer(cfg.Build, backendAndSuite{cfg.Backend, cfg.Suite}, cfg.Account, cfg.SeedRoot, cfg.Logger)
}

func newServer(build string, backend served, account Account, seedRoot string, logger *slog.Logger) (*Server, error) {
	if account.Name == "" || !filepath.IsAbs(seedRoot) {
		return nil, errors.New("cordonhost: a server needs its account and an absolute seed root")
	}
	if err := os.MkdirAll(seedRoot, 0o700); err != nil {
		return nil, fmt.Errorf("cordonhost: seed root: %w", err)
	}
	entries, err := os.ReadDir(seedRoot)
	if err != nil {
		return nil, fmt.Errorf("cordonhost: seed root: %w", err)
	}
	for _, e := range entries {
		// Only what this server names: a run id.
		if cordon.ValidRunID(e.Name()) {
			if err := os.RemoveAll(filepath.Join(seedRoot, e.Name())); err != nil {
				return nil, fmt.Errorf("cordonhost: remove a stale seed: %w", err)
			}
		}
	}
	return &Server{
		build: build, backend: backend, account: account, seedRoot: seedRoot, logger: logger,
		awake: &awakeHolder{logger: logger}, conns: map[net.Conn]struct{}{},
	}, nil
}

// Serve accepts connections on ln until Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.ln = ln
	s.mu.Unlock()
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		if !s.track(conn) {
			_ = conn.Close()
			return nil
		}
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			s.serveConn(conn)
		}()
	}
}

// Close stops serving: the backend detaches, so every waiting launch and
// recovery returns at once and signals nothing, and every connection
// closes. The runs keep running under their supervisors, for the next
// launcher to recover.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	ln := s.ln
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	s.backend.Detach()
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
}

func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	// Added under the lock, so Close, which sets closed under it, waits
	// for every connection track admitted.
	s.wg.Add(1)
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.Close()
}

// connection is one client's operation.
type connection struct {
	conn    net.Conn
	writeMu sync.Mutex
	// acks carries the client's answers to records; gone closes when the
	// client is gone.
	acks     chan recordedBody
	gone     chan struct{}
	goneOnce sync.Once
}

func (c *connection) write(f frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.conn, f)
}

func (c *connection) leave() { c.goneOnce.Do(func() { close(c.gone) }) }

type connKey struct{}

// ConnectionRecorder is the recorder a launcher's suite records through:
// each record crosses to the client whose operation wrote it, over that
// operation's connection, and fails when the client refuses it, is gone,
// or doesn't answer within recordTimeout. A record that fails leaves the
// launcher's class undeclared, as a local recorder's failure does.
func ConnectionRecorder() cordon.ConformanceRecorder { return connectionRecorder{} }

type connectionRecorder struct{}

func (connectionRecorder) RecordBackendConformance(ctx context.Context, record domain.BackendConformance) error {
	c, ok := ctx.Value(connKey{}).(*connection)
	if !ok {
		return errors.New("cordonhost: no client to record to")
	}
	body := &recordBody{
		Backend: record.Backend, Outcome: record.Outcome, ConfigurationDigest: record.ConfigurationDigest,
		Capabilities: record.Capabilities, ProvedAt: record.ProvedAt,
	}
	if err := c.write(frame{Kind: kindRecord, Record: body}); err != nil {
		return fmt.Errorf("cordonhost: send the record: %w", err)
	}
	timer := time.NewTimer(recordTimeout)
	defer timer.Stop()
	select {
	case ack := <-c.acks:
		if ack.Error != "" {
			return fmt.Errorf("cordonhost: the client refused the record: %s", boundText(ack.Error))
		}
		return nil
	case <-c.gone:
		return errors.New("cordonhost: the client left before it recorded")
	case <-timer.C:
		return errors.New("cordonhost: the client did not answer the record in time")
	}
}

func (s *Server) serveConn(conn net.Conn) {
	c := &connection{conn: conn, acks: make(chan recordedBody, 1), gone: make(chan struct{})}
	hello, err := readFrame(conn, true)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			s.logger.Warn("cordon host: a client's hello", "error", err)
		}
		return
	}
	s.saw(hello.Kind)
	if hello.Kind != kindHello || hello.Hello == nil {
		s.logger.Warn("cordon host: a client sent no hello")
		return
	}
	if hello.Hello.Build != s.build {
		s.logger.Warn("cordon host: refused a client of another build", "client", boundText(hello.Hello.Build), "launcher", s.build)
		_ = c.write(frame{Kind: kindReply, Reply: &replyBody{Error: encodeError(
			fmt.Errorf("%w: the launcher is %s", ErrBuildMismatch, s.build))}})
		return
	}
	if err := c.write(frame{Kind: kindHello, Hello: s.hello()}); err != nil {
		return
	}
	req, err := readFrame(conn, false)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			s.logger.Warn("cordon host: a client's request", "error", err)
		}
		return
	}
	s.saw(req.Kind)
	reply := s.handle(c, req)
	if err := c.write(frame{Kind: kindReply, Reply: reply}); err != nil {
		s.logger.Info("cordon host: the client left before the reply", "kind", req.Kind, "error", err)
	}
}

func (s *Server) saw(k kind) {
	if s.onFrame != nil {
		s.onFrame(k)
	}
}

func (s *Server) hello() *helloBody {
	h := &helloBody{Build: s.build, ConfigurationDigest: s.backend.ConfigurationDigest(), Account: &s.account}
	facts, err := s.backend.HostFacts()
	if err != nil {
		s.logger.Warn("cordon host: read the launcher's approvals", "error", err)
		return h
	}
	h.Facts = &factsBody{ScreenRecording: facts.ScreenRecording, Accessibility: facts.Accessibility, DesktopSession: facts.DesktopSession}
	return h
}

// handle runs one request. Every operation but a launch ends when its
// client leaves; a launch keeps running and only a cancel frame cancels it.
func (s *Server) handle(c *connection, req frame) *replyBody {
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), connKey{}, c))
	defer cancel()
	launch := req.Kind == kindLaunch
	var staged string
	var token ward.SetupToken
	if launch {
		var refusal *replyBody
		staged, token, refusal = s.receiveLaunch(c.conn, req.Launch)
		if refusal != nil {
			return refusal
		}
		defer func() {
			if err := os.RemoveAll(staged); err != nil {
				s.logger.Warn("cordon host: remove a staged seed", "run", req.Launch.RunID, "error", err)
			}
		}()
	}
	go s.read(c, cancel, launch)
	switch req.Kind {
	case kindLaunch:
		return s.launch(ctx, req.Launch, staged, token)
	case kindRecover:
		return s.recover(ctx, req.Recover)
	case kindStarted:
		started, err := s.backend.Started(ctx, req.RunID)
		if err != nil {
			return refuse(err)
		}
		return &replyBody{Started: &started}
	case kindRequestCancellation:
		return refuse(s.backend.RequestCancellation(ctx, req.RunID))
	case kindReleaseFinished:
		return refuse(s.backend.ReleaseFinished(ctx, req.RunIDs))
	case kindExport, kindAuthenticate:
		return refuse(ErrExportUnsupported)
	case kindFull:
		return refuse(s.backend.Full(ctx))
	case kindProbeBeforeJob:
		return refuse(s.backend.ProbeBeforeJob(ctx))
	case kindHello, kindCancel, kindRecord, kindRecorded, kindReply:
	}
	return refuse(fmt.Errorf("%w: a %s frame is not a request", ErrProtocol, req.Kind))
}

// read takes the client's frames while its operation runs: a launch's
// cancel, and the answers to records. A client that leaves, or breaks the
// protocol, ends every operation but a launch, which keeps running.
func (s *Server) read(c *connection, cancel context.CancelFunc, launch bool) {
	defer c.leave()
	for {
		f, err := readFrame(c.conn, false)
		if err != nil {
			if !launch {
				cancel()
			}
			return
		}
		s.saw(f.Kind)
		switch {
		case f.Kind == kindCancel && launch:
			cancel()
		case f.Kind == kindRecorded:
			select {
			case c.acks <- *f.Recorded:
			default:
				s.logger.Warn("cordon host: a client answered a record nobody sent")
				if !launch {
					cancel()
				}
				return
			}
		default:
			s.logger.Warn("cordon host: a client sent a stray frame", "kind", f.Kind)
			if !launch {
				cancel()
			}
			return
		}
	}
}

// receiveLaunch takes a launch's token and stages its seed, which follows
// the frame. The token's bytes in the frame are cleared once wrapped. A
// refusal before the seed is read leaves it unread; the connection closes
// after the reply.
func (s *Server) receiveLaunch(r io.Reader, req *launchBody) (string, ward.SetupToken, *replyBody) {
	token, err := ward.NewSetupToken(req.Token)
	clear(req.Token)
	if err != nil {
		return "", ward.SetupToken{}, refuse(fmt.Errorf("cordonhost: the launch's token: %w", err))
	}
	if !cordon.ValidRunID(req.RunID) {
		return "", ward.SetupToken{}, refuse(fmt.Errorf("cordonhost: run id %q is not one plain path component", req.RunID))
	}
	staged := filepath.Join(s.seedRoot, req.RunID)
	if err := os.Mkdir(staged, 0o700); err != nil {
		// Another connection stages this run, or a stale seed survived:
		// either way it is not this connection's to remove.
		return "", ward.SetupToken{}, refuse(fmt.Errorf("cordonhost: stage %s's seed: %w", req.RunID, err))
	}
	if err := readSeed(r, filepath.Join(staged, "tree")); err != nil {
		if rmErr := os.RemoveAll(staged); rmErr != nil {
			s.logger.Warn("cordon host: remove a staged seed", "run", req.RunID, "error", rmErr)
		}
		return "", ward.SetupToken{}, refuse(fmt.Errorf("cordonhost: receive %s's seed: %w", req.RunID, err))
	}
	return staged, token, nil
}

func (s *Server) launch(ctx context.Context, req *launchBody, staged string, token ward.SetupToken) *replyBody {
	release := s.awake.hold()
	defer release()
	s.logger.Info("cordon host: launch", "run", req.RunID)
	res, err := s.backend.Launch(ctx, cordon.LaunchRequest{
		RunID: req.RunID, Repository: req.Repository, BaseRef: req.BaseRef, BaseSHA: req.BaseSHA,
		Prompt: req.Prompt, Args: req.Args, Token: token, RequireProof: req.RequireProof,
		Seeder: &stagedSeed{
			tree: filepath.Join(staged, "tree"), repository: req.Repository, baseRef: req.BaseRef, baseSHA: req.BaseSHA,
		},
	})
	if err != nil {
		s.logger.Warn("cordon host: launch refused or blocked", "run", req.RunID, "error", err)
		return refuse(err)
	}
	s.logger.Info("cordon host: launch ended", "run", req.RunID, "ended_by", res.EndedBy, "exit_code", res.ExitCode)
	return &replyBody{End: failedEnd(endBody{EndedBy: res.EndedBy, ExitCode: res.ExitCode}, res.TranscriptPath, res.StderrPath)}
}

func (s *Server) recover(ctx context.Context, req *recoverBody) *replyBody {
	release := s.awake.hold()
	defer release()
	res, err := s.backend.Recover(ctx, cordon.RecoverRequest{RunID: req.RunID, Export: cordon.ExportRequest{
		RunID: req.RunID, Transcript: req.Transcript, Sources: req.Sources,
	}})
	// A run canceled between the end record's read and the export ended
	// canceled; its refusal crosses as itself, not as an export's.
	if !errors.Is(err, cordon.ErrCanceled) && exportRefusal(err) {
		// Only a clean end's export fails these ways, and no export
		// returns from a launcher yet.
		return refuse(fmt.Errorf("%w: run %s's export on the launcher: %w", ErrExportUnsupported, req.RunID, err))
	}
	if err != nil {
		return refuse(err)
	}
	s.logger.Info("cordon host: recovered", "run", req.RunID, "outcome", res.Outcome)
	if res.Outcome == cordon.RecoveryExported {
		return refuse(fmt.Errorf("%w: run %s exited cleanly and is exported on the launcher", ErrExportUnsupported, req.RunID))
	}
	end := endBody{Outcome: res.Outcome, EndedBy: res.EndedBy, ExitCode: res.ExitCode, Reason: boundText(res.Reason)}
	return &replyBody{End: failedEnd(end, res.TranscriptPath, res.StderrPath)}
}

// exportRefusal reports whether err is a refusal of the export step a
// recovery of a clean end runs.
func exportRefusal(err error) bool {
	var scan *cordon.ScanRefusalError
	return errors.As(err, &scan) || errors.Is(err, cordon.ErrWorktreeReplaced) ||
		errors.Is(err, cordon.ErrNotExportable) || errors.Is(err, export.ErrLinkedFile)
}

// failedEnd adds a failed end's transcript and stderr, the two files the
// daemon's gate reads only for a nonzero exit or a stall.
func failedEnd(end endBody, transcriptPath, stderrPath string) *endBody {
	failed := end.EndedBy == cordon.EndedByStalled || (end.EndedBy == cordon.EndedByExited && end.ExitCode != 0)
	if failed && end.Outcome != cordon.RecoveryCanceled && end.Outcome != cordon.RecoveryLost {
		end.Transcript, end.Stderr = readFailureBody(transcriptPath), readFailureBody(stderrPath)
	}
	return &end
}

// readFailureBody reads a regular file of at most
// ward.MaxFailureTranscriptBytes; one that is absent, larger, or not a
// regular file sends nothing, which the daemon reads as an unavailable
// transcript, as it reads a local one.
func readFailureBody(path string) *failureBody {
	if path == "" {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // G304: a path the backend's own result names, under its runs root
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > ward.MaxFailureTranscriptBytes {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(f, ward.MaxFailureTranscriptBytes+1))
	if err != nil || len(body) > ward.MaxFailureTranscriptBytes {
		return nil
	}
	return &failureBody{Bytes: body}
}

func refuse(err error) *replyBody {
	if err == nil {
		return &replyBody{}
	}
	return &replyBody{Error: encodeError(err)}
}

// stagedSeed seeds one launch from the seed its connection staged: it
// renames the staged tree into place, once, and only for that launch's
// base.
type stagedSeed struct {
	tree                         string
	repository, baseRef, baseSHA string
}

func (s *stagedSeed) FetchBaseWorktree(_ context.Context, repository, baseRef, baseSHA, dir string) error {
	if repository != s.repository || baseRef != s.baseRef || baseSHA != s.baseSHA {
		return errors.New("cordonhost: the staged seed is another base's")
	}
	return os.Rename(s.tree, dir)
}

// NoForgeSeeder is a launcher backend's own seeder: a host holds no forge
// credential, so it refuses, and every launch seeds from its connection.
type NoForgeSeeder struct{}

func (NoForgeSeeder) FetchBaseWorktree(context.Context, string, string, string, string) error {
	return errors.New("cordonhost: the launcher holds no forge credential; a launch brings its seed")
}
