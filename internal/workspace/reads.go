package workspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	MaxListedEntries       = 200
	MaxSearchMatches       = 200
	MaxTraversalEntries    = 10000
	MaxSearchFileBytes     = 4 << 20
	MaxSearchTotalBytes    = 32 << 20
	MaxReadToolOutputBytes = 64 << 10
)

// contextReader bounds cancellation latency between local file reads. The
// file itself is opened nonblocking and checked as regular before reading.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p[:min(len(p), 32<<10)])
}

func boundedDir(ctx context.Context, root *os.Root, rel string, limit int) ([]fs.DirEntry, bool, error) {
	f, err := openForRead(root, rel)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, fmt.Errorf("not a directory: %s", rel)
	}
	var entries []fs.DirEntry
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		batch, err := f.ReadDir(min(128, limit+1-len(entries)))
		entries = append(entries, batch...)
		if len(entries) > limit {
			entries = entries[:limit]
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			return entries, true, nil
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, false, nil
}

func (w *Workspace) ListFiles(ctx context.Context, path string) (string, error) {
	_, rel, err := w.toolCanonical(path, "")
	if err != nil {
		return "", err
	}
	r, err := os.OpenRoot(w.Root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	entries, truncated, err := boundedDir(ctx, r, rel, MaxTraversalEntries)
	if err != nil {
		return "", err
	}
	var lines []string
	outputBytes := 0
	for i, e := range entries {
		if controlRelative(filepath.Join(rel, e.Name())) {
			continue
		}
		if i >= MaxListedEntries {
			truncated = true
			break
		}
		kind := "[F]"
		if e.IsDir() {
			kind = "[D]"
		} else if e.Type()&os.ModeSymlink != 0 {
			kind = "[L]"
		} else if e.Type() != 0 {
			kind = "[S]"
		}
		line := kind + " " + filepath.ToSlash(filepath.Join(rel, e.Name()))
		if outputBytes+len(line) > MaxReadToolOutputBytes {
			truncated = true
			break
		}
		lines = append(lines, line)
		outputBytes += len(line) + 1
	}
	if truncated {
		lines = append(lines, "[truncated: at most 200 entries / 64 KiB shown; narrow the directory]")
	}
	if len(lines) == 0 {
		return "(empty)", nil
	}
	return strings.Join(lines, "\n"), nil
}

// SearchFiles never follows discovered symlinks, opens via os.Root, and limits
// visited entries, input bytes and result bytes independently. Partial scans are
// marked explicitly rather than returning a misleading complete "no matches".
func (w *Workspace) SearchFiles(ctx context.Context, pattern, path string) (string, error) {
	if pattern == "" || len(pattern) > 4096 {
		return "", errors.New("search pattern must contain 1..4096 bytes")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}
	_, rel, err := w.toolCanonical(path, "")
	if err != nil {
		return "", err
	}
	r, err := os.OpenRoot(w.Root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	var lines []string
	visited, listed, total, output, skipped := 0, 0, 0, 0, 0
	partial := false
	stop := errors.New("search limit reached")
	var walk func(string) error
	walk = func(path string) error {
		if controlRelative(path) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > MaxTraversalEntries {
			partial = true
			return stop
		}
		info, err := r.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			skipped++
			return nil
		}
		if info.IsDir() {
			if listed >= MaxTraversalEntries {
				partial = true
				return stop
			}
			entries, truncated, err := boundedDir(ctx, r, path, MaxTraversalEntries-listed)
			if err != nil {
				return err
			}
			partial = partial || truncated
			listed += len(entries)
			for _, e := range entries {
				if ignored[e.Name()] {
					continue
				}
				if err := walk(filepath.Join(path, e.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		f, err := openForRead(r, path)
		if err != nil {
			return err
		}
		// A directory entry may have been replaced since Lstat. Verify the
		// opened descriptor, not the old pathname, before accepting its bytes.
		opened, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if !opened.Mode().IsRegular() || opened.Size() > MaxSearchFileBytes {
			f.Close()
			skipped++
			return nil
		}
		remaining := MaxSearchTotalBytes - total
		if remaining <= 0 {
			f.Close()
			partial = true
			return stop
		}
		data, readErr := io.ReadAll(io.LimitReader(contextReader{ctx, f}, int64(min(MaxSearchFileBytes, remaining))+1))
		closeErr := f.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if len(data) > remaining {
			partial = true
			return stop
		}
		total += len(data)
		if len(data) > MaxSearchFileBytes || bytes.IndexByte(data, 0) >= 0 {
			skipped++
			return nil
		}
		s := bufio.NewScanner(bytes.NewReader(data))
		s.Buffer(make([]byte, 4096), MaxReadToolOutputBytes)
		line := 0
		for s.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
			line++
			if !re.Match(s.Bytes()) {
				continue
			}
			match := fmt.Sprintf("%s:%d:%s", filepath.ToSlash(path), line, s.Text())
			if output+len(match) > MaxReadToolOutputBytes {
				partial = true
				return stop
			}
			lines = append(lines, match)
			output += len(match) + 1
			if len(lines) >= MaxSearchMatches {
				partial = true
				return stop
			}
		}
		if s.Err() != nil {
			skipped++
		}
		return nil
	}
	err = walk(rel)
	if err != nil && !errors.Is(err, stop) {
		return "", err
	}
	if len(lines) == 0 {
		lines = append(lines, "(no matches in scanned text)")
	}
	if partial {
		lines = append(lines, "[partial search: limit reached; narrow path or pattern]")
	}
	if skipped > 0 {
		lines = append(lines, fmt.Sprintf("[skipped %d symlink, special, binary, oversized file or line entries]", skipped))
	}
	return strings.Join(lines, "\n"), nil
}
