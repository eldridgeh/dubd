package library

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eldridgeh/dubd/internal/player"
)

func open(t *testing.T, path string) *Library {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestKeptAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "library.json")
	l := open(t, path)
	at := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return at }

	tracks := []player.Track{{ID: "aaaaaaaaaaa", Title: "One", By: "A"}, {ID: "bbbbbbbbbbb", Title: "Two"}}
	kept, err := l.SetResults("heavy dub", tracks)
	if err != nil {
		t.Fatal(err)
	}
	// What comes back is this search, whatever is recorded after it.
	if _, err := l.SetResults("another", nil); err != nil {
		t.Fatal(err)
	}
	if kept.Query != "heavy dub" || len(kept.Tracks) != 2 || kept.Tracks[0].Link == "" {
		t.Fatalf("SetResults returned %+v", kept)
	}
	if _, err := l.SetResults("heavy dub", tracks); err != nil {
		t.Fatal(err)
	}
	if err := l.Played(tracks[0]); err != nil {
		t.Fatal(err)
	}
	if err := l.Save("heavy", player.Stream{Query: "heavy dub"}); err != nil {
		t.Fatal(err)
	}

	again := open(t, path)
	r := again.Results()
	if r.Query != "heavy dub" || !r.At.Equal(at) || len(r.Tracks) != 2 ||
		r.Tracks[1] != (player.Track{ID: "bbbbbbbbbbb", Title: "Two", Link: "https://music.youtube.com/watch?v=bbbbbbbbbbb"}) {
		t.Fatalf("results = %+v", r)
	}
	h := again.History()
	if len(h) != 1 || h[0].Title != "One" || h[0].Link != "https://music.youtube.com/watch?v=aaaaaaaaaaa" || !h[0].At.Equal(at) {
		t.Fatalf("history = %+v", h)
	}
	if s := again.Saved(); len(s) != 1 || s[0] != (Named{Name: "heavy", Stream: player.Stream{Query: "heavy dub"}}) {
		t.Fatalf("saved = %+v", s)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*")); len(left) != 1 {
		t.Fatalf("temporary files left behind: %q", left)
	}
}

func TestUnreadableFileIsNotWrittenOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "not a dubd library") {
		t.Fatalf("Open = %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{not json" {
		t.Fatalf("file changed: %q", raw)
	}
}

func TestHistory(t *testing.T) {
	l := open(t, "")
	a, b := player.Track{ID: "aaaaaaaaaaa", Title: "A"}, player.Track{ID: "bbbbbbbbbbb", Title: "B"}
	for _, tr := range []player.Track{a, a, b, a} {
		if err := l.Played(tr); err != nil {
			t.Fatal(err)
		}
	}
	if h := l.History(); len(h) != 3 || h[0].ID != a.ID || h[1].ID != b.ID || h[2].ID != a.ID {
		t.Fatalf("history = %+v", h)
	}
	for i := 0; i < MaxHistory+10; i++ {
		_ = l.Played(player.Track{ID: fmt.Sprintf("%011d", i), Title: "T"})
	}
	h := l.History()
	if len(h) != MaxHistory || h[len(h)-1].ID != fmt.Sprintf("%011d", MaxHistory+9) || h[0].ID != fmt.Sprintf("%011d", 10) {
		t.Fatalf("history holds %d, from %s to %s", len(h), h[0].ID, h[len(h)-1].ID)
	}
}

func TestSaveAndUnsave(t *testing.T) {
	l := open(t, filepath.Join(t.TempDir(), "library.json"))
	for _, bad := range []string{"", "3", "-7", "has space", "../x", ".hidden"} {
		if err := l.Save(bad, player.Stream{Query: "x"}); err == nil {
			t.Errorf("name %q should be refused", bad)
		}
	}
	if err := l.Save("empty", player.Stream{}); err == nil {
		t.Error("an empty stream should be refused")
	}
	if err := l.Save("b", player.Stream{Query: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Save("b", player.Stream{Query: "two"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Save("a", player.Stream{URL: "https://music.youtube.com/watch?list=PL1"}); err != nil {
		t.Fatal(err)
	}
	s := l.Saved()
	if len(s) != 2 || s[0].Name != "a" || s[1].Stream.Query != "two" {
		t.Fatalf("saved = %+v", s)
	}
	if ok, err := l.Unsave("b"); !ok || err != nil {
		t.Fatalf("Unsave = %v, %v", ok, err)
	}
	if ok, _ := l.Unsave("b"); ok {
		t.Fatal("removed a stream that was not there")
	}
	if len(l.Saved()) != 1 {
		t.Fatalf("saved = %+v", l.Saved())
	}
}

func TestSaveThatCannotBeWrittenIsNotKept(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "sub")
	l := open(t, filepath.Join(blocker, "library.json"))
	// The library's directory cannot be made: a file is in the way.
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Save("a", player.Stream{Query: "x"}); err == nil {
		t.Fatal("expected an error")
	}
	if len(l.Saved()) != 0 {
		t.Fatalf("kept a stream that was not written: %+v", l.Saved())
	}
}

func TestResolve(t *testing.T) {
	l := open(t, "")
	if _, err := l.Resolve("2"); err == nil || !strings.Contains(err.Error(), "dubd search") {
		t.Fatalf("a number with no search: %v", err)
	}
	_, _ = l.SetResults("q", []player.Track{{ID: "aaaaaaaaaaa", Title: "One"}, {ID: "bbbbbbbbbbb", Title: "Two"}})
	_ = l.Save("chill", player.Stream{URL: "https://music.youtube.com/watch?list=PL1"})

	for text, want := range map[string]player.Stream{
		"2":            {URL: "https://music.youtube.com/watch?v=bbbbbbbbbbb&list=RDAMVMbbbbbbbbbbb"},
		" 1\n":         {URL: "https://music.youtube.com/watch?v=aaaaaaaaaaa&list=RDAMVMaaaaaaaaaaa"},
		"chill":        {URL: "https://music.youtube.com/watch?list=PL1"},
		"chill dub":    {Query: "chill dub"},
		"heavy techno": {Query: "heavy techno"},
		"https://music.youtube.com/playlist?list=PL9": {URL: "https://music.youtube.com/watch?list=PL9"},
	} {
		if got, err := l.Resolve(text); err != nil || got != want {
			t.Errorf("Resolve(%q) = %+v, %v; want %+v", text, got, err, want)
		}
	}
	for _, bad := range []string{"0", "3", "-1", "", "https://example.com/x"} {
		if got, err := l.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) = %+v, want an error", bad, got)
		}
	}
}

func TestManyWritersAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.json")
	l := open(t, path)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = l.Played(player.Track{ID: fmt.Sprintf("%011d", i), Title: "T"})
			_ = l.Save(fmt.Sprintf("s%d", i), player.Stream{Query: "x"})
			_, _ = l.SetResults("q", []player.Track{{ID: "aaaaaaaaaaa", Title: "One"}})
			_, _ = l.Resolve("1")
			_ = l.History()
			_ = l.Saved()
		}(i)
	}
	wg.Wait()
	again := open(t, path)
	if len(again.History()) != 20 || len(again.Saved()) != 20 {
		t.Fatalf("after 20 writers: %d played, %d saved", len(again.History()), len(again.Saved()))
	}
}
