package harness

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// TailFile returns the last `n` lines of `path` joined with newlines.
// If the file has fewer than `n` lines, the entire contents are returned.
func TailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var all []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if len(all) == 0 {
		return "", nil
	}
	start := len(all) - n
	if start < 0 {
		start = 0
	}
	return strings.Join(all[start:], "\n"), nil
}

// GrepFile returns the first line of `path` that matches `pattern` (compiled
// as a Go regexp). Empty pattern matches every line (returns the first line).
func GrepFile(path, pattern string) (string, error) {
	re, err := compilePattern(pattern)
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if re.MatchString(sc.Text()) {
			return sc.Text(), nil
		}
	}
	return "", sc.Err()
}

// GrepCountFile returns the number of lines in `path` matching `pattern`.
func GrepCountFile(path, pattern string) (int, error) {
	re, err := compilePattern(pattern)
	if err != nil {
		return 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	count := 0
	for sc.Scan() {
		if re.MatchString(sc.Text()) {
			count++
		}
	}
	return count, sc.Err()
}

// WaitForLogMatch polls `path` until a line matches `pattern` or `timeout`
// elapses.
func WaitForLogMatch(ctx context.Context, path, pattern string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if line, _ := GrepFile(path, pattern); line != "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for log match %q in %s after %s", pattern, path, timeout)
}

func compilePattern(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return regexp.Compile(`.`)
	}
	return regexp.Compile(pattern)
}
