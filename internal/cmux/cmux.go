// Package cmux drives a cmux browser surface through the cmux CLI.
package cmux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ErrUnavailable means cmux itself cannot be reached: it is not running, or
// it refused the connection. Opening another surface will not help.
var ErrUnavailable = errors.New("cmux unavailable")

// ErrScript means cmux reached the page but the script failed there.
var ErrScript = errors.New("script failed in page")

// AppBin is where the cmux app bundle keeps its CLI.
const AppBin = "/Applications/cmux.app/Contents/Resources/bin/cmux"

// Runner runs the cmux CLI and returns its stdout and stderr.
type Runner func(ctx context.Context, bin string, args ...string) (stdout, stderr []byte, err error)

// Client runs cmux CLI commands.
type Client struct {
	Bin     string
	Timeout time.Duration
	// DialogDelay is how long Goto waits before it looks for a dialog
	// that is holding the navigation up.
	DialogDelay time.Duration
	// OnDialog, when set, is called after a dialog was accepted.
	OnDialog func(surface string)
	Run      Runner
}

// Find locates the cmux CLI: the explicit path if given, then $PATH, then
// the app bundle.
func Find(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("cmux CLI not found at %s", explicit)
		}
		return explicit, nil
	}
	if p, err := exec.LookPath("cmux"); err == nil {
		return p, nil
	}
	if _, err := os.Stat(AppBin); err == nil {
		return AppBin, nil
	}
	return "", errors.New("cmux CLI not found on PATH or in /Applications/cmux.app; install cmux or pass --cmux")
}

// New returns a client for the CLI at bin.
func New(bin string) *Client {
	return &Client{Bin: bin, Timeout: 25 * time.Second, DialogDelay: time.Second, Run: execRunner}
}

func execRunner(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// call runs one CLI command with --json and decodes the JSON object it
// prints. Anything else the CLI prints is reported as an error.
func (c *Client) call(ctx context.Context, args ...string) (map[string]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	stdout, stderr, runErr := c.Run(ctx, c.Bin, append([]string{"--json"}, args...)...)
	out := bytes.TrimSpace(stdout)
	var obj map[string]json.RawMessage
	if runErr == nil && json.Unmarshal(out, &obj) == nil {
		return obj, nil
	}
	msg := strings.TrimSpace(string(out) + "\n" + string(bytes.TrimSpace(stderr)))
	if msg == "" && runErr != nil {
		msg = runErr.Error()
	}
	return nil, classify(msg)
}

func classify(msg string) error {
	msg = strings.TrimPrefix(msg, "Error: ")
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "only processes started inside cmux"):
		return fmt.Errorf("%w: cmux only accepts commands from processes started in one of its own terminals; start dubd from a cmux terminal tab", ErrUnavailable)
	case strings.Contains(low, "no live cmux socket"):
		return fmt.Errorf("%w: cmux is not running", ErrUnavailable)
	case strings.Contains(low, "js_error"):
		return fmt.Errorf("%w: %s", ErrScript, msg)
	}
	return errors.New(firstLine(msg))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// Check reports whether cmux accepts commands from this process.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.call(ctx, "identify")
	return err
}

var surfaceText = regexp.MustCompile(`surface=(\S+)`)

// Open opens url in a new browser surface, without taking focus, and
// returns the surface handle.
func (c *Client) Open(ctx context.Context, url string) (string, error) {
	obj, err := c.call(ctx, "browser", "open", url, "--focus", "false")
	if err != nil {
		return "", err
	}
	for _, key := range []string{"surface_id", "surface_ref", "surface"} {
		var s string
		if json.Unmarshal(obj[key], &s) == nil && s != "" {
			return s, nil
		}
	}
	return "", errors.New("cmux opened a browser but did not report its surface")
}

// Goto navigates the surface to url. A page that is playing music asks
// "Leave site?" before it lets go, and cmux holds the navigation until that
// is answered, so Goto answers it while the navigation is under way.
func (c *Client) Goto(ctx context.Context, surface, url string) error {
	done := make(chan error, 1)
	go func() {
		_, err := c.call(ctx, "browser", "--surface", surface, "goto", url)
		done <- err
	}()
	wait := c.DialogDelay
	for {
		select {
		case err := <-done:
			return err
		case <-time.After(wait):
			_, _ = c.AcceptDialog(ctx, surface)
			wait = 2 * c.DialogDelay
		}
	}
}

// AcceptDialog accepts the dialog the surface's page is showing, such as
// "Leave site?", and reports whether there was one.
func (c *Client) AcceptDialog(ctx context.Context, surface string) (bool, error) {
	_, err := c.call(ctx, "browser", "--surface", surface, "dialog", "accept")
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no pending dialog") {
		return false, nil
	}
	if err == nil && c.OnDialog != nil {
		c.OnDialog(surface)
	}
	return err == nil, err
}

// Close closes the surface.
func (c *Client) Close(ctx context.Context, surface string) error {
	_, err := c.call(ctx, "close-surface", "--surface", surface)
	return err
}

// Eval runs script in the surface's page and decodes its result into out.
// A script that returns a JSON string has that string decoded.
func (c *Client) Eval(ctx context.Context, surface, script string, out any) error {
	obj, err := c.call(ctx, "browser", "--surface", surface, "eval", "--script", script)
	if err != nil {
		return err
	}
	value := obj["value"]
	if len(value) == 0 {
		return errors.New("cmux eval returned no value")
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		value = json.RawMessage(text)
	}
	if err := json.Unmarshal(value, out); err != nil {
		return fmt.Errorf("unexpected eval result: %s", firstLine(string(value)))
	}
	return nil
}
