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
)

// The seed is a launch's base, the checkout the daemon's own seeder made
// (its exact base, .git included), streamed after the launch frame as a
// tar in chunks: each a 4-byte big-endian length and that many bytes, the
// stream ended by a zero length. The launcher holds no forge credential, so
// this is the only source a launch on a host has.

const (
	// seedChunk is the size the client writes in.
	seedChunk = 1 << 16
	// maxSeedBytes bounds a seed's tar stream.
	maxSeedBytes = 1 << 30
	// maxSeedEntries bounds a seed's entries.
	maxSeedEntries = 1 << 20
)

// chunkWriter frames what is written to it as seed chunks.
type chunkWriter struct{ w io.Writer }

func (c chunkWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), seedChunk)
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

// chunkReader reads a seed stream's bytes, refusing more than
// maxSeedBytes, until the zero length, after which it reports io.EOF.
type chunkReader struct {
	r     io.Reader
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
			return 0, fmt.Errorf("%w: the seed ended early: %w", ErrProtocol, err)
		}
		n := binary.BigEndian.Uint32(size[:])
		if n == 0 {
			c.ended = true
			continue
		}
		if n > seedChunk {
			return 0, fmt.Errorf("%w: a seed chunk of %d bytes", ErrProtocol, n)
		}
		if c.total += int64(n); c.total > maxSeedBytes {
			return 0, fmt.Errorf("%w: a seed over %d bytes", ErrProtocol, maxSeedBytes)
		}
		c.left = n
	}
	if uint32(len(p)) > c.left { //nolint:gosec // G115: compared, not converted back
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= uint32(n) //nolint:gosec // G115: n is at most len(p), at most c.left
	if errors.Is(err, io.EOF) {
		err = fmt.Errorf("%w: the seed ended mid-chunk", ErrProtocol)
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

// writeSeed streams dir, which the daemon's seeder filled, as the seed:
// directories, regular files with their executable bit, and symbolic
// links. Any other entry refuses the seed.
func writeSeed(w io.Writer, dir string) error {
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
			return fmt.Errorf("cordonhost: the seed holds %s, which is neither a directory, a file, nor a link", rel)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}
		f, err := os.Open(path) //nolint:gosec // G304: a file under the directory the daemon's seeder filled
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
			return fmt.Errorf("cordonhost: seed %s: %w", rel, err)
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

// readSeed extracts a seed stream into dir, which must not exist, through
// an os.Root over it, so no entry lands outside dir however the stream
// names it or links. It accepts what writeSeed writes and nothing else:
// local names, directories, regular files of mode 0600 or 0700, and
// symbolic links, which it creates and never follows.
func readSeed(r io.Reader, dir string) error {
	cr := &chunkReader{r: r}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(cr)
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: the seed: %w", ErrProtocol, err)
		}
		if entries >= maxSeedEntries {
			return fmt.Errorf("%w: a seed of over %d entries", ErrProtocol, maxSeedEntries)
		}
		if err := extract(root, tr, hdr); err != nil {
			return err
		}
	}
	return cr.drain()
}

func extract(root *os.Root, tr *tar.Reader, hdr *tar.Header) error {
	name := filepath.FromSlash(strings.TrimSuffix(hdr.Name, "/"))
	if !filepath.IsLocal(name) || strings.Contains(hdr.Name, `\`) {
		return fmt.Errorf("%w: a seed entry named %q", ErrProtocol, hdr.Name)
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return root.Mkdir(name, 0o700)
	case tar.TypeReg:
		if hdr.Mode != 0o600 && hdr.Mode != 0o700 {
			return fmt.Errorf("%w: seed file %q has mode %o", ErrProtocol, hdr.Name, hdr.Mode)
		}
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(hdr.Mode))
		if err != nil {
			return err
		}
		_, err = io.CopyN(f, tr, hdr.Size)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return err
	case tar.TypeSymlink:
		if hdr.Linkname == "" {
			return fmt.Errorf("%w: seed link %q has no target", ErrProtocol, hdr.Name)
		}
		return root.Symlink(hdr.Linkname, name)
	}
	return fmt.Errorf("%w: seed entry %q of type %q", ErrProtocol, hdr.Name, hdr.Typeflag)
}
