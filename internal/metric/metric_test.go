package metric

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]int{"7": 7, " 12\n": 12, "3 landings/hour": 3, "-2": -2} {
		if got, err := Parse(in); err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "  \n", "seven", "1.5"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q): expected an error", in)
		}
	}
}

func TestCommand(t *testing.T) {
	if v, err := Command("echo 9")(context.Background()); err != nil || v != 9 {
		t.Fatalf("Command = %d, %v", v, err)
	}
	if _, err := Command("echo nope >&2; exit 3")(context.Background()); err == nil {
		t.Fatal("expected an error from a failing command")
	}
}

func TestFileAndPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "landings")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	values, failures := make(chan int, 8), make(chan error, 8)
	go Poll(ctx, File(path), 10*time.Millisecond, func(v int) { values <- v }, func(err error) { failures <- err })

	select {
	case <-failures: // the file does not exist yet
	case <-time.After(2 * time.Second):
		t.Fatal("no failure reported for a missing file")
	}
	if err := os.WriteFile(path, []byte("4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-values:
		if v != 4 {
			t.Fatalf("value = %d", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no value read from the file")
	}
}
