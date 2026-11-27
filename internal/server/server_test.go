package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eldridgeh/dubd/internal/library"
	"github.com/eldridgeh/dubd/internal/metric"
	"github.com/eldridgeh/dubd/internal/player"
)

type fakePlayer struct {
	paused bool
	nexts  int
	stream player.Stream

	searched  []string
	found     []player.Track
	searchErr error
}

func (f *fakePlayer) Search(_ context.Context, query string) ([]player.Track, error) {
	f.searched = append(f.searched, query)
	return f.found, f.searchErr
}

func (f *fakePlayer) Next()                     { f.nexts++ }
func (f *fakePlayer) SetStream(s player.Stream) { f.stream = s }

func (f *fakePlayer) SetPaused(paused bool) { f.paused = paused }
func (f *fakePlayer) Status() player.Status {
	return player.Status{Paused: f.paused, Phase: "playing", Stream: f.stream}
}

func do(t *testing.T, h http.Handler, method, path, body string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// newAPI returns the handler with a board whose changes are recorded.
func newAPI(p *fakePlayer) (http.Handler, *metric.Board, *[]string) {
	board := metric.NewBoard(0, metric.Sum, 0, time.Hour)
	var changes []string
	board.OnChange = func(total int, source string) { changes = append(changes, fmt.Sprintf("%d %s", total, source)) }
	lib, err := library.Open("")
	if err != nil {
		panic(err)
	}
	return New(p, board, lib, "test"), board, &changes
}

func TestMetric(t *testing.T) {
	h, board, changes := newAPI(&fakePlayer{})
	for _, method := range []string{"PUT", "POST"} {
		board.Set(0, "test")
		w := do(t, h, method, "/metric", "7\n")
		if w.Code != 200 || board.Total() != 7 || (*changes)[len(*changes)-1] != "7 http" {
			t.Fatalf("%s /metric: code %d total %d changes %v", method, w.Code, board.Total(), *changes)
		}
		var info Info
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil || info.Version != "test" || info.Counting.Mode != "total" {
			t.Fatalf("response = %s (%v)", w.Body, err)
		}
	}
	if w := do(t, h, "PUT", "/metric", "fast"); w.Code != 400 || board.Total() != 7 {
		t.Fatalf("bad body: code %d total %d", w.Code, board.Total())
	}
	if w := do(t, h, "GET", "/metric", ""); strings.TrimSpace(w.Body.String()) != "7" {
		t.Fatalf("GET /metric = %q", w.Body)
	}
}

func TestAgentsOverHTTP(t *testing.T) {
	h, board, _ := newAPI(&fakePlayer{})
	do(t, h, "PUT", "/metric", "50")
	for agent, v := range map[string]string{"agent-1": "2", "agent-2": "3"} {
		if w := do(t, h, "PUT", "/metric/"+agent, v); w.Code != 200 {
			t.Fatalf("PUT /metric/%s = %d %s", agent, w.Code, w.Body)
		}
	}
	w := do(t, h, "GET", "/status", "")
	var info Info
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if board.Total() != 5 || info.Counting.Mode != "agents" || len(info.Counting.Agents) != 2 || info.Counting.Agents[1].Value != 3 {
		t.Fatalf("total %d counting %+v", board.Total(), info.Counting)
	}
	if w := do(t, h, "PUT", "/metric/bad%20name", "1"); w.Code != 400 {
		t.Fatalf("bad agent name = %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/metric/agent-1", ""); w.Code != 200 || board.Total() != 3 {
		t.Fatalf("DELETE: code %d total %d", w.Code, board.Total())
	}
	if w := do(t, h, "DELETE", "/metric/agent-1", ""); w.Code != 404 {
		t.Fatalf("DELETE of an unknown agent = %d", w.Code)
	}
}

func TestLandingsOverHTTP(t *testing.T) {
	h, board, _ := newAPI(&fakePlayer{})
	do(t, h, "PUT", "/metric", "50")
	if w := do(t, h, "POST", "/land", ""); w.Code != 200 || board.Total() != 1 {
		t.Fatalf("POST /land: code %d total %d", w.Code, board.Total())
	}
	if w := do(t, h, "POST", "/land/agent-1", "3"); w.Code != 200 || board.Total() != 4 || board.Mode() != "landings" {
		t.Fatalf("POST /land/agent-1: code %d total %d mode %s", w.Code, board.Total(), board.Mode())
	}
	for _, body := range []string{"0", "x", "5000"} {
		if w := do(t, h, "POST", "/land", body); w.Code != 400 || board.Total() != 4 {
			t.Fatalf("POST /land %q: code %d total %d", body, w.Code, board.Total())
		}
	}
}

func TestPausePlayStatus(t *testing.T) {
	p := &fakePlayer{}
	h, _, _ := newAPI(p)
	if do(t, h, "POST", "/pause", ""); !p.paused {
		t.Fatal("not paused")
	}
	if do(t, h, "POST", "/play", ""); p.paused {
		t.Fatal("still paused")
	}
	if w := do(t, h, "GET", "/status", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"phase": "playing"`) {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	if do(t, h, "POST", "/next", ""); p.nexts != 1 {
		t.Fatal("next not passed on")
	}
	if w := do(t, h, "GET", "/pause", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /pause = %d", w.Code)
	}
}

func TestStream(t *testing.T) {
	p := &fakePlayer{}
	h, _, _ := newAPI(p)
	if w := do(t, h, "PUT", "/stream", "heavy dub techno\n"); w.Code != 200 || p.stream.Query != "heavy dub techno" {
		t.Fatalf("search words: code %d stream %+v", w.Code, p.stream)
	}
	if w := do(t, h, "POST", "/stream", "https://music.youtube.com/playlist?list=PL123"); w.Code != 200 || p.stream.URL != "https://music.youtube.com/watch?list=PL123" {
		t.Fatalf("link: code %d stream %+v", w.Code, p.stream)
	}
	if w := do(t, h, "PUT", "/stream", "https://example.com/x"); w.Code != 400 || p.stream.URL != "https://music.youtube.com/watch?list=PL123" {
		t.Fatalf("foreign link: code %d stream %+v", w.Code, p.stream)
	}
}

func TestWebPagesAreRefused(t *testing.T) {
	h, board, _ := newAPI(&fakePlayer{})
	w := do(t, h, "POST", "/metric", "99", "Origin", "https://example.com")
	if w.Code != http.StatusForbidden || board.Total() != 0 {
		t.Fatalf("code %d total %d", w.Code, board.Total())
	}
}

func TestSearchThenPickByNumber(t *testing.T) {
	p := &fakePlayer{found: []player.Track{{ID: "aaaaaaaaaaa", Title: "One", By: "A"}, {ID: "bbbbbbbbbbb", Title: "Two"}}}
	h, _, _ := newAPI(p)

	if w := do(t, h, "PUT", "/stream", "2"); w.Code != 400 || !strings.Contains(w.Body.String(), "dubd search") {
		t.Fatalf("a number before any search: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/search", "  "); w.Code != 400 || len(p.searched) != 0 {
		t.Fatalf("an empty search: %d, searched %q", w.Code, p.searched)
	}
	w := do(t, h, "POST", "/search", "heavy dub techno\n")
	var r library.Results
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || w.Code != 200 {
		t.Fatalf("POST /search: %d %s (%v)", w.Code, w.Body, err)
	}
	if p.searched[0] != "heavy dub techno" || r.Query != "heavy dub techno" || len(r.Tracks) != 2 ||
		r.Tracks[0].Link != "https://music.youtube.com/watch?v=aaaaaaaaaaa" {
		t.Fatalf("searched %q, results %+v", p.searched, r)
	}
	if w := do(t, h, "GET", "/search", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"title": "Two"`) {
		t.Fatalf("GET /search: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/stream", "2"); w.Code != 200 || p.stream.URL != "https://music.youtube.com/watch?v=bbbbbbbbbbb&list=RDAMVMbbbbbbbbbbb" {
		t.Fatalf("pick 2: %d, stream %+v", w.Code, p.stream)
	}
	if w := do(t, h, "PUT", "/stream", "3"); w.Code != 400 {
		t.Fatalf("pick 3 of 2: %d", w.Code)
	}

	// A search that fails keeps the results there were.
	p.searchErr = errors.New("the search page showed no tracks")
	if w := do(t, h, "POST", "/search", "nothing"); w.Code != 502 {
		t.Fatalf("failed search: %d", w.Code)
	}
	if w := do(t, h, "GET", "/search", ""); !strings.Contains(w.Body.String(), "heavy dub techno") {
		t.Fatalf("results after a failed search: %s", w.Body)
	}
}

func TestSavedStreams(t *testing.T) {
	p := &fakePlayer{stream: player.Stream{Query: "ai dub reggae"}}
	h, _, _ := newAPI(p)

	// No body saves what is playing; a body saves what it names.
	if w := do(t, h, "PUT", "/saved/usual", ""); w.Code != 200 {
		t.Fatalf("save current: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/saved/mine", "https://music.youtube.com/playlist?list=PL1"); w.Code != 200 {
		t.Fatalf("save a link: %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{"/saved/3", "/saved/has%20space"} {
		if w := do(t, h, "PUT", bad, ""); w.Code != 400 {
			t.Errorf("PUT %s = %d, want 400", bad, w.Code)
		}
	}
	if w := do(t, h, "PUT", "/saved/x", "https://example.com/"); w.Code != 400 {
		t.Fatalf("save a link that is not YouTube: %d", w.Code)
	}
	var saved []library.Named
	w := do(t, h, "GET", "/saved", "")
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || len(saved) != 2 ||
		saved[0] != (library.Named{Name: "mine", Stream: player.Stream{URL: "https://music.youtube.com/watch?list=PL1"}}) ||
		saved[1].Stream.Query != "ai dub reggae" {
		t.Fatalf("GET /saved = %s (%v)", w.Body, err)
	}

	// A saved name plays what was saved, ahead of a search for the word.
	if w := do(t, h, "PUT", "/stream", "mine"); w.Code != 200 || p.stream.URL != "https://music.youtube.com/watch?list=PL1" {
		t.Fatalf("play a saved name: %d, stream %+v", w.Code, p.stream)
	}
	if w := do(t, h, "PUT", "/stream", "mine too"); w.Code != 200 || p.stream.Query != "mine too" {
		t.Fatalf("search words that start with a saved name: stream %+v", p.stream)
	}
	if w := do(t, h, "DELETE", "/saved/mine", ""); w.Code != 200 {
		t.Fatalf("unsave: %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/saved/mine", ""); w.Code != 404 {
		t.Fatalf("unsave twice: %d", w.Code)
	}
	if w := do(t, h, "PUT", "/stream", "mine"); p.stream.Query != "mine" {
		t.Fatalf("after unsave the name is search words again: %d, stream %+v", w.Code, p.stream)
	}
	if w := do(t, h, "GET", "/history", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("GET /history: %d %s", w.Code, w.Body)
	}
}
