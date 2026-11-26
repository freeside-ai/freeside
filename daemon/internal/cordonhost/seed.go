package cordonhost

import (
	"archive/tar"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// A tree crosses the connection as a tar in chunks: each a 4-byte
// big-endian length and that many bytes, the stream ended by a zero
// length. Two trees cross. The seed is a launch's base, the checkout the
// daemon's own seeder made (its exact base, .git included), streamed after
// the launch frame; the launcher holds no forge credential, so this is the
// only source a launch on a host has. The export is a clean run's export
// directory, streamed after the reply that names it; the client receives
// it as untrusted input (receipt.go).

const (
	// streamChunk is the size a writer writes in.
	streamChunk = 1 << 16
	// maxExportBytes, maxExportEntries, and maxExportStreamBytes are the
	// ward's daemon-side receipt bounds, ward.Config's MaxExportBytes,
	// MaxExportEntries, and MaxArchiveBytes defaults, so an export from a
	// host is held to what an export from the ward is.
	maxExportBytes       = 2 << 30
	maxExportEntries     = 10_000
	maxExportStreamBytes = 4 << 30
)

// treeRules are what a tree stream may carry.
type treeRules struct {
	// what names the tree in errors.
	what string
	// refused is what a refusal for what the stream holds matches; a
	// stream cut short is ErrProtocol alone.
	refused error
	// maxStreamBytes bounds the stream's bytes, and maxBytes, apart, the
	// file bytes extracted from it, which a sparse entry would make the
	// larger.
	maxStreamBytes int64
	maxBytes       int64
	maxEntries     int
	// links accepts symbolic links and keeps a file's executable bit.
	// Without it a tree holds only directories and regular files, each
	// file created 0600.
	links bool
}

var (
	seedRules = treeRules{
		what: "seed", refused: ErrProtocol, maxStreamBytes: 1 << 30, maxBytes: 1 << 30, maxEntries: 1 << 20, links: true,
	}
	exportRules = treeRules{
		what: "export", refused: ErrExportRefused, maxStreamBytes: maxExportStreamBytes,
		maxBytes: maxExportBytes, maxEntries: maxExportEntries,
	}
)

// chunkWriter frames what is written to it as stream chunks.
type chunkWriter struct{ w io.Writer }

func (c chunkWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), streamChunk)
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(n))
		if _, err := c.w.Write(size[:]); err != nil {
			return written, err
		}
		if _, err := c.w.Write(p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// end writes the zero length that ends the stream.
func (c chunkWriter) end() error {
	_, err := c.w.Write(make([]byte, 4))
	return err
}

// chunkReader reads a stream's bytes, refusing more than its rules'
// maxStreamBytes, until the zero length, after which it reports io.EOF.
type chunkReader struct {
	r     io.Reader
	rules treeRules
	left  uint32
	total int64
	ended bool
}

func (c *chunkReader) Read(p []byte) (int, error) {
	for c.left == 0 {
		if c.ended {
			return 0, io.EOF
		}
		var size [4]byte
		if _, err := io.ReadFull(c.r, size[:]); err != nil {
			return 0, fmt.Errorf("%w: the %s ended early: %w", ErrProtocol, c.rules.what, err)
		}
		n := binary.BigEndian.Uint32(size[:])
		if n == 0 {
			c.ended = true
			continue
		}
		if n > streamChunk {
			return 0, fmt.Errorf("%w: the %s sent a chunk of %d bytes", c.rules.refused, c.rules.what, n)
		}
		if c.total += int64(n); c.total > c.rules.maxStreamBytes {
			return 0, fmt.Errorf("%w: the %s is over %d bytes", c.rules.refused, c.rules.what, c.rules.maxStreamBytes)
		}
		c.left = n
	}
	if uint32(len(p)) > c.left { //nolint:gosec // G115: compared, not converted back
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= uint32(n) //nolint:gosec // G115: n is at most len(p), at most c.left
	if errors.Is(err, io.EOF) {
		err = fmt.Errorf("%w: the %s ended mid-chunk", ErrProtocol, c.rules.what)
	}
	return n, err
}

// drain reads to the stream's end, so the next frame starts where it
// should; a tar reader stops at its own end marker, which may come before
// the client's padding and the zero length.
func (c *chunkReader) drain() error {
	_, err := io.Copy(io.Discard, c)
	return err
}

// writeTree streams dir, the daemon's seed or the launcher's export:
// directories, regular files with their executable bit, and symbolic
// links. Any other entry refuses the stream. The reader's rules decide
// what of it is accepted.
func writeTree(w io.Writer, dir string) error {
	cw := chunkWriter{w: w}
	tw := tar.NewWriter(cw)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: filepath.ToSlash(rel), Format: tar.FormatPAX}
		switch mode := info.Mode(); {
		case mode.IsDir():
			hdr.Typeflag, hdr.Mode = tar.TypeDir, 0o700
		case mode.IsRegular():
			hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, 0o600, info.Size()
			if mode&0o111 != 0 {
				hdr.Mode = 0o700
			}
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, target, 0o700
		default:
			return fmt.Errorf("cordonhost: the tree holds %s, which is neither a directory, a file, nor a link", rel)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		f, err := os.Open(path) //nolint:gosec // G304: a file under the seed or export directory this side wrote
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
			return fmt.Errorf("cordonhost: stream %s: %w", rel, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return cw.end()
}

// readTree extracts a stream into dir, which must not exist, through an
// os.Root over it, so no entry lands outside dir however the stream names
// it or links. It accepts what writeTree writes, within rules, and nothing
// else: local names, directories, regular files of mode 0600 or 0700, and,
// where rules allow them, symbolic links, which it creates and never
// follows. A refusal leaves what was extracted for the caller to remove.
func readTree(r io.Reader, dir string, rules treeRules) error {
	cr := &chunkReader{r: r, rules: rules}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(cr)
	var extracted int64
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		// The chunk reader's errors carry their own class; any other is
		// the tar's, and so what the stream holds.
		if errors.Is(err, ErrProtocol) {
			return fmt.Errorf("the %s: %w", rules.what, err)
		}
		if err != nil {
			return fmt.Errorf("%w: the %s: %w", rules.refused, rules.what, err)
		}
		if entries >= rules.maxEntries {
			return fmt.Errorf("%w: the %s holds over %d entries", rules.refused, rules.what, rules.maxEntries)
		}
		if hdr.Size < 0 || hdr.Size > rules.maxBytes-extracted {
			return fmt.Errorf("%w: the %s extracts over %d bytes", rules.refused, rules.what, rules.maxBytes)
		}
		extracted += hdr.Size
		if err := extract(root, tr, hdr, rules); err != nil {
			return extractFailure(err, rules)
		}
	}
	return cr.drain()
}

// extractFailure classes an entry's extraction failure. The chunk
// reader's carry their own class, and the host's I/O failures stay
// unclassed so the stream is asked for again. Any other, such as an entry
// that already exists, a parent that doesn't, or a file the tar ends
// inside, is what the stream holds.
func extractFailure(err error, rules treeRules) error {
	switch {
	case errors.Is(err, ErrProtocol):
		return err
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT), errors.Is(err, syscall.EIO):
		return fmt.Errorf("cordonhost: extract the %s: %w", rules.what, err)
	}
	return fmt.Errorf("%w: the %s: %w", rules.refused, rules.what, err)
}

func extract(root *os.Root, tr *tar.Reader, hdr *tar.Header, rules treeRules) error {
	name := filepath.FromSlash(strings.TrimSuffix(hdr.Name, "/"))
	if !filepath.IsLocal(name) || strings.Contains(hdr.Name, `\`) {
		return fmt.Errorf("%w: the %s names an entry %q", rules.refused, rules.what, hdr.Name)
	}
	// writeTree never writes a sparse file, whose holes would extract as
	// bytes the stream never carried.
	for key := range hdr.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return fmt.Errorf("%w: %s entry %q is sparse", rules.refused, rules.what, hdr.Name)
		}
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return root.Mkdir(name, 0o700)
	case tar.TypeReg:
		if hdr.Mode != 0o600 && hdr.Mode != 0o700 {
			return fmt.Errorf("%w: %s file %q has mode %o", rules.refused, rules.what, hdr.Name, hdr.Mode)
		}
		mode := fs.FileMode(0o600)
		if rules.links {
			mode = fs.FileMode(hdr.Mode) //nolint:gosec // G115: hdr.Mode is 0600 or 0700, checked above
		}
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		_, err = io.CopyN(f, tr, hdr.Size)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return err
	case tar.TypeSymlink:
		if !rules.links {
			break
		}
		if hdr.Linkname == "" {
			return fmt.Errorf("%w: %s link %q has no target", rules.refused, rules.what, hdr.Name)
		}
		return root.Symlink(hdr.Linkname, name)
	}
	return fmt.Errorf("%w: %s entry %q of type %q", rules.refused, rules.what, hdr.Name, hdr.Typeflag)
}
