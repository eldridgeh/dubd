package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFlagsFirst(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{[]string{"3", "-from", "a"}, []string{"-from", "a", "3"}},
		{[]string{"-from", "a", "3"}, []string{"-from", "a", "3"}},
		{[]string{"7", "-addr", "127.0.0.1:8401"}, []string{"-addr", "127.0.0.1:8401", "7"}},
		{[]string{"-addr=127.0.0.1:1", "7"}, []string{"-addr=127.0.0.1:1", "7"}},
		{[]string{"-2"}, []string{"-2"}}, // a negative metric is a value, not a flag
		{[]string{"-from", "a", "--", "-2"}, []string{"-from", "a", "--", "-2"}},
		{nil, nil},
	} {
		if got := flagsFirst(tc.in); !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
			t.Errorf("flagsFirst(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAddrDefault(t *testing.T) {
	t.Setenv("DUBD_ADDR", "")
	if got := addrDefault(); got != defaultAddr {
		t.Fatalf("without DUBD_ADDR: %q", got)
	}
	t.Setenv("DUBD_ADDR", "127.0.0.1:9001")
	if got := addrDefault(); got != "127.0.0.1:9001" {
		t.Fatalf("with DUBD_ADDR: %q", got)
	}
}

func TestSurfaceFilePerInstance(t *testing.T) {
	plain, a, b := surfaceFile(""), surfaceFile("alpha"), surfaceFile("beta")
	if plain == "" {
		t.Skip("no user cache directory here")
	}
	if filepath.Base(plain) != "surface" || filepath.Base(a) != "surface.alpha" || a == b || a == plain {
		t.Fatalf("files: %q %q %q", plain, a, b)
	}
	if filepath.Dir(a) != filepath.Dir(plain) {
		t.Fatalf("named instance file is outside the dubd directory: %q", a)
	}
}

func TestInstanceNames(t *testing.T) {
	for _, ok := range []string{"alpha", "team-2", "army_3.east", "A1"} {
		if !nameRE.MatchString(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", ".hidden", "has space", "-dash"} {
		if nameRE.MatchString(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestRunRefusesABadName(t *testing.T) {
	if err := run([]string{"-name", "../escape"}); err == nil {
		t.Fatal("expected an error for a name that is a path")
	}
}

func TestLibraryFilePerInstance(t *testing.T) {
	plain, a, b := libraryFile(""), libraryFile("alpha"), libraryFile("beta")
	if plain == "" {
		t.Skip("no user configuration directory here")
	}
	if filepath.Base(plain) != "library.json" || filepath.Base(a) != "library.alpha.json" || a == b {
		t.Fatalf("files: %q %q %q", plain, a, b)
	}
}

func TestPrintResults(t *testing.T) {
	out, err := printResults([]byte(`{"query":"heavy dub","tracks":[{"id":"aaaaaaaaaaa","title":"One","by":"Video • A"},{"id":"bbbbbbbbbbb","title":"Two"}]}`))
	want := "Results for \"heavy dub\":\n  1  One — Video • A\n  2  Two\nPlay one with: dubd stream <number>\n"
	if err != nil || string(out) != want {
		t.Fatalf("printResults = %q, %v", out, err)
	}
	if out, err := printResults([]byte(`{"query":"","tracks":[]}`)); err != nil || !strings.Contains(string(out), "dubd search <words>") {
		t.Fatalf("no results: %q, %v", out, err)
	}
	if _, err := printResults([]byte("not json")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestPrintHistory(t *testing.T) {
	at := time.Date(2026, 11, 26, 18, 30, 0, 0, time.Local)
	raw := []byte(`[{"at":"` + at.Format(time.RFC3339) + `","id":"aaaaaaaaaaa","title":"One","by":"A","link":"https://music.youtube.com/watch?v=aaaaaaaaaaa"},` +
		`{"at":"` + at.Add(time.Hour).Format(time.RFC3339) + `","id":"bbbbbbbbbbb","title":"Two","link":"https://music.youtube.com/watch?v=bbbbbbbbbbb"}]`)
	out, err := printHistory(raw, 0)
	want := "2026-11-26 18:30  One — A\n                  https://music.youtube.com/watch?v=aaaaaaaaaaa\n" +
		"2026-11-26 19:30  Two\n                  https://music.youtube.com/watch?v=bbbbbbbbbbb\n"
	if err != nil || string(out) != want {
		t.Fatalf("printHistory = %q, %v", out, err)
	}
	if out, _ := printHistory(raw, 1); strings.Contains(string(out), "One") || !strings.Contains(string(out), "Two") {
		t.Fatalf("the latest one = %q", out)
	}
	if out, _ := printHistory([]byte("[]"), 20); !strings.Contains(string(out), "Nothing has played") {
		t.Fatalf("empty = %q", out)
	}
}

func TestPrintSaved(t *testing.T) {
	out, err := printSaved([]byte(`[{"name":"a","stream":{"url":"https://music.youtube.com/watch?list=PL1"}},{"name":"heavy","stream":{"query":"heavy dub"}}]`))
	want := "a      https://music.youtube.com/watch?list=PL1\nheavy  search: heavy dub\n"
	if err != nil || string(out) != want {
		t.Fatalf("printSaved = %q, %v", out, err)
	}
}

// TestLibraryCommands checks what each command sends to the daemon.
func TestLibraryCommands(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 256)
		n, _ := r.Body.Read(body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(body[:n]))
		switch {
		case strings.HasPrefix(r.URL.Path, "/search"):
			w.Write([]byte(`{"query":"q","tracks":[]}`))
		case r.URL.Path == "/stream":
			w.Write([]byte(`{}`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	for _, tc := range []struct {
		cmd  string
		args []string
		want string
	}{
		{"search", []string{"-addr", addr, "heavy", "dub", "techno"}, "POST /search heavy dub techno"},
		{"search", []string{"-addr", addr}, "GET /search "},
		{"stream", []string{"-addr", addr, "3"}, "PUT /stream 3"},
		{"save", []string{"-addr", addr, "chill"}, "PUT /saved/chill "},
		{"save", []string{"-addr", addr, "chill", "heavy", "dub"}, "PUT /saved/chill heavy dub"},
		{"saved", []string{"-addr", addr}, "GET /saved "},
		{"unsave", []string{"chill", "-addr", addr}, "DELETE /saved/chill "},
		{"history", []string{"-n", "5", "-addr", addr}, "GET /history "},
	} {
		got = nil
		if err := client(tc.args, tc.cmd); err != nil {
			t.Errorf("dubd %s %q: %v", tc.cmd, tc.args, err)
		} else if len(got) != 1 || got[0] != tc.want {
			t.Errorf("dubd %s %q sent %q, want %q", tc.cmd, tc.args, got, tc.want)
		}
	}
	for _, tc := range []struct {
		cmd  string
		args []string
	}{
		{"save", []string{"-addr", addr}},
		{"save", []string{"-addr", addr, "3"}},
		{"unsave", []string{"-addr", addr}},
		{"history", []string{"-addr", addr, "extra"}},
		{"history", []string{"-addr", addr, "-n", "-1"}},
	} {
		got = nil
		if err := client(tc.args, tc.cmd); err == nil || len(got) != 0 {
			t.Errorf("dubd %s %q: err %v, sent %q; want a refusal before anything is sent", tc.cmd, tc.args, err, got)
		}
	}
}
