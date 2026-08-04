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

// VendorInstructionConfig identifies the operator-owned instruction file and
// conformed native delivery binding for one vendor. HostPath is resolved by
// the kernel at admission; a final symlink is allowed and its target bytes are
// frozen, while the live path is never exposed to the container.
type VendorInstructionConfig struct {
	Vendor             domain.AgentVendor
	Delivery           domain.VendorInstructionDelivery
	HostPath           string
	ForbiddenHostPaths []string
}

type pinnedForbiddenHostInput struct {
	path string
	file *os.File
	info fs.FileInfo
}

func (c VendorInstructionConfig) validate() error {
	if err := domain.ValidateVendorInstructionBinding(c.Vendor, c.Delivery); err != nil {
		return err
	}
	if !filepath.IsAbs(c.HostPath) || filepath.Clean(c.HostPath) != c.HostPath ||
		c.HostPath == string(filepath.Separator) {
		return fmt.Errorf(
			"vendor instruction path %q is not a clean absolute non-root path",
			c.HostPath,
		)
	}
	return nil
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
	if err := cfg.validate(); err != nil {
		return domain.VendorInstructionSnapshot{}, nil, err
	}
	snapshot := domain.VendorInstructionSnapshot{
		Vendor: cfg.Vendor, Delivery: cfg.Delivery,
	}
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

	forbidden, err := pinForbiddenHostInputs(cfg.ForbiddenHostPaths)
	if err != nil {
		return domain.VendorInstructionSnapshot{}, nil, err
	}
	defer closePinnedForbiddenHostInputs(forbidden)

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
	for _, input := range forbidden {
		if os.SameFile(before, input.info) {
			_ = file.Close()
			return domain.VendorInstructionSnapshot{}, nil,
				errors.New("vendor instruction source aliases a forbidden host input")
		}
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
	if err := revalidatePinnedForbiddenHostInputs(forbidden); err != nil {
		return domain.VendorInstructionSnapshot{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return domain.VendorInstructionSnapshot{}, nil, err
	}
	sum := sha256.Sum256(body)
	digest := domain.Digest(fmt.Sprintf("sha256:%x", sum))
	snapshot.Digest = &digest
	return snapshot, body, nil
}

func pinForbiddenHostInputs(paths []string) ([]pinnedForbiddenHostInput, error) {
	inputs := make([]pinnedForbiddenHostInput, 0, len(paths))
	for _, path := range paths {
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // deployment-owned forbidden input path
		if err != nil {
			closePinnedForbiddenHostInputs(inputs)
			return nil, fmt.Errorf("open forbidden vendor instruction source %q: %w", path, err)
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			closePinnedForbiddenHostInputs(inputs)
			if err != nil {
				return nil, fmt.Errorf("stat forbidden vendor instruction source %q: %w", path, err)
			}
			return nil, fmt.Errorf("forbidden vendor instruction source %q is not a regular file", path)
		}
		if current, err := os.Stat(path); err != nil || !os.SameFile(info, current) {
			_ = file.Close()
			closePinnedForbiddenHostInputs(inputs)
			if err != nil {
				return nil, fmt.Errorf("revalidate forbidden vendor instruction source %q: %w", path, err)
			}
			return nil, fmt.Errorf("forbidden vendor instruction source %q changed while admission pinned it", path)
		}
		inputs = append(inputs, pinnedForbiddenHostInput{path: path, file: file, info: info})
	}
	return inputs, nil
}

func revalidatePinnedForbiddenHostInputs(inputs []pinnedForbiddenHostInput) error {
	for _, input := range inputs {
		current, err := os.Stat(input.path)
		if err != nil {
			return fmt.Errorf("revalidate forbidden vendor instruction source %q: %w", input.path, err)
		}
		if !os.SameFile(input.info, current) {
			return fmt.Errorf("forbidden vendor instruction source %q changed while admission read", input.path)
		}
	}
	return nil
}

func closePinnedForbiddenHostInputs(inputs []pinnedForbiddenHostInput) {
	for _, input := range inputs {
		_ = input.file.Close()
	}
}
