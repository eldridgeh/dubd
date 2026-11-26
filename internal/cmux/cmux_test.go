package cmux

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type call struct {
	args   []string
	stdout string
	stderr string
	err    error
}

func fake(t *testing.T, calls ...call) *Client {
	t.Helper()
	i := 0
	return &Client{Bin: "cmux", Timeout: time.Second, DialogDelay: time.Hour, Run: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		if i >= len(calls) {
			t.Fatalf("unexpected call %v", args)
		}
		c := calls[i]
		i++
		if c.args != nil && !reflect.DeepEqual(args, c.args) {
			t.Fatalf("args = %q, want %q", args, c.args)
		}
		return []byte(c.stdout), []byte(c.stderr), c.err
	}}
}

func TestOpen(t *testing.T) {
	c := fake(t, call{
		args:   []string{"--json", "browser", "open", "https://example.com/", "--focus", "false"},
		stdout: `{"created_split":true,"pane_ref":"pane:2","surface_ref":"surface:7"}` + "\n",
	})
	got, err := c.Open(context.Background(), "https://example.com/")
	if err != nil || got != "surface:7" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestOpenPrefersID(t *testing.T) {
	c := fake(t, call{stdout: `{"surface_ref":"surface:7","surface_id":"83F4E6A4"}`})
	if got, _ := c.Open(context.Background(), "u"); got != "83F4E6A4" {
		t.Fatalf("Open = %q", got)
	}
}

func TestEval(t *testing.T) {
	type res struct {
		N int `json:"n"`
	}
	for name, stdout := range map[string]string{
		"json string": `{"surface_ref":"surface:7","value":"{\"n\":4}"}`,
		"object":      `{"surface_ref":"surface:7","value":{"n":4}}`,
	} {
		c := fake(t, call{
			args:   []string{"--json", "browser", "--surface", "surface:7", "eval", "--script", "x"},
			stdout: stdout,
		})
		var r res
		if err := c.Eval(context.Background(), "surface:7", "x", &r); err != nil || r.N != 4 {
			t.Errorf("%s: r = %+v, err = %v", name, r, err)
		}
	}
}

func TestErrors(t *testing.T) {
	exit := errors.New("exit status 1")
	for name, tc := range map[string]struct {
		c    call
		want error
	}{
		"denied":       {call{stderr: "Error: ERROR: Access denied - only processes started inside cmux can connect", err: exit}, ErrUnavailable},
		"denied exit0": {call{stdout: "Error: ERROR: Access denied - only processes started inside cmux can connect"}, ErrUnavailable},
		"not running":  {call{stderr: "Error: No live cmux socket found. Tried:\n  /tmp/cmux.sock", err: exit}, ErrUnavailable},
		"script":       {call{stderr: "Error: js_error: boom", err: exit}, ErrScript},
	} {
		c := fake(t, tc.c)
		var out any
		err := c.Eval(context.Background(), "s", "x", &out)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	c := fake(t, call{stderr: "Error: not_found: surface gone\nmore", err: exit})
	err := c.Goto(context.Background(), "s", "u")
	if err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrScript) || err.Error() != "not_found: surface gone" {
		t.Errorf("plain error = %v", err)
	}
}

func TestAcceptDialog(t *testing.T) {
	c := fake(t,
		call{args: []string{"--json", "browser", "--surface", "s", "dialog", "accept"}, stdout: `{"accepted":true,"dialog":{"type":"alert"}}`},
		call{stderr: "Error: not_found: No pending dialog", err: errors.New("exit status 1")},
		call{stderr: "Error: Command timed out", err: errors.New("exit status 1")},
	)
	if ok, err := c.AcceptDialog(context.Background(), "s"); !ok || err != nil {
		t.Fatalf("pending dialog: %v, %v", ok, err)
	}
	if ok, err := c.AcceptDialog(context.Background(), "s"); ok || err != nil {
		t.Fatalf("no dialog: %v, %v", ok, err)
	}
	if ok, err := c.AcceptDialog(context.Background(), "s"); ok || err == nil {
		t.Fatalf("failure: %v, %v", ok, err)
	}
}

func TestClose(t *testing.T) {
	c := fake(t, call{args: []string{"--json", "close-surface", "--surface", "s"}, stdout: `{}`})
	if err := c.Close(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
}

// A navigation held up by "Leave site?" completes once Goto answers it.
func TestGotoAnswersTheLeaveSiteDialog(t *testing.T) {
	answered := make(chan struct{})
	var once sync.Once
	c := &Client{Bin: "cmux", Timeout: 5 * time.Second, DialogDelay: 5 * time.Millisecond,
		Run: func(ctx context.Context, _ string, args ...string) ([]byte, []byte, error) {
			if strings.Contains(strings.Join(args, " "), "dialog accept") {
				once.Do(func() { close(answered) })
				return []byte(`{"accepted":true}`), nil, nil
			}
			select {
			case <-answered:
				return []byte(`{}`), nil, nil
			case <-ctx.Done():
				return nil, []byte("Error: Command timed out"), errors.New("exit status 1")
			}
		}}
	if err := c.Goto(context.Background(), "s", "https://example.com/"); err != nil {
		t.Fatalf("Goto = %v", err)
	}
}
