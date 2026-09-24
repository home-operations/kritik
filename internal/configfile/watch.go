package configfile

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"time"
)

// Watch polls path every interval and calls apply with each new File whose
// content differs from the last one applied. It returns when ctx is done.
//
// Polling rather than inotify: a ConfigMap mount updates by swapping a
// symlink, which inotify on the file itself misses, and a content hash makes
// the loop idempotent regardless of how the bytes arrived. A file that fails
// to load is logged and skipped, so the last good state stays live; the same
// bad content is not re-logged on every tick.
func Watch(ctx context.Context, path string, interval time.Duration, logger *slog.Logger, apply func(*File)) {
	var applied, rejected [sha256.Size]byte
	tick := func() {
		raw, err := os.ReadFile(path)
		if err != nil {
			logger.Error("configfile: read failed, keeping the last good configuration", "path", path, "error", err)
			return
		}
		sum := sha256.Sum256(raw)
		if sum == applied || sum == rejected {
			return
		}
		f, err := Parse(raw)
		if err != nil {
			rejected = sum
			logger.Error("configfile: rejected, keeping the last good configuration", "path", path, "error", err)
			return
		}
		applied = sum
		rejected = [sha256.Size]byte{}
		apply(f)
	}

	// The caller has already loaded once at startup; seed the hash from that
	// content so the first tick is a no-op rather than a duplicate apply.
	if raw, err := os.ReadFile(path); err == nil {
		applied = sha256.Sum256(raw)
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
