package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eldridgeh/dubd/internal/player"
)

type fakePlayer struct {
	metric int
	source string
	paused bool
	nexts  int
}

func (f *fakePlayer) Next() { f.nexts++ }

func (f *fakePlayer) SetMetric(v int, source string) { f.metric, f.source = v, source }
func (f *fakePlayer) SetPaused(paused bool)          { f.paused = paused }
func (f *fakePlayer) Status() player.Status {
	return player.Status{Metric: f.metric, Paused: f.paused, Phase: "playing"}
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

func TestMetric(t *testing.T) {
	p := &fakePlayer{}
	h := New(p, "test")
	for _, method := range []string{"PUT", "POST"} {
		p.metric = 0
		w := do(t, h, method, "/metric", "7\n")
		if w.Code != 200 || p.metric != 7 || p.source != "http" {
			t.Fatalf("%s /metric: code %d metric %d", method, w.Code, p.metric)
		}
		var info Info
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil || info.Metric != 7 || info.Version != "test" {
			t.Fatalf("response = %s (%v)", w.Body, err)
		}
	}
	if w := do(t, h, "PUT", "/metric", "fast"); w.Code != 400 || p.metric != 7 {
		t.Fatalf("bad body: code %d metric %d", w.Code, p.metric)
	}
	if w := do(t, h, "GET", "/metric", ""); strings.TrimSpace(w.Body.String()) != "7" {
		t.Fatalf("GET /metric = %q", w.Body)
	}
}

func TestPausePlayStatus(t *testing.T) {
	p := &fakePlayer{}
	h := New(p, "test")
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

func TestWebPagesAreRefused(t *testing.T) {
	p := &fakePlayer{}
	w := do(t, New(p, "test"), "POST", "/metric", "99", "Origin", "https://example.com")
	if w.Code != http.StatusForbidden || p.metric != 0 {
		t.Fatalf("code %d metric %d", w.Code, p.metric)
	}
}
