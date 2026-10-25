package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/angel"
)

// logChunkSize is how many bytes tailLines reads at a time from the end.
// Tests shrink it to exercise multi-chunk tails.
var logChunkSize = 16 * 1024

// tailLines returns the last n lines of path, keeping any trailing '\r'
// bytes intact. Missing files report fs.ErrNotExist.
func tailLines(path string, n int) ([]string, error) {
	if n == 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return nil, nil
	}

	// Read backwards until the buffer holds enough complete lines.
	var buf []byte
	atStart := false
	for len(buf) == 0 || countNewlines(buf) <= n {
		chunk := int64(logChunkSize)
		end := size - int64(len(buf))
		if end-chunk <= 0 {
			chunk = end
			atStart = true
		}
		start := end - chunk
		data := make([]byte, chunk)
		if _, err := f.ReadAt(data, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read log: %w", err)
		}
		buf = append(data, buf...)
		if atStart {
			break
		}
	}

	// When we stopped early, the first segment may be a partial line.
	lines := splitLines(buf)
	if !atStart && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func countNewlines(b []byte) int {
	c := 0
	for _, x := range b {
		if x == '\n' {
			c++
		}
	}
	return c
}

// splitLines splits on '\n', dropping one trailing empty element from a
// final newline; a file without a trailing newline keeps its last line.
func splitLines(b []byte) []string {
	parts := string(b)
	var lines []string
	for len(parts) > 0 {
		i := strings.IndexByte(parts, '\n')
		if i < 0 {
			lines = append(lines, parts)
			break
		}
		lines = append(lines, parts[:i])
		parts = parts[i+1:]
	}
	return lines
}

// parseLogsArgs parses the flags of the --logs command.
func parseLogsArgs(args []string) (n int, follow bool, err error) {
	n = 50
	usage := errors.New("usage: ccgp --logs [-f] [-n N]")
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-f":
			follow = true
		case "-n":
			if i+1 >= len(args) {
				return 0, false, usage
			}
			i++
			v, cerr := strconv.Atoi(args[i])
			if cerr != nil || v < 0 {
				return 0, false, usage
			}
			n = v
		default:
			return 0, false, usage
		}
	}
	return n, follow, nil
}

// followLog writes the bytes appended to path after offset until ctx ends.
// The file is looked up by path on every tick, so a rotated or replaced log
// is picked up again from its start; a missing file is waited for.
func followLog(ctx context.Context, w io.Writer, path string, poll time.Duration, offset int64) error {
	var last os.FileInfo
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		info, err := os.Stat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			offset, last = 0, nil
		case err != nil:
			return err
		default:
			if info.Size() < offset || (last != nil && !os.SameFile(last, info)) {
				offset = 0
			}
			last = info
			if info.Size() > offset {
				n, err := copyFrom(w, path, offset)
				offset += n
				if err != nil {
					return err
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// copyFrom writes the bytes of path from offset to the end of the file.
func copyFrom(w io.Writer, path string, offset int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return io.Copy(w, f)
}

// fileSize is the size of path, 0 when it does not exist.
func fileSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func logsCmd(args []string) error {
	n, follow, err := parseLogsArgs(args)
	if err != nil {
		return err
	}
	path, err := angel.LogPath()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Follow from the size seen before the tail was read: nothing is lost.
	offset := fileSize(path)
	lines, err := tailLines(path, n)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(os.Stderr, "no log yet: %s\n", path)
	case err != nil:
		return err
	}
	for _, line := range lines {
		fmt.Println(line)
	}
	if !follow {
		return nil
	}
	if err := followLog(ctx, os.Stdout, path, 250*time.Millisecond, offset); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
