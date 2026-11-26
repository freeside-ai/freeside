package cordonhost

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

var fixtureBaseSHA = strings.Repeat("ab", 20)

// exportFixture is a real export of a worktree holding files, as the
// trusted exporter writes it on the launcher's side.
func exportFixture(t *testing.T, files map[string]string) cordon.ExportResult {
	t.Helper()
	worktree := t.TempDir()
	for name, body := range files {
		if err := os.MkdirAll(filepath.Join(worktree, filepath.Dir(name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "export")
	manifest, err := export.Export(os.DirFS(worktree), out, export.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return cordon.ExportResult{Dir: out, Manifest: manifest, BaseSHA: fixtureBaseSHA}
}

// assertSameTree fails unless got holds exactly want's names, kinds, and
// file bytes.
func assertSameTree(t *testing.T, want, got string) {
	t.Helper()
	read := func(root string) map[string]string {
		tree := map[string]string{}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || path == root {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if !d.Type().IsRegular() {
				tree[rel] = d.Type().String()
				return nil
			}
			body, err := os.ReadFile(path) //nolint:gosec // G304: a test's own tree
			tree[rel] = string(body)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return tree
	}
	w, g := read(want), read(got)
	if len(w) != len(g) {
		t.Fatalf("received %d entries, sent %d", len(g), len(w))
	}
	for name, body := range w {
		if g[name] != body {
			t.Fatalf("received %s differs from what was sent", name)
		}
	}
}

// rawResponse is one connection's answer from a rawLauncher: a reply frame,
// then whatever stream writes.
type rawResponse struct {
	reply  *replyBody
	stream func(io.Writer)
}

// rawLauncher answers each connection with the next response, writing
// bytes no real launcher would, so the receipt's refusals can be shown.
type rawLauncher struct {
	socket string
	mu     sync.Mutex
	queue  []rawResponse
	dials  int
}

func newRawLauncher(t *testing.T, responses ...rawResponse) *rawLauncher {
	t.Helper()
	l := &rawLauncher{socket: filepath.Join(shortDir(t), "l.sock"), queue: responses}
	ln, err := net.Listen("unix", l.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			l.answer(conn)
		}
	}()
	return l
}

func (l *rawLauncher) answer(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	l.mu.Lock()
	l.dials++
	if len(l.queue) == 0 {
		l.mu.Unlock()
		return
	}
	next := l.queue[0]
	l.queue = l.queue[1:]
	l.mu.Unlock()
	if _, err := readFrame(conn, true); err != nil {
		return
	}
	hello := &helloBody{Build: "build", ConfigurationDigest: digestOf("config"), Account: &Account{Name: "run", UID: 501}}
	if writeFrame(conn, frame{Kind: kindHello, Hello: hello}) != nil {
		return
	}
	if _, err := readFrame(conn, false); err != nil {
		return
	}
	if writeFrame(conn, frame{Kind: kindReply, Reply: next.reply}) != nil {
		return
	}
	if next.stream != nil {
		next.stream(conn)
	}
}

func (l *rawLauncher) dialed() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dials
}

func exportReply() *replyBody { return &replyBody{Export: &exportReplyBody{BaseSHA: fixtureBaseSHA}} }

// sends answers with the export reply and streams dir as a launcher does.
func sends(dir string) rawResponse {
	return rawResponse{reply: exportReply(), stream: func(w io.Writer) { _ = writeTree(w, dir) }}
}

// sendsTar answers with the export reply and streams the tar build writes.
// Close errors are ignored, so a header may promise bytes never sent.
func sendsTar(build func(*tar.Writer)) rawResponse {
	return rawResponse{reply: exportReply(), stream: func(w io.Writer) {
		cw := chunkWriter{w: w}
		tw := tar.NewWriter(cw)
		build(tw)
		_ = tw.Close()
		_ = cw.end()
	}}
}

// sendsChunked answers with the export reply and streams raw tar bytes in
// chunks, ended.
func sendsChunked(raw []byte) rawResponse {
	return rawResponse{reply: exportReply(), stream: func(w io.Writer) {
		cw := chunkWriter{w: w}
		_, _ = cw.Write(raw)
		_ = cw.end()
	}}
}

// receiptPlaces is an export root inside a parent directory the test
// watches, so an entry that escaped the root is seen.
type receiptPlaces struct{ parent, root string }

func newReceiptPlaces(t *testing.T) receiptPlaces {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "exports")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return receiptPlaces{parent: parent, root: root}
}

func newReceiptClient(t *testing.T, socket, exportRoot string) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{
		Socket: socket, Build: "build", Seeder: &countingSeeder{}, Recorder: &collectingRecorder{},
		ScratchRoot: t.TempDir(), ExportRoot: exportRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Detach)
	return c
}

// assertNothingKept fails unless the refused receipt left no export, no
// record, no partial tree, and nothing beside the export root.
func (p receiptPlaces) assertNothingKept(t *testing.T, runID string) {
	t.Helper()
	beside, err := os.ReadDir(p.parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(beside) != 1 || beside[0].Name() != "exports" {
		t.Fatalf("the receipt reached %v beside the export root", beside)
	}
	under, err := os.ReadDir(p.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(under) > 1 || (len(under) == 1 && under[0].Name() != runID) {
		t.Fatalf("the export root holds %v", under)
	}
	left, err := os.ReadDir(filepath.Join(p.root, runID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("a refused receipt left %v", left)
	}
}

// pemFixture is a private key's armor, split so no tracked line matches
// the credential rules (scripts/check-credential-literals.sh).
const pemFixture = "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nabc\n"

// TestAReceiptRefusesWhatNoExportHolds shows a stream a launcher could send
// that no clean export is, and a reply that names no base commit, each
// refuse the export and leave nothing at or beside the export root.
func TestAReceiptRefusesWhatNoExportHolds(t *testing.T) {
	t.Parallel()
	file := func(tw *tar.Writer, name string) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: 1})
		_, _ = tw.Write([]byte("x"))
	}
	good := exportFixture(t, map[string]string{"README": "the change\n"})
	tampered := exportFixture(t, map[string]string{"README": "the change\n"})
	if err := os.WriteFile(filepath.Join(tampered.Dir, export.ManifestFilename), []byte(`{"version":"other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(p receiptPlaces) rawResponse{
		"parent name": func(receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) { file(tw, "../../../escaped") })
		},
		"absolute name": func(p receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) { file(tw, filepath.Join(p.parent, "escaped")) })
		},
		"link inside": func(receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) {
				_ = tw.WriteHeader(&tar.Header{Name: "inside", Typeflag: tar.TypeSymlink, Linkname: "manifest.json", Mode: 0o700})
			})
		},
		"link outside": func(receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) {
				_ = tw.WriteHeader(&tar.Header{Name: "out", Typeflag: tar.TypeSymlink, Linkname: "../../..", Mode: 0o700})
				file(tw, "out/escaped")
			})
		},
		"hard link": func(receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) {
				file(tw, "a")
				_ = tw.WriteHeader(&tar.Header{Name: "b", Typeflag: tar.TypeLink, Linkname: "a"})
			})
		},
		"a duplicate entry": func(receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) {
				file(tw, "a")
				file(tw, "a")
			})
		},
		// The stream ends cleanly, so this is the tar's content, not a cut.
		"a file the tar ends inside": func(receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) {
				_ = tw.WriteHeader(&tar.Header{Name: "short", Typeflag: tar.TypeReg, Mode: 0o600, Size: 10})
				_, _ = tw.Write([]byte("x"))
			})
		},
		"fifo": func(receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) {
				_ = tw.WriteHeader(&tar.Header{Name: "pipe", Typeflag: tar.TypeFifo, Mode: 0o600})
			})
		},
		"device": func(receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) {
				_ = tw.WriteHeader(&tar.Header{Name: "dev", Typeflag: tar.TypeChar, Mode: 0o600})
			})
		},
		"too many entries": func(receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) {
				for i := range maxExportEntries + 1 {
					_ = tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("d%05d/", i), Typeflag: tar.TypeDir, Mode: 0o700})
				}
			})
		},
		"too many bytes": func(receiptPlaces) rawResponse {
			return sendsTar(func(tw *tar.Writer) {
				_ = tw.WriteHeader(&tar.Header{Name: "big", Typeflag: tar.TypeReg, Mode: 0o600, Size: maxExportBytes + 1})
			})
		},
		// A sparse file extracts bytes the stream never carried, past any
		// bound on the stream's own bytes.
		"sparse file": func(receiptPlaces) rawResponse { return sendsChunked(sparseTar()) },
		"cut mid-chunk": func(receiptPlaces) rawResponse {
			return rawResponse{reply: exportReply(), stream: func(w io.Writer) {
				var size [4]byte
				binary.BigEndian.PutUint32(size[:], 1000)
				_, _ = w.Write(size[:])
				_, _ = w.Write(make([]byte, 10))
			}}
		},
		"cut before its end": func(receiptPlaces) rawResponse {
			return rawResponse{reply: exportReply(), stream: func(w io.Writer) {
				var all bytes.Buffer
				_ = writeTree(&all, good.Dir)
				_, _ = w.Write(all.Bytes()[:all.Len()-4])
			}}
		},
		"a credential": func(receiptPlaces) rawResponse {
			return sends(exportFixture(t, map[string]string{"id_ed25519": pemFixture}).Dir)
		},
		"a malformed manifest": func(receiptPlaces) rawResponse { return sends(tampered.Dir) },
		"a malformed base": func(receiptPlaces) rawResponse {
			r := sends(good.Dir)
			r.reply = &replyBody{Export: &exportReplyBody{BaseSHA: "main"}}
			return r
		},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newReceiptPlaces(t)
			l := newRawLauncher(t, response(p))
			c := newReceiptClient(t, l.socket, p.root)
			res, err := c.Export(t.Context(), cordon.ExportRequest{RunID: "run-1"})
			if err == nil {
				t.Fatalf("the receipt was accepted: %+v", res)
			}
			// A cut stream is asked for again; any other refusal repeats on
			// every ask, so the gate ends the run refused.
			var scan *cordon.ScanRefusalError
			switch name {
			case "a credential":
				if !errors.As(err, &scan) {
					t.Fatalf("a credential = %v, want a scan refusal", err)
				}
			case "cut mid-chunk", "cut before its end":
				if !errors.Is(err, ErrProtocol) || errors.Is(err, ErrExportRefused) {
					t.Fatalf("%s = %v, want ErrProtocol alone", name, err)
				}
			default:
				if !errors.Is(err, ErrExportRefused) {
					t.Fatalf("%s = %v, want ErrExportRefused", name, err)
				}
			}
			p.assertNothingKept(t, "run-1")
		})
	}
}

// TestAReceiptIsKeptAndAuthenticatedOnTheClient shows a receipt cut short
// is received whole on the next request, a fresh client over the same
// export root returns it without asking the launcher, its authentication
// needs no launcher, an edited member or a lost record refuses it, and
// a released run leaves nothing.
func TestAReceiptIsKeptAndAuthenticatedOnTheClient(t *testing.T) {
	t.Parallel()
	exp := exportFixture(t, map[string]string{"README": "the change\n", "cmd/main.go": "package main\n"})
	cut := rawResponse{reply: exportReply(), stream: func(w io.Writer) {
		var all bytes.Buffer
		_ = writeTree(&all, exp.Dir)
		_, _ = w.Write(all.Bytes()[:all.Len()/2])
	}}
	p := newReceiptPlaces(t)
	l := newRawLauncher(t, cut, sends(exp.Dir), sends(exp.Dir), rawResponse{reply: &replyBody{}})
	c := newReceiptClient(t, l.socket, p.root)
	if _, err := c.Export(t.Context(), cordon.ExportRequest{RunID: "run-1"}); err == nil {
		t.Fatal("a cut receipt was accepted")
	}
	res, err := c.Export(t.Context(), cordon.ExportRequest{RunID: "run-1"})
	if err != nil {
		t.Fatalf("the repeat: %v", err)
	}
	assertSameTree(t, exp.Dir, res.Dir)
	if res.BaseSHA != fixtureBaseSHA || res.Manifest.Version != exp.Manifest.Version || len(res.Manifest.Entries) != len(exp.Manifest.Entries) {
		t.Fatalf("received %+v", res)
	}

	// A restarted daemon's client holds the receipt; with the launcher
	// gone it still returns and authenticates it.
	fresh := newReceiptClient(t, filepath.Join(p.parent, "absent.sock"), p.root)
	again, err := fresh.Export(t.Context(), cordon.ExportRequest{RunID: "run-1"})
	if err != nil || again.Dir != res.Dir || again.BaseSHA != fixtureBaseSHA {
		t.Fatalf("a fresh client's export = %+v, %v", again, err)
	}
	rec, err := fresh.Recover(t.Context(), cordon.RecoverRequest{RunID: "run-1", Export: cordon.ExportRequest{RunID: "run-1"}})
	if err != nil || rec.Outcome != cordon.RecoveryExported || rec.Export.Dir != res.Dir {
		t.Fatalf("a fresh client's recovery = %+v, %v", rec, err)
	}
	if err := fresh.AuthenticateReleasedExport(t.Context(), "run-1", res.Dir); err != nil {
		t.Fatalf("authenticate with no launcher: %v", err)
	}
	if l.dialed() != 2 {
		t.Fatalf("the launcher was asked %d times, want 2", l.dialed())
	}
	if err := fresh.AuthenticateReleasedExport(t.Context(), "run-1", exp.Dir); !errors.Is(err, cordon.ErrExportUnauthenticated) {
		t.Fatalf("another directory = %v, want ErrExportUnauthenticated", err)
	}

	manifest := filepath.Join(res.Dir, export.ManifestFilename)
	body, err := os.ReadFile(manifest) //nolint:gosec // G304: the test's own receipt
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, append(body, ' '), 0o600); err != nil { //nolint:gosec // G703: the test's own receipt
		t.Fatal(err)
	}
	if err := fresh.AuthenticateReleasedExport(t.Context(), "run-1", res.Dir); !errors.Is(err, cordon.ErrExportUnauthenticated) {
		t.Fatalf("an edited manifest = %v, want ErrExportUnauthenticated", err)
	}
	if _, err := fresh.Export(t.Context(), cordon.ExportRequest{RunID: "run-1"}); !errors.Is(err, cordon.ErrExportUnauthenticated) {
		t.Fatalf("an edited manifest's export = %v, want ErrExportUnauthenticated", err)
	}
	if err := os.WriteFile(manifest, body, 0o600); err != nil { //nolint:gosec // G703: the test's own receipt
		t.Fatal(err)
	}

	// With its record gone the receipt is no export: authentication
	// refuses, and the next request clears it and receives it again.
	if err := os.Remove(filepath.Join(p.root, "run-1", recordName)); err != nil {
		t.Fatal(err)
	}
	if err := c.AuthenticateReleasedExport(t.Context(), "run-1", res.Dir); !errors.Is(err, cordon.ErrExportUnauthenticated) {
		t.Fatalf("a receipt with no record = %v, want ErrExportUnauthenticated", err)
	}
	if res, err = c.Export(t.Context(), cordon.ExportRequest{RunID: "run-1"}); err != nil {
		t.Fatalf("a receipt with no record received again: %v", err)
	}
	assertSameTree(t, exp.Dir, res.Dir)

	if err := c.ReleaseFinished(t.Context(), []string{"run-1"}); err != nil {
		t.Fatalf("ReleaseFinished: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.root, "run-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a released run left its receipt: %v", err)
	}
}

// TestACrashedRecordWriteIsCleared shows the temporary file a crash
// leaves while the receipt record is written, with the export it was
// recording, is cleared and the export received again.
func TestACrashedRecordWriteIsCleared(t *testing.T) {
	t.Parallel()
	exp := exportFixture(t, map[string]string{"README": "the change\n"})
	p := newReceiptPlaces(t)
	run := filepath.Join(p.root, "run-1")
	if err := os.MkdirAll(filepath.Join(run, exportName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "."+recordName+"-123.tmp"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newRawLauncher(t, sends(exp.Dir))
	c := newReceiptClient(t, l.socket, p.root)
	res, err := c.Export(t.Context(), cordon.ExportRequest{RunID: "run-1"})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	assertSameTree(t, exp.Dir, res.Dir)
	entries, err := os.ReadDir(run)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{exportName, recordName}) {
		t.Fatalf("the receipt directory holds %v", names)
	}
}

// TestAReceiptDirectoryHoldingAnythingElseIsNotCleared shows the client
// removes only the names a receipt puts in a run's directory: a directory
// holding another name is refused, not cleared.
func TestAReceiptDirectoryHoldingAnythingElseIsNotCleared(t *testing.T) {
	t.Parallel()
	p := newReceiptPlaces(t)
	stranger := filepath.Join(p.root, "run-1", "worktree")
	if err := os.MkdirAll(stranger, 0o700); err != nil {
		t.Fatal(err)
	}
	l := newRawLauncher(t, rawResponse{reply: &replyBody{}})
	c := newReceiptClient(t, l.socket, p.root)
	if _, err := c.Export(t.Context(), cordon.ExportRequest{RunID: "run-1"}); err == nil {
		t.Fatal("a directory holding another name was received into")
	}
	if err := c.ReleaseFinished(t.Context(), []string{"run-1"}); err == nil {
		t.Fatal("a directory holding another name was released")
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Fatalf("the other name is gone: %v", err)
	}
}

// sparseTar is a PAX GNU sparse 1.0 file: Go's tar writer refuses to write
// one, so the bytes are laid out by hand. Its header keeps TypeReg, and its
// map claims a logical size far past the five bytes it carries.
func sparseTar() []byte {
	var out bytes.Buffer
	records := paxRecord("GNU.sparse.major", "1") + paxRecord("GNU.sparse.minor", "0") +
		paxRecord("GNU.sparse.name", "sparse") + paxRecord("GNU.sparse.realsize", strconv.Itoa(1<<20))
	out.Write(ustarHeader("PaxHeaders/sparse", tar.TypeXHeader, int64(len(records))))
	out.Write(padBlock([]byte(records)))
	sparseMap := padBlock([]byte("1\n0\n5\n"))
	out.Write(ustarHeader("sparse", tar.TypeReg, int64(len(sparseMap)+5)))
	out.Write(sparseMap)
	out.Write(padBlock([]byte("hello")))
	out.Write(make([]byte, 1024))
	return out.Bytes()
}

func paxRecord(key, value string) string {
	base := len(" " + key + "=" + value + "\n")
	n := base + 1
	for len(strconv.Itoa(n))+base != n {
		n = len(strconv.Itoa(n)) + base
	}
	return fmt.Sprintf("%d %s=%s\n", n, key, value)
}

func ustarHeader(name string, typeflag byte, size int64) []byte {
	b := make([]byte, 512)
	copy(b[0:100], name)
	copy(b[100:108], "0000600\x00")
	copy(b[108:116], "0000000\x00")
	copy(b[116:124], "0000000\x00")
	copy(b[124:136], fmt.Sprintf("%011o\x00", size))
	copy(b[136:148], fmt.Sprintf("%011o\x00", 0))
	b[156] = typeflag
	copy(b[257:263], "ustar\x00")
	copy(b[263:265], "00")
	copy(b[148:156], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

func padBlock(b []byte) []byte {
	if r := len(b) % 512; r != 0 {
		b = append(b, make([]byte, 512-r)...)
	}
	return b
}
