// Package player keeps a YouTube Music stream playing in a cmux browser
// surface at a playback rate that follows a metric.
package player

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/eldridgeh/dubd/internal/cmux"
	"github.com/eldridgeh/dubd/internal/tempo"
)

// Browser is the part of cmux the player uses.
type Browser interface {
	Open(ctx context.Context, url string) (surface string, err error)
	Goto(ctx context.Context, surface, url string) error
	Eval(ctx context.Context, surface, script string, out any) error
}

// Config is the player's settings.
type Config struct {
	// URL plays a specific YouTube Music page. When empty, the player
	// searches YouTube Music for Query and plays the radio of a result.
	URL   string
	Query string

	Curve     tempo.Curve
	KeepPitch bool
	// Glide is the largest change in playback rate per second.
	Glide float64
	// Tick is how often the page is checked and corrected.
	Tick time.Duration
	// StallAfter restarts the stream when playback has not advanced this long.
	StallAfter time.Duration
	// SearchTimeout reloads the search page when it has shown no results this long.
	SearchTimeout time.Duration
	// ReopenAfter is the shortest time between opening browser surfaces.
	ReopenAfter time.Duration
	// StaleAfter treats the metric as its minimum when it has not been
	// updated this long. Zero never does.
	StaleAfter time.Duration
	// SurfaceFile remembers the browser surface between runs. Empty does not.
	SurfaceFile string
}

// Defaults fills unset timings.
func (c *Config) Defaults() {
	if c.Tick <= 0 {
		c.Tick = time.Second
	}
	if c.StallAfter <= 0 {
		c.StallAfter = 45 * time.Second
	}
	if c.SearchTimeout <= 0 {
		c.SearchTimeout = 30 * time.Second
	}
	if c.ReopenAfter <= 0 {
		c.ReopenAfter = 30 * time.Second
	}
}

const (
	phaseOpening   = "opening"
	phaseSearching = "searching"
	phasePlaying   = "playing"
)

// Status is a snapshot of the player.
type Status struct {
	Metric        int       `json:"metric"`
	MetricSource  string    `json:"metric_source"`
	MetricUpdated time.Time `json:"metric_updated"`
	MetricStale   bool      `json:"metric_stale"`
	TargetRate    float64   `json:"target_rate"`
	Rate          float64   `json:"rate"`
	Phase         string    `json:"phase"`
	Paused        bool      `json:"paused"`
	Surface       string    `json:"surface,omitempty"`
	Page          string    `json:"page,omitempty"`
	Title         string    `json:"title,omitempty"`
	Artist        string    `json:"artist,omitempty"`
	Position      float64   `json:"position_seconds"`
	Duration      float64   `json:"duration_seconds"`
	Error         string    `json:"error,omitempty"`
}

// Player runs the stream.
type Player struct {
	cfg Config
	b   Browser
	log *slog.Logger
	now func() time.Time

	mu           sync.Mutex
	metric       int
	metricSource string
	metricAt     time.Time
	paused       bool

	status Status // published by the loop after every step

	// The fields below belong to the loop goroutine alone.
	pausedNow    bool
	phase        string
	phaseSince   time.Time
	surface      string
	triedSaved   bool
	lastOpen     time.Time
	rate         float64
	pick         int
	fails        int
	lastPos      float64
	lastProgress time.Time
	page         page
	lastErr      string
}

// New returns a player with the given starting metric.
func New(cfg Config, b Browser, log *slog.Logger, metric int) *Player {
	cfg.Defaults()
	p := &Player{cfg: cfg, b: b, log: log, now: time.Now, phase: phaseOpening}
	p.metric, p.metricSource, p.metricAt = metric, "start", p.now()
	p.status = Status{Phase: phaseOpening}
	return p
}

// SetMetric records a new metric value and where it came from.
func (p *Player) SetMetric(v int, source string) {
	p.mu.Lock()
	changed := v != p.metric
	p.metric, p.metricSource, p.metricAt = v, source, p.now()
	p.mu.Unlock()
	if changed {
		p.log.Info("metric", "value", v, "source", source, "target_rate", p.cfg.Curve.Rate(v))
	}
}

// SetPaused pauses or resumes the stream.
func (p *Player) SetPaused(paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused = paused
}

// target is the rate the current metric calls for. Caller holds mu.
func (p *Player) target() (rate float64, stale bool) {
	stale = p.cfg.StaleAfter > 0 && p.now().Sub(p.metricAt) > p.cfg.StaleAfter
	if stale {
		return p.cfg.Curve.RateMin, true
	}
	return p.cfg.Curve.Rate(p.metric), false
}

// Status returns the current state.
func (p *Player) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.status
	s.Metric, s.MetricSource, s.MetricUpdated = p.metric, p.metricSource, p.metricAt
	s.TargetRate, s.MetricStale = p.target()
	s.Paused = p.paused
	return s
}

// publish makes the loop's state visible to Status.
func (p *Player) publish() {
	s := Status{Rate: p.rate, Phase: p.phase, Surface: p.surface, Error: p.lastErr}
	if p.phase == phasePlaying {
		s.Page = p.page.Host + p.page.Path
		s.Title, s.Artist = p.page.Title, p.page.Artist
		s.Position, s.Duration = p.page.Time, p.page.Duration
	}
	p.mu.Lock()
	p.status = s
	p.mu.Unlock()
}

// Run drives the stream until ctx ends, then pauses it.
func (p *Player) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.Tick)
	defer t.Stop()
	for {
		p.Step(ctx)
		select {
		case <-ctx.Done():
			p.stop()
			return
		case <-t.C:
		}
	}
}

// stop pauses playback, so silence means dubd is not running.
func (p *Player) stop() {
	surface, rate := p.surface, p.rate
	if surface == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var pg page
	_ = p.b.Eval(ctx, surface, tickScript(want{Rate: rate, KeepPitch: p.cfg.KeepPitch, Play: false}), &pg)
}

func (p *Player) searchURL() string {
	return "https://music.youtube.com/search?q=" + url.QueryEscape(p.cfg.Query)
}

func (p *Player) startURL() string {
	if p.cfg.URL != "" {
		return p.cfg.URL
	}
	return p.searchURL()
}

// radioURL is the endless "radio" queue YouTube Music builds from one track.
func radioURL(id string) string {
	return "https://music.youtube.com/watch?v=" + id + "&list=RDAMVM" + id
}

func (p *Player) setPhase(phase string) {
	p.phase, p.phaseSince = phase, p.now()
	if phase == phasePlaying {
		p.lastProgress, p.lastPos = p.now(), -1
	}
}

// Step does one round of work: get a surface, get a stream going in it, then
// hold the playback rate and watch for stalls. Only the loop calls it; it
// does not hold the lock while cmux works.
func (p *Player) Step(ctx context.Context) {
	p.mu.Lock()
	target, _ := p.target()
	paused := p.paused
	p.mu.Unlock()
	if p.pausedNow && !paused {
		p.lastProgress = p.now() // resuming is not a stall
	}
	p.pausedNow = paused
	defer p.publish()

	if p.rate == 0 {
		p.rate = target
	} else {
		p.rate = tempo.Step(p.rate, target, p.cfg.Glide*p.cfg.Tick.Seconds())
	}

	var err error
	switch p.phase {
	case phaseOpening:
		err = p.open(ctx)
	case phaseSearching:
		err = p.search(ctx)
	case phasePlaying:
		err = p.play(ctx)
	}
	p.note(err)
}

func (p *Player) open(ctx context.Context) error {
	if !p.triedSaved {
		p.triedSaved = true
		if p.adopt(ctx) {
			return nil
		}
	}
	if !p.lastOpen.IsZero() && p.now().Sub(p.lastOpen) < p.cfg.ReopenAfter {
		return nil
	}
	p.lastOpen = p.now()
	surface, err := p.b.Open(ctx, p.startURL())
	if err != nil {
		return err
	}
	p.surface = surface
	p.saveSurface()
	p.log.Info("opened browser surface", "surface", surface, "url", p.startURL())
	if p.cfg.URL != "" {
		p.setPhase(phasePlaying)
	} else {
		p.setPhase(phaseSearching)
	}
	return nil
}

// adopt takes over the surface a previous run left behind, if it is still a
// YouTube page.
func (p *Player) adopt(ctx context.Context) bool {
	if p.cfg.SurfaceFile == "" {
		return false
	}
	raw, err := os.ReadFile(p.cfg.SurfaceFile)
	surface := strings.TrimSpace(string(raw))
	if err != nil || surface == "" {
		return false
	}
	var pg page
	w := want{Rate: p.rate, KeepPitch: p.cfg.KeepPitch, Play: !p.pausedNow}
	if p.b.Eval(ctx, surface, tickScript(w), &pg) != nil || !strings.HasSuffix(pg.Host, "youtube.com") {
		return false
	}
	p.surface = surface
	p.setPhase(phasePlaying)
	// Give it a few seconds to show it is playing before starting over.
	p.lastProgress = p.now().Add(5*time.Second - p.cfg.StallAfter)
	p.log.Info("reusing browser surface", "surface", surface)
	return true
}

func (p *Player) saveSurface() {
	if p.cfg.SurfaceFile == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.cfg.SurfaceFile), 0o755); err == nil {
		err = os.WriteFile(p.cfg.SurfaceFile, []byte(p.surface+"\n"), 0o644)
		if err != nil {
			p.log.Warn("could not remember browser surface", "error", err)
		}
	}
}

func (p *Player) search(ctx context.Context) error {
	var f found
	if err := p.b.Eval(ctx, p.surface, findJS, &f); err != nil {
		return err
	}
	if len(f.IDs) == 0 {
		if p.now().Sub(p.phaseSince) > p.cfg.SearchTimeout {
			p.log.Warn("search page shows no tracks; reloading", "query", p.cfg.Query)
			p.setPhase(phaseSearching)
			return p.b.Goto(ctx, p.surface, p.searchURL())
		}
		return nil
	}
	// Each restart moves on to the next of the top results.
	id := f.IDs[p.pick%min(len(f.IDs), 5)]
	p.pick++
	if err := p.b.Goto(ctx, p.surface, radioURL(id)); err != nil {
		return err
	}
	p.log.Info("starting stream", "track", id)
	p.setPhase(phasePlaying)
	return nil
}

func (p *Player) play(ctx context.Context) error {
	var pg page
	w := want{Rate: p.rate, KeepPitch: p.cfg.KeepPitch, Play: !p.pausedNow}
	if err := p.b.Eval(ctx, p.surface, tickScript(w), &pg); err != nil {
		return err
	}
	if pg.Title != p.page.Title && pg.Video && pg.Title != "" {
		p.log.Info("now playing", "title", pg.Title, "artist", pg.Artist, "rate", p.rate)
	}
	p.page = pg
	if pg.Video && pg.Time != p.lastPos {
		p.lastPos, p.lastProgress = pg.Time, p.now()
	}
	if p.pausedNow || p.now().Sub(p.lastProgress) <= p.cfg.StallAfter {
		return nil
	}
	p.log.Warn("playback stalled; restarting stream", "page", pg.Host+pg.Path)
	if p.cfg.URL != "" {
		p.setPhase(phasePlaying)
		return p.b.Goto(ctx, p.surface, p.cfg.URL)
	}
	p.setPhase(phaseSearching)
	return p.b.Goto(ctx, p.surface, p.searchURL())
}

// note records the outcome of a step. Three failures in a row that are not
// cmux being unreachable or a script error mean the surface is gone, so the
// player opens a new one.
func (p *Player) note(err error) {
	if err == nil {
		p.fails, p.lastErr = 0, ""
		return
	}
	if err.Error() != p.lastErr {
		p.log.Error("cmux", "error", err)
	}
	p.lastErr = err.Error()
	if errors.Is(err, cmux.ErrUnavailable) || errors.Is(err, cmux.ErrScript) || errors.Is(err, context.Canceled) {
		return
	}
	p.fails++
	if p.fails >= 3 && p.phase != phaseOpening {
		p.log.Warn("browser surface lost; opening a new one", "surface", p.surface)
		p.fails, p.surface = 0, ""
		p.setPhase(phaseOpening)
	}
}
