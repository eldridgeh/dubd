package player

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
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
	dialog bool     // the page shows a dialog and answers nothing else
	closed []string // surfaces closed
	gone   bool     // the surface no longer exists
	err    error    // every call fails with this
	pos    float64
	last   want
	track  string // track the page says is playing, when not the one in the address
	title  string // its title, when not "Track"
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

func (f *fakeBrowser) AcceptDialog(context.Context, string) (bool, error) {
	if f.err != nil || f.gone {
		return false, errors.New("Command timed out")
	}
	was := f.dialog
	f.dialog = false
	return was, nil
}

func (f *fakeBrowser) Close(_ context.Context, surface string) error {
	f.closed = append(f.closed, surface)
	return nil
}

func (f *fakeBrowser) Goto(_ context.Context, _ string, url string) error {
	if f.err != nil {
		return f.err
	}
	if f.dialog {
		return errors.New("Command timed out")
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
	if f.dialog {
		return errors.New("Command timed out")
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
		list := ""
		if i := strings.Index(f.url, "list="); i >= 0 {
			list = f.url[i+len("list="):]
		}
		id := f.track
		if m := regexp.MustCompile(`[?&]v=([\w-]{11})`).FindStringSubmatch(f.url); m != nil && id == "" {
			id = m[1]
		}
		title := f.title
		if title == "" {
			title = "Track"
		}
		res = page{Host: "music.youtube.com", Path: "/watch", List: list, Video: watching, Loaded: watching,
			Paused: !f.last.Play, Rate: f.last.Rate, Time: f.pos, ID: id, Title: title, Ad: f.ad}
	} else {
		var tracks []Track
		if strings.Contains(f.url, "/search") {
			for _, id := range f.ids {
				tracks = append(tracks, Track{ID: id, Title: "Title " + id, By: "Artist"})
			}
		}
		res = found{Tracks: tracks}
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
	p, c := newTest(t, Config{Stream: Stream{Query: "ai dub reggae"}}, f, 5)
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
	p, c := newTest(t, Config{Stream: Stream{URL: u}}, f, 0)
	steps(p, c, 3)
	if len(f.opened) != 1 || f.opened[0] != u || len(f.gotos) != 0 || p.Status().Phase != phasePlaying {
		t.Fatalf("opened = %q gotos = %q", f.opened, f.gotos)
	}
}

func TestRateFollowsMetricInOneChange(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}}, f, 0)
	steps(p, c, 3)
	if f.last.Rate != 0.5 {
		t.Fatalf("starting rate = %v", f.last.Rate)
	}
	p.SetMetric(10, "test")
	if got := p.Status().TargetRate; got != 1.5 {
		t.Fatalf("target = %v", got)
	}
	steps(p, c, 1)
	if f.last.Rate != 1.5 || p.Status().Rate != 1.5 {
		t.Fatalf("rate one step after the change = %v, want 1.5", f.last.Rate)
	}
}

func TestNextAfterReusingATabMovesOn(t *testing.T) {
	file := filepath.Join(t.TempDir(), "surface")
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, SurfaceFile: file}, f, 5)
	steps(p, c, 4)

	p2, c2 := newTest(t, Config{Stream: Stream{Query: "q"}, SurfaceFile: file}, f, 5)
	steps(p2, c2, 2)
	p2.Next()
	steps(p2, c2, 3)
	if got := f.gotos[len(f.gotos)-1]; got != RadioURL("bbbbbbbbbbb") {
		t.Fatalf("next after reuse went to %q", got)
	}
}

func TestKeepPitch(t *testing.T) {
	for _, keep := range []bool{true, false} {
		f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
		p, c := newTest(t, Config{Stream: Stream{Query: "q"}, KeepPitch: keep}, f, 0)
		steps(p, c, 3)
		if f.last.KeepPitch != keep {
			t.Errorf("keepPitch = %v, want %v", f.last.KeepPitch, keep)
		}
	}
}

func TestStallRestartsWithNextTrack(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, StallAfter: 10 * time.Second}, f, 5)
	steps(p, c, 5)
	f.stuck = true
	steps(p, c, 14)
	f.stuck = false
	steps(p, c, 3)
	want := []string{RadioURL("aaaaaaaaaaa"), "https://music.youtube.com/search?q=q", RadioURL("bbbbbbbbbbb")}
	if strings.Join(f.gotos, " ") != strings.Join(want, " ") {
		t.Fatalf("gotos = %q, want %q", f.gotos, want)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened %d surfaces, want 1", len(f.opened))
	}
}

func TestNextStartsAnotherStream(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}}, f, 5)
	steps(p, c, 4)
	p.Next()
	steps(p, c, 3)
	want := []string{RadioURL("aaaaaaaaaaa"), "https://music.youtube.com/search?q=q", RadioURL("bbbbbbbbbbb")}
	if strings.Join(f.gotos, " ") != strings.Join(want, " ") || p.Status().Phase != phasePlaying {
		t.Fatalf("gotos = %q", f.gotos)
	}
}

func TestAdvertIsReported(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}, ad: true}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}}, f, 5)
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
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, StallAfter: 10 * time.Second}, f, 5)
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
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, ReopenAfter: 5 * time.Second}, f, 5)
	steps(p, c, 4)
	f.gone = true
	steps(p, c, 10)
	if len(f.opened) != 2 || p.Status().Phase != phasePlaying || p.Status().Error != "" {
		t.Fatalf("opened = %d status = %+v", len(f.opened), p.Status())
	}
	if len(f.closed) != 1 || f.closed[0] != "surface:1" {
		t.Fatalf("closed = %q, want the abandoned surface", f.closed)
	}
}

func TestUnreachableCmuxOpensNothingExtra(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, ReopenAfter: 5 * time.Second}, f, 5)
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
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, StaleAfter: time.Minute}, f, 10)
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
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, SurfaceFile: file}, f, 5)
	steps(p, c, 4)

	p2, c2 := newTest(t, Config{Stream: Stream{Query: "q"}, SurfaceFile: file}, f, 5)
	steps(p2, c2, 4)
	if len(f.opened) != 1 || p2.Status().Surface != "surface:1" || p2.Status().Phase != phasePlaying {
		t.Fatalf("opened = %d status = %+v", len(f.opened), p2.Status())
	}

	f.gone = true
	p3, c3 := newTest(t, Config{Stream: Stream{Query: "q"}, SurfaceFile: file}, f, 5)
	steps(p3, c3, 4)
	if len(f.opened) != 2 || p3.Status().Surface != "surface:2" {
		t.Fatalf("opened = %d status = %+v", len(f.opened), p3.Status())
	}
}

func TestStopPauses(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}}, f, 5)
	steps(p, c, 4)
	p.stop()
	if f.last.Play {
		t.Fatal("stop did not pause playback")
	}
}

func TestParseStream(t *testing.T) {
	for in, want := range map[string]Stream{
		"heavy dub techno": {Query: "heavy dub techno"},
		"  slopcore dub\n": {Query: "slopcore dub"},
		"https://music.youtube.com/playlist?list=PL123":            {URL: "https://music.youtube.com/watch?list=PL123"},
		"https://www.youtube.com/playlist?list=PL123&si=x":         {URL: "https://www.youtube.com/watch?list=PL123"},
		"https://music.youtube.com/watch?v=aaaaaaaaaaa&list=PL123": {URL: "https://music.youtube.com/watch?v=aaaaaaaaaaa&list=PL123"},
	} {
		if got, err := ParseStream(in); err != nil || got != want {
			t.Errorf("ParseStream(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "https://example.com/watch?v=x", "http://music.youtube.com/watch?v=x", "https://notyoutube.com/x", "https://evil.example/music.youtube.com"} {
		if got, err := ParseStream(in); err == nil {
			t.Errorf("ParseStream(%q) = %+v, want an error", in, got)
		}
	}
}

func TestSetStreamSwitches(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}}, f, 5)
	steps(p, c, 4)

	link := "https://music.youtube.com/watch?list=PL123"
	p.SetStream(Stream{URL: link})
	steps(p, c, 3)
	if got := f.gotos[len(f.gotos)-1]; got != link || p.Status().Stream.URL != link || p.Status().Phase != phasePlaying {
		t.Fatalf("after a link: last goto %q status %+v", got, p.Status())
	}

	p.SetStream(Stream{Query: "other words"})
	steps(p, c, 3)
	n := len(f.gotos)
	if f.gotos[n-2] != "https://music.youtube.com/search?q=other+words" || f.gotos[n-1] != RadioURL("aaaaaaaaaaa") {
		t.Fatalf("after search words: gotos %q", f.gotos[n-2:])
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened %d surfaces, want 1", len(f.opened))
	}
}

func TestReusedTabSwitchesToTheNewStream(t *testing.T) {
	file := filepath.Join(t.TempDir(), "surface")
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, SurfaceFile: file}, f, 5)
	steps(p, c, 4)

	link := "https://music.youtube.com/watch?list=PL123"
	p2, c2 := newTest(t, Config{Stream: Stream{URL: link}, SurfaceFile: file}, f, 5)
	steps(p2, c2, 3)
	if len(f.opened) != 1 || f.gotos[len(f.gotos)-1] != link {
		t.Fatalf("opened %d, gotos %q", len(f.opened), f.gotos)
	}

	// The same stream again leaves the tab playing.
	n := len(f.gotos)
	p3, c3 := newTest(t, Config{Stream: Stream{URL: link}, SurfaceFile: file}, f, 5)
	steps(p3, c3, 3)
	if len(f.gotos) != n {
		t.Fatalf("reloaded a tab that was already playing the stream: %q", f.gotos[n:])
	}
}

func TestSetStreamWhileSearching(t *testing.T) {
	f := &fakeBrowser{} // the search page shows nothing yet
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}}, f, 5)
	steps(p, c, 2)
	link := "https://music.youtube.com/watch?list=PL123"
	p.SetStream(Stream{URL: link})
	steps(p, c, 2)
	if len(f.gotos) != 1 || f.gotos[0] != link || p.Status().Phase != phasePlaying {
		t.Fatalf("gotos %q status %+v", f.gotos, p.Status())
	}
}

func TestStreamSwitchIsRetriedAfterAFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "surface")
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, SurfaceFile: file}, f, 5)
	steps(p, c, 4)

	link := "https://music.youtube.com/watch?list=PL123"
	f.err = cmux.ErrUnavailable
	p.SetStream(Stream{URL: link})
	steps(p, c, 5)
	if p.Status().Error == "" {
		t.Fatal("the failed switch was not reported")
	}
	if saved, _ := os.ReadFile(file); strings.Contains(string(saved), link) {
		t.Fatalf("the new stream was recorded before it loaded: %q", saved)
	}

	f.err = nil
	steps(p, c, 2)
	if got := f.gotos[len(f.gotos)-1]; got != link || p.Status().Error != "" {
		t.Fatalf("after cmux came back: last goto %q status %+v", got, p.Status())
	}
	if saved, _ := os.ReadFile(file); !strings.Contains(string(saved), link) {
		t.Fatalf("the new stream was not recorded: %q", saved)
	}
}

func TestDialogIsAcceptedNotTreatedAsALostTab(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}}, f, 5)
	steps(p, c, 4)
	f.dialog = true
	steps(p, c, 6)
	if f.dialog || len(f.opened) != 1 || len(f.closed) != 0 || p.Status().Error != "" || p.Status().Phase != phasePlaying {
		t.Fatalf("dialog %v opened %d closed %q status %+v", f.dialog, len(f.opened), f.closed, p.Status())
	}
}

func TestReusedTabWithADialogIsKept(t *testing.T) {
	file := filepath.Join(t.TempDir(), "surface")
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, SurfaceFile: file}, f, 5)
	steps(p, c, 4)

	f.dialog = true
	p2, c2 := newTest(t, Config{Stream: Stream{Query: "q"}, SurfaceFile: file}, f, 5)
	steps(p2, c2, 3)
	if len(f.opened) != 1 || p2.Status().Surface != "surface:1" || p2.Status().Error != "" {
		t.Fatalf("opened %d status %+v", len(f.opened), p2.Status())
	}
}

func TestOpenFailureStaysReported(t *testing.T) {
	f := &fakeBrowser{err: errors.New("browser is disabled")}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, ReopenAfter: 10 * time.Second}, f, 5)
	steps(p, c, 5)
	if got := p.Status().Error; got != "browser is disabled" {
		t.Fatalf("error while waiting to retry = %q", got)
	}
}

func TestRateSettlesBetweenChanges(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa"}}
	p, c := newTest(t, Config{Stream: Stream{Query: "q"}, Settle: 10 * time.Second}, f, 0)
	steps(p, c, 3) // the first rate is applied at once
	if f.last.Rate != 0.5 {
		t.Fatalf("starting rate = %v", f.last.Rate)
	}
	// A burst of reports within the settle time is not followed one by one.
	for _, m := range []int{2, 9, 4, 10} {
		p.SetMetric(m, "test")
		steps(p, c, 1)
		if f.last.Rate != 0.5 {
			t.Fatalf("rate moved to %v during the settle time", f.last.Rate)
		}
	}
	steps(p, c, 5) // 12s since the first rate
	if f.last.Rate != 1.5 || p.Status().TargetRate != 1.5 {
		t.Fatalf("after settling: rate %v, want 1.5 (the latest metric)", f.last.Rate)
	}
	p.SetMetric(0, "test")
	steps(p, c, 3)
	if f.last.Rate != 1.5 {
		t.Fatalf("rate moved to %v only 3s after the last change", f.last.Rate)
	}
	steps(p, c, 8)
	if f.last.Rate != 0.5 {
		t.Fatalf("rate = %v, want 0.5 once settled", f.last.Rate)
	}
}

func TestSearchResultsAndTracksAreReported(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}}
	var query string
	var results, played []Track
	cfg := Config{Stream: Stream{Query: "ai dub reggae"},
		OnResults: func(q string, tr []Track) { query, results = q, tr },
		OnTrack:   func(tr Track) { played = append(played, tr) }}
	p, c := newTest(t, cfg, f, 5)
	steps(p, c, 3)
	if query != "ai dub reggae" || len(results) != 2 || results[1] != (Track{ID: "bbbbbbbbbbb", Title: "Title bbbbbbbbbbb", By: "Artist"}) {
		t.Fatalf("results for %q = %+v", query, results)
	}
	if len(played) != 0 {
		t.Fatalf("a track was reported before it had held steady: %+v", played)
	}
	steps(p, c, 5)
	if len(played) != 1 || played[0] != (Track{ID: "aaaaaaaaaaa", Title: "Track"}) {
		t.Fatalf("played = %+v", played)
	}

	// The radio moves on: the new track is reported once, with the title
	// that settled, not the one showing while the page changed over.
	f.track = "ccccccccccc"
	steps(p, c, 1)
	f.title = "Second"
	steps(p, c, 6)
	if len(played) != 2 || played[1] != (Track{ID: "ccccccccccc", Title: "Second"}) {
		t.Fatalf("played = %+v", played)
	}

	// An advert is not a track.
	f.ad, f.track, f.title = true, "ddddddddddd", "Buy things"
	steps(p, c, 6)
	if len(played) != 2 {
		t.Fatalf("an advert was reported as a track: %+v", played)
	}
}

func TestSearchUsesItsOwnTab(t *testing.T) {
	f := &fakeBrowser{ids: []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}}
	p, _ := newTest(t, Config{Stream: Stream{Query: "x"}}, f, 5)
	got, err := p.Search(context.Background(), "heavy dub techno")
	if err != nil || len(got) != 2 || got[0].Title != "Title aaaaaaaaaaa" {
		t.Fatalf("Search = %+v, %v", got, err)
	}
	if len(f.opened) != 1 || f.opened[0] != "https://music.youtube.com/search?q=heavy+dub+techno" {
		t.Fatalf("opened = %q", f.opened)
	}
	if len(f.closed) != 1 || f.closed[0] != "surface:1" || len(f.gotos) != 0 {
		t.Fatalf("closed = %q, gotos = %q", f.closed, f.gotos)
	}
}

func TestSearchWithNoResultsClosesItsTab(t *testing.T) {
	f := &fakeBrowser{}
	p, _ := newTest(t, Config{Stream: Stream{Query: "x"}, Tick: 5 * time.Millisecond, SearchTimeout: 40 * time.Millisecond}, f, 5)
	if got, err := p.Search(context.Background(), "nothing at all"); err == nil || !strings.Contains(err.Error(), "no tracks") {
		t.Fatalf("Search = %+v, %v", got, err)
	}
	if len(f.closed) != 1 {
		t.Fatalf("the search tab was left open: closed = %q", f.closed)
	}
	f.err = errors.New("Command timed out")
	if _, err := p.Search(context.Background(), "x"); err == nil {
		t.Fatal("expected the error from cmux")
	}
}
