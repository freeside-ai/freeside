// Package advisory owns inference-produced claims and audit samples that may
// be shown to humans but can never participate in policy evaluation.
package advisory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

const fileVersion = "freeside.advisory/v1"

var ErrInvalidEntry = errors.New("invalid advisory entry")

// Entry is an untrusted, producer-labeled advisory claim or audit sample.
type Entry struct {
	ID          string    `json:"id"`
	RootLineage string    `json:"root_lineage"`
	Site        string    `json:"site"`
	Producer    string    `json:"producer"`
	Kind        string    `json:"kind"`
	InputDigest string    `json:"input_digest"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
	RetainUntil time.Time `json:"retain_until"`
}

func (e Entry) validate(maxBodyBytes int) error {
	if e.ID == "" || e.RootLineage == "" || e.Site == "" || e.Producer == "" || e.Kind == "" ||
		!contentaddr.Valid(e.InputDigest) || e.CreatedAt.IsZero() || !e.RetainUntil.After(e.CreatedAt) ||
		!utf8.ValidString(e.Body) || len(e.Body) > maxBodyBytes {
		return ErrInvalidEntry
	}
	return nil
}

type diskState struct {
	Version string  `json:"version"`
	Entries []Entry `json:"entries"`
}

// Store is an append-only logical store with bounded physical retention.
// It deliberately exposes no policy query or aggregation surface.
type Store struct {
	mu           sync.Mutex
	path         string
	maxEntries   int
	maxBodyBytes int
	entries      []Entry
	now          func() time.Time
	disabled     error
}

// Option configures the advisory store without widening its read/write API.
type Option func(*Store)

// WithClock installs the daemon clock used for retention decisions.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// Open loads or creates an owner-only advisory store.
func Open(path string, maxEntries, maxBodyBytes int, opts ...Option) (*Store, error) {
	if path == "" || maxEntries < 1 || maxBodyBytes < 1 {
		return nil, errors.New("invalid advisory store configuration")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create advisory directory: %w", err)
	}
	s := &Store{path: path, maxEntries: maxEntries, maxBodyBytes: maxBodyBytes, now: time.Now}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	body, err := os.ReadFile(path) //nolint:gosec // daemon-owned configured state path
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read advisory store: %w", err)
	}
	var state diskState
	if err := json.Unmarshal(body, &state); err != nil || state.Version != fileVersion {
		return nil, errors.New("decode advisory store")
	}
	if len(state.Entries) > maxEntries {
		return nil, errors.New("advisory store exceeds configured retention")
	}
	seen := make(map[string]bool, len(state.Entries))
	now := s.now().UTC()
	for _, entry := range state.Entries {
		if err := entry.validate(maxBodyBytes); err != nil {
			return nil, fmt.Errorf("decode advisory store entry: %w", err)
		}
		if seen[entry.ID] {
			return nil, errors.New("duplicate advisory entry identity")
		}
		seen[entry.ID] = true
		if !now.Before(entry.RetainUntil) {
			continue
		}
		s.entries = append(s.entries, entry)
	}
	if len(s.entries) != len(state.Entries) {
		if err := s.persist(s.entries); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Append persists an entry, retaining only the newest configured count.
func (s *Store) Append(_ context.Context, entry Entry) error {
	if err := entry.validate(s.maxBodyBytes); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled != nil {
		return s.disabled
	}
	for _, existing := range s.entries {
		if existing.ID == entry.ID {
			if existing == entry {
				return nil
			}
			return errors.New("advisory entry identity collision")
		}
	}
	entries := make([]Entry, 0, len(s.entries)+1)
	now := s.now().UTC()
	for _, retained := range s.entries {
		if now.Before(retained.RetainUntil) {
			entries = append(entries, retained)
		}
	}
	entries = append(entries, entry)
	if len(entries) > s.maxEntries {
		entries = entries[len(entries)-s.maxEntries:]
	}
	if err := s.persist(entries); err != nil {
		return err
	}
	s.entries = entries
	return nil
}

func (s *Store) persist(entries []Entry) error {
	body, err := json.Marshal(diskState{Version: fileVersion, Entries: entries})
	if err != nil {
		return fmt.Errorf("encode advisory store: %w", err)
	}
	if err := atomicfile.WriteFile(s.path, body, 0o600); err != nil {
		s.disabled = fmt.Errorf("persist advisory store: %w", err)
		return s.disabled
	}
	return nil
}

// Prune physically removes entries whose declared retention has elapsed.
func (s *Store) Prune(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled != nil {
		return s.disabled
	}
	now := s.now().UTC()
	retained := make([]Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		if now.Before(entry.RetainUntil) {
			retained = append(retained, entry)
		}
	}
	if len(retained) == len(s.entries) {
		return nil
	}
	if err := s.persist(retained); err != nil {
		return err
	}
	s.entries = retained
	return nil
}

// List returns retained advisory entries for a human-facing reader and
// reports any physical-retention failure.
func (s *Store) List(ctx context.Context) ([]Entry, error) {
	if err := s.Prune(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.entries), nil
}

type unavailableWriter struct{ err error }

func (w unavailableWriter) Append(context.Context, Entry) error { return w.err }
func (w unavailableWriter) Prune(context.Context) error         { return w.err }

// Unavailable returns a write boundary that keeps a state failure out of the
// daemon's startup path while making every inference audit fail closed.
func Unavailable(err error) interface {
	Append(context.Context, Entry) error
	Prune(context.Context) error
} {
	if err == nil {
		err = errors.New("advisory store unavailable")
	}
	return unavailableWriter{err: err}
}
