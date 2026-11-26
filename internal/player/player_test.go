package player

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/eldridgeh/dubd/internal/cmux"
	"github.com/eldridgeh/dubd/internal/tempo"
)

// fakeBrowser stands in for cmux and a YouTube Music page.
type fakeBrowser struct {
	opened []string
	gotos  []string
	url    string
	ids    []string // tracks the search page shows
	stuck  bool     // playback does not advance
	ad     bool     // an advert is showing
	gone   bool     // the surface no longer exists
	err    error    // every call fails with this
	pos    float64
	last   want
}

var wantRE = regexp.MustCompile(`const want = (\{.*?\});`)

func (f *fakeBrowser) Open(_ context.Context, url string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.opened = append(f.opened, url)
	f.url, f.gone = url, false
	return "surface:" + string(rune('0'+len(f.opened))), nil
}

func (f *fakeBrowser) Goto(_ context.Context, _ string, url string) error {
	if f.err != nil {
		return f.err
	}
	if f.gone {
		return errors.New("not_found: surface")
	}
	f.gotos = append(f.gotos, url)
	f.url, f.pos = url, 0
	return nil
}

func (f *fakeBrowser) Eval(_ context.Context, _ string, script string, out any) error {
	if f.err != nil {
		return f.err
	}
	if f.gone {
		return errors.New("not_found: surface")
	}
	var res any
	if m := wantRE.FindStringSubmatch(script); m != nil {
		if err := json.Unmarshal([]byte(m[1]), &f.last); err != nil {
			return err
		}
		watching := strings.Contains(f.url, "/watch")
		if watching && f.last.Play && !f.stuck {
			f.pos += f.last.Rate
		}
		res = page{Host: "music.youtube.com", Path: "/watch", Video: watching, Loaded: watching,
			Paused: !f.last.Play, Rate: f.last.Rate, Time: f.pos, Title: "Track", Ad: f.ad}
	} else {
		ids := f.ids
		if !strings.Contains(f.url, "/search") {
			ids = nil
		}
		res = found{IDs: ids}
	}
	b, _ := json.Marshal(res)
	return json.Unmarshal(b, out)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTest(t *testing.T, cfg Config, f *fakeBrowser, metric int) (*Player, *clock) {
	t.Helper()
	if cfg.Curve == (tempo.Curve{}) {
		cfg.Curve = tempo.Curve{MetricMin: 0, MetricMax: 10, RateMin: 0.5, RateMax: 1.5}
	}
	p := New(cfg, f, slog.New(slog.NewTextHandler(io.Discard, nil)), metric)
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	p.now = c.now
	p.metricAt = c.t
	return p, c
}

// steps runs n steps, one second apart.
func steps(p *Player, c *clock, n int) {
	for i := 0; i < n; i++ {
		p.Step(context.Background())
		c.t = c.t.Add(time.Second)
	}
}

func TestSearchThenRadio(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}}
	p, c := newTest(t, Config{Query: "ai dub reggae"}, f, 5)
	steps(p, c, 4)

	if len(f.opened) != 1 || f.opened[0] != "https://music.youtube.com/search?q=ai+dub+reggae" {
		t.Fatalf("opened = %q", f.opened)
	}
	if len(f.gotos) != 1 || f.gotos[0] != "https://music.youtube.com/watch?v=aaaaaaaaaaa&list=RDAMVMaaaaaaaaaaa" {
		t.Fatalf("gotos = %q", f.gotos)
	}
	st := p.Status()
	if st.Phase != phasePlaying || st.Rate != 1.0 || !f.last.Play || f.last.Rate != 1.0 || st.Title != "Track" {
		t.Fatalf("status = %+v, last want = %+v", st, f.last)
	}
}

func TestDirectURL(t *testing.T) {
	f := &fakeBrowser{}
	u := "https://music.youtube.com/watch?v=ccccccccccc&list=PL123"
	p, c := newTest(t, Config{URL: u}, f, 0)
	steps(p, c, 3)
	if len(f.opened) != 1 || f.opened[0] != u || len(f.gotos) != 0 || p.Status().Phase != phasePlaying {
		t.Fatalf("opened = %q gotos = %q", f.opened, f.gotos)
	}
}

func TestRateGlidesToMetric(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Query: "q", Glide: 0.1}, f, 0)
	steps(p, c, 3)
	if f.last.Rate != 0.5 {
		t.Fatalf("starting rate = %v", f.last.Rate)
	}
	p.SetMetric(10, "test")
	if got := p.Status().TargetRate; got != 1.5 {
		t.Fatalf("target = %v", got)
	}
	steps(p, c, 3)
	if f.last.Rate != 0.8 {
		t.Fatalf("rate after 3s = %v, want 0.8", f.last.Rate)
	}
	steps(p, c, 20)
	if f.last.Rate != 1.5 || p.Status().Rate != 1.5 {
		t.Fatalf("settled rate = %v", f.last.Rate)
	}
}

func TestKeepPitch(t *testing.T) {
	for _, keep := range []bool{true, false} {
		f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
		p, c := newTest(t, Config{Query: "q", KeepPitch: keep}, f, 0)
		steps(p, c, 3)
		if f.last.KeepPitch != keep {
			t.Errorf("keepPitch = %v, want %v", f.last.KeepPitch, keep)
		}
	}
}

func TestStallRestartsWithNextTrack(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}}
	p, c := newTest(t, Config{Query: "q", StallAfter: 10 * time.Second}, f, 5)
	steps(p, c, 5)
	f.stuck = true
	steps(p, c, 14)
	f.stuck = false
	steps(p, c, 3)
	want := []string{radioURL("aaaaaaaaaaa"), "https://music.youtube.com/search?q=q", radioURL("bbbbbbbbbbb")}
	if strings.Join(f.gotos, " ") != strings.Join(want, " ") {
		t.Fatalf("gotos = %q, want %q", f.gotos, want)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened %d surfaces, want 1", len(f.opened))
	}
}

func TestNextStartsAnotherStream(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}}
	p, c := newTest(t, Config{Query: "q"}, f, 5)
	steps(p, c, 4)
	p.Next()
	steps(p, c, 3)
	want := []string{radioURL("aaaaaaaaaaa"), "https://music.youtube.com/search?q=q", radioURL("bbbbbbbbbbb")}
	if strings.Join(f.gotos, " ") != strings.Join(want, " ") || p.Status().Phase != phasePlaying {
		t.Fatalf("gotos = %q", f.gotos)
	}
}

func TestAdvertIsReported(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}, ad: true}
	p, c := newTest(t, Config{Query: "q"}, f, 5)
	steps(p, c, 4)
	if !p.Status().Ad {
		t.Fatal("advert not reported")
	}
	f.ad = false
	steps(p, c, 1)
	if p.Status().Ad {
		t.Fatal("advert still reported")
	}
}

func TestPauseIsNotAStall(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Query: "q", StallAfter: 10 * time.Second}, f, 5)
	steps(p, c, 4)
	p.SetPaused(true)
	steps(p, c, 30)
	if f.last.Play || !p.Status().Paused {
		t.Fatal("page was not paused")
	}
	p.SetPaused(false)
	steps(p, c, 5)
	if !f.last.Play || len(f.gotos) != 1 {
		t.Fatalf("play = %v gotos = %q", f.last.Play, f.gotos)
	}
}

func TestLostSurfaceIsReopened(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Query: "q", ReopenAfter: 5 * time.Second}, f, 5)
	steps(p, c, 4)
	f.gone = true
	steps(p, c, 10)
	if len(f.opened) != 2 || p.Status().Phase != phasePlaying || p.Status().Error != "" {
		t.Fatalf("opened = %d status = %+v", len(f.opened), p.Status())
	}
}

func TestUnreachableCmuxOpensNothingExtra(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Query: "q", ReopenAfter: 5 * time.Second}, f, 5)
	steps(p, c, 4)
	f.err = cmux.ErrUnavailable
	steps(p, c, 30)
	if p.Status().Error == "" || p.Status().Phase != phasePlaying {
		t.Fatalf("status = %+v", p.Status())
	}
	f.err = nil
	steps(p, c, 2)
	if len(f.opened) != 1 || p.Status().Error != "" {
		t.Fatalf("opened = %d status = %+v", len(f.opened), p.Status())
	}
}

func TestStaleMetricFallsToMinimum(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Query: "q", StaleAfter: time.Minute}, f, 10)
	steps(p, c, 5)
	if f.last.Rate != 1.5 {
		t.Fatalf("rate = %v", f.last.Rate)
	}
	steps(p, c, 70)
	if st := p.Status(); !st.MetricStale || st.TargetRate != 0.5 || f.last.Rate != 0.5 {
		t.Fatalf("status = %+v rate = %v", st, f.last.Rate)
	}
	p.SetMetric(10, "test")
	steps(p, c, 2)
	if st := p.Status(); st.MetricStale || f.last.Rate != 1.5 {
		t.Fatalf("status = %+v rate = %v", st, f.last.Rate)
	}
}

func TestSurfaceIsReusedAcrossRuns(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "surface")
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Query: "q", SurfaceFile: file}, f, 5)
	steps(p, c, 4)

	p2, c2 := newTest(t, Config{Query: "q", SurfaceFile: file}, f, 5)
	steps(p2, c2, 4)
	if len(f.opened) != 1 || p2.Status().Surface != "surface:1" || p2.Status().Phase != phasePlaying {
		t.Fatalf("opened = %d status = %+v", len(f.opened), p2.Status())
	}

	f.gone = true
	p3, c3 := newTest(t, Config{Query: "q", SurfaceFile: file}, f, 5)
	steps(p3, c3, 4)
	if len(f.opened) != 2 || p3.Status().Surface != "surface:2" {
		t.Fatalf("opened = %d status = %+v", len(f.opened), p3.Status())
	}
}

func TestStopPauses(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Query: "q"}, f, 5)
	steps(p, c, 4)
	p.stop()
	if f.last.Play {
		t.Fatal("stop did not pause playback")
	}
}
