package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// VendorInstructionConfig identifies the operator-owned instruction file a
// vendor loads natively inside its agent container. HostPath is resolved by
// the kernel at admission; a final symlink is allowed and its target bytes are
// frozen, while the live path is never exposed to the container.
type VendorInstructionConfig struct {
	Vendor   domain.AgentVendor
	HostPath string
}

func (c VendorInstructionConfig) validate() error {
	switch c.Vendor {
	case domain.AgentVendorClaude:
		if !filepath.IsAbs(c.HostPath) || filepath.Clean(c.HostPath) != c.HostPath ||
			c.HostPath == string(filepath.Separator) {
			return fmt.Errorf(
				"vendor instruction path %q is not a clean absolute non-root path",
				c.HostPath,
			)
		}
		return nil
	}
	return fmt.Errorf("vendor instruction vendor %q is unsupported", c.Vendor)
}

// snapshotVendorInstructions freezes the bytes reached through the configured
// path. Lstat first distinguishes a genuinely missing path (admitted absence)
// from a dangling symlink or a source removed during admission (failure).
// O_NONBLOCK prevents a raced FIFO or device from hanging before fstat can
// reject it; omitting O_NOFOLLOW is deliberate because the configured final
// symlink is an accepted operator mechanism.
func snapshotVendorInstructions(
	ctx context.Context, cfg VendorInstructionConfig,
) (domain.VendorInstructionSnapshot, []byte, error) {
	snapshot := domain.VendorInstructionSnapshot{Vendor: cfg.Vendor}
	if err := ctx.Err(); err != nil {
		return domain.VendorInstructionSnapshot{}, nil, err
	}
	_, err := os.Lstat(cfg.HostPath)
	if errors.Is(err, fs.ErrNotExist) {
		return snapshot, nil, nil
	}
	if err != nil {
		return domain.VendorInstructionSnapshot{}, nil,
			fmt.Errorf("inspect vendor instruction path %q: %w", cfg.HostPath, err)
	}

	file, err := os.OpenFile(
		cfg.HostPath, os.O_RDONLY|syscall.O_NONBLOCK, 0,
	)
	if err != nil {
		return domain.VendorInstructionSnapshot{}, nil,
			fmt.Errorf("open vendor instruction path %q: %w", cfg.HostPath, err)
	}
	before, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return domain.VendorInstructionSnapshot{}, nil,
			fmt.Errorf("stat vendor instruction path %q: %w", cfg.HostPath, err)
	}
	if !before.Mode().IsRegular() {
		_ = file.Close()
		return domain.VendorInstructionSnapshot{}, nil,
			fmt.Errorf("vendor instruction path %q resolves to %s, not a regular file",
				cfg.HostPath, before.Mode().Type())
	}
	if before.Size() > domain.MaxVendorInstructionBytes {
		_ = file.Close()
		return domain.VendorInstructionSnapshot{}, nil,
			fmt.Errorf("vendor instruction path %q is %d bytes, limit %d",
				cfg.HostPath, before.Size(), domain.MaxVendorInstructionBytes)
	}

	body, readErr := io.ReadAll(io.LimitReader(file, domain.MaxVendorInstructionBytes+1))
	_, seekErr := file.Seek(0, io.SeekStart)
	verifiedBody, verifyErr := io.ReadAll(
		io.LimitReader(file, domain.MaxVendorInstructionBytes+1),
	)
	after, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(readErr, seekErr, verifyErr, statErr, closeErr); err != nil {
		return domain.VendorInstructionSnapshot{}, nil,
			fmt.Errorf("read vendor instruction path %q: %w", cfg.HostPath, err)
	}
	if int64(len(body)) > domain.MaxVendorInstructionBytes ||
		int64(len(verifiedBody)) > domain.MaxVendorInstructionBytes {
		return domain.VendorInstructionSnapshot{}, nil,
			fmt.Errorf("vendor instruction path %q exceeds %d bytes",
				cfg.HostPath, domain.MaxVendorInstructionBytes)
	}
	if !bytes.Equal(body, verifiedBody) ||
		int64(len(body)) != before.Size() || !os.SameFile(before, after) ||
		after.Size() != before.Size() ||
		!after.ModTime().Equal(before.ModTime()) {
		return domain.VendorInstructionSnapshot{}, nil,
			fmt.Errorf("vendor instruction path %q changed while admission read it", cfg.HostPath)
	}
	if err := ctx.Err(); err != nil {
		return domain.VendorInstructionSnapshot{}, nil, err
	}
	sum := sha256.Sum256(body)
	digest := domain.Digest(fmt.Sprintf("sha256:%x", sum))
	snapshot.Digest = &digest
	return snapshot, body, nil
}
