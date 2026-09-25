package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// MaxCheckoutBytes bounds a checkout's total size on the pod's scratch
// volume, beside the fetched pack.
const MaxCheckoutBytes = 256 << 20

// CheckoutStats is what Checkout wrote and what it left out.
type CheckoutStats struct {
	Files int
	Bytes int64
	// Skipped counts ignored paths, symlinks and files over the size cap.
	Skipped int
	// Truncated is set when the total cap stopped the checkout early.
	Truncated bool
}

// Checkout writes the tree's files under dir, an empty directory, for the
// commands the run tool executes. It skips ignored paths, files over the
// read_file size cap and symlinks, which could point out of dir, and stops
// before the total would pass maxBytes. Every file is written 0644,
// whatever mode git recorded: the commands read the checkout, and nothing
// in it is meant to run.
func (t *Tree) Checkout(ctx context.Context, dir string, maxBytes int64) (CheckoutStats, error) {
	var stats CheckoutStats
	err := t.root.Files().ForEach(func(f *object.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if f.Mode == filemode.Symlink || t.ignored(f.Name) || f.Size > maxBlobBytes {
			stats.Skipped++
			return nil
		}
		if stats.Bytes+f.Size > maxBytes {
			stats.Truncated = true
			return errStopWalk
		}
		name, err := cleanPath(f.Name)
		if err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, filepath.FromSlash(name)), f); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		stats.Files++
		stats.Bytes += f.Size
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return stats, fmt.Errorf("agent: checkout: %w", err)
	}
	return stats, nil
}

func writeFile(name string, f *object.File) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	r, err := f.Reader()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	// O_EXCL: a path is written once, never through whatever is there.
	out, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
