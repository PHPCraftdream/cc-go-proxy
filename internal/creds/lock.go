package creds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/securefile"
)

const lockRetryInterval = 10 * time.Millisecond

// LockFile locks path using a persistent path+".lock" sidecar. Never remove
// the sidecar: kernel locks are released automatically when a process exits.
func LockFile(ctx context.Context, path string) (unlock func(), err error) {
	if ctx == nil {
		return nil, errors.New("lock context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	closeOnError := func(err error) (func(), error) {
		_ = f.Close()
		return nil, err
	}
	if err := securefile.RestrictToOwner(f.Name()); err != nil {
		return closeOnError(fmt.Errorf("restrict lock file: %w", err))
	}

	var release func() error
	for {
		if err := ctx.Err(); err != nil {
			return closeOnError(err)
		}
		var acquired bool
		release, acquired, err = tryLockFile(f)
		if err != nil {
			return closeOnError(fmt.Errorf("lock %s: %w", path, err))
		}
		if acquired {
			if err := ctx.Err(); err != nil {
				_ = release()
				return closeOnError(err)
			}
			var once sync.Once
			return func() {
				once.Do(func() {
					_ = release()
					_ = f.Close()
				})
			}, nil
		}

		timer := time.NewTimer(lockRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return closeOnError(ctx.Err())
		case <-timer.C:
		}
	}
}
