package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eldridgeh/dubd/internal/metric"

	"github.com/eldridgeh/dubd/internal/player"
)

type fakePlayer struct {
	paused bool
	nexts  int
	stream player.Stream
}

func (f *fakePlayer) Next()                     { f.nexts++ }
func (f *fakePlayer) SetStream(s player.Stream) { f.stream = s }

func (f *fakePlayer) SetPaused(paused bool) { f.paused = paused }
func (f *fakePlayer) Status() player.Status {
	return player.Status{Paused: f.paused, Phase: "playing"}
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
	return New(p, board, "test"), board, &changes
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
