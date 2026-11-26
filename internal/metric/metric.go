// Package metric reads the integer metric from a command or a file.
package metric

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Parse reads an integer from the first word of text.
func Parse(text string) (int, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0, errors.New("no value")
	}
	v, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, fmt.Errorf("%q is not an integer", fields[0])
	}
	return v, nil
}

// Source reads the current metric value.
type Source func(ctx context.Context) (int, error)

// Command returns a source that runs a shell command and parses its output.
func Command(command string) Source {
	return func(ctx context.Context) (int, error) {
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return Parse(stdout.String())
	}
}

// File returns a source that reads the value from a file.
func File(path string) Source {
	return func(context.Context) (int, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		return Parse(string(b))
	}
}

// Poll reads src now and then every interval until ctx ends, passing each
// value to set and each failure to fail.
func Poll(ctx context.Context, src Source, interval time.Duration, set func(int), fail func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		readCtx, cancel := context.WithTimeout(ctx, interval)
		v, err := src(readCtx)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			fail(err)
		default:
			set(v)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
