// Package player keeps a YouTube Music stream playing in a cmux browser
// surface at a playback rate that follows a metric.
package player

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	// AcceptDialog accepts a dialog the page is showing and reports
	// whether there was one.
	AcceptDialog(ctx context.Context, surface string) (bool, error)
	Close(ctx context.Context, surface string) error
}

// Stream says what to play: a YouTube or YouTube Music URL, or, when URL is
// empty, a YouTube Music search whose top results' radios are played.
type Stream struct {
	URL   string `json:"url,omitempty"`
	Query string `json:"query,omitempty"`
}

// ParseStream reads a stream from text: a link, or search words. A link to
// a playlist page, which does not play by itself, becomes the link that
// plays that playlist.
func ParseStream(text string) (Stream, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Stream{}, errors.New("no link or search words given")
	}
	if !strings.HasPrefix(text, "http://") && !strings.HasPrefix(text, "https://") {
		return Stream{Query: text}, nil
	}
	u, err := url.Parse(text)
	if err != nil {
		return Stream{}, fmt.Errorf("not a usable link: %w", err)
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || (host != "youtube.com" && !strings.HasSuffix(host, ".youtube.com")) {
		return Stream{}, errors.New("the link must be an https youtube.com or music.youtube.com link")
	}
	if list := u.Query().Get("list"); u.Path == "/playlist" && list != "" {
		u.Path, u.RawQuery = "/watch", url.Values{"list": {list}}.Encode()
	}
	return Stream{URL: u.String()}, nil
}

// Config is the player's settings.
type Config struct {
	// Stream is what to play at the start.
	Stream Stream

	Curve     tempo.Curve
	KeepPitch bool
	// Settle is the shortest time between changes of playback rate, so
	// that a burst of reports is one change of tempo. Zero does not wait.
	Settle time.Duration
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
	// OnResults is told what a search returned, when set.
	OnResults func(query string, tracks []Track)
	// OnTrack is told each track that plays, when set.
	OnTrack func(Track)
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
	Stream        Stream    `json:"stream"`
	Ad            bool      `json:"ad"`
	SignedIn      bool      `json:"signed_in"`
	Dialogs       int       `json:"dialogs_accepted"`
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
	next         bool
	dialogs      int // page dialogs accepted
	stream       Stream

	status Status // published by the loop after every step

	// The fields below belong to the loop goroutine alone.
	pausedNow    bool
	pending      bool      // a new stream is still to be loaded
	rateAt       time.Time // when the rate last changed
	src          Stream    // the stream the loop is working on
	phase        string
	phaseSince   time.Time
	surface      string
	triedSaved   bool
	lastOpen     time.Time
	rate         float64
	pick         int
	seed         string // track whose radio is playing
	fails        int
	lastPos      float64
	lastProgress time.Time
	page         page
	playing      string  // title last logged
	cand         Track   // track the page shows, until it has held steady
	candSeen     int     // checks on which it was playing
	candPos      float64 // where playback was on the last check
	told         string  // track last passed to OnTrack
	lastErr      string

	searchMu sync.Mutex // one Search at a time
}

// New returns a player with the given starting metric.
func New(cfg Config, b Browser, log *slog.Logger, metric int) *Player {
	cfg.Defaults()
	p := &Player{cfg: cfg, b: b, log: log, now: time.Now, phase: phaseOpening}
	p.metric, p.metricSource, p.metricAt = metric, "start", p.now()
	p.status = Status{Phase: phaseOpening}
	p.stream, p.src = cfg.Stream, cfg.Stream
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

// Next drops the current stream and starts the radio of the next search
// result (or reloads the URL, when one was given).
func (p *Player) Next() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next = true
}

// NoteDialog records that a dialog shown by the page, such as "Leave
// site?", was accepted.
func (p *Player) NoteDialog(surface string) {
	p.mu.Lock()
	p.dialogs++
	p.mu.Unlock()
	p.log.Info("accepted a dialog the page was showing", "surface", surface)
}

// SetStream switches to another stream.
func (p *Player) SetStream(s Stream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stream, p.next = s, true
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
	s.Paused, s.Stream, s.Dialogs = p.paused, p.stream, p.dialogs
	return s
}

// publish makes the loop's state visible to Status.
func (p *Player) publish() {
	s := Status{Rate: p.rate, Phase: p.phase, Surface: p.surface, Error: p.lastErr}
	if p.phase == phasePlaying {
		s.Page = p.page.Host + p.page.Path
		s.Title, s.Artist, s.Ad = p.page.Title, p.page.Artist, p.page.Ad
		s.SignedIn = p.page.SignedIn
		s.Position, s.Duration = math.Round(p.page.Time), math.Round(p.page.Duration)
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

func (p *Player) searchURL() string { return searchURL(p.src.Query) }

func searchURL(query string) string {
	return "https://music.youtube.com/search?q=" + url.QueryEscape(query)
}

// MaxResults is how many results of a search are kept.
const MaxResults = 20

// Search looks words up on YouTube Music in a tab of its own, so that what
// is playing carries on, and returns the tracks found.
func (p *Player) Search(ctx context.Context, query string) ([]Track, error) {
	p.searchMu.Lock()
	defer p.searchMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, p.cfg.SearchTimeout)
	defer cancel()
	surface, err := p.b.Open(ctx, searchURL(query))
	if err != nil {
		return nil, err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.b.Close(c, surface); err != nil {
			p.log.Warn("could not close the search tab", "surface", surface, "error", err)
		}
	}()
	for {
		var f found
		err := p.b.Eval(ctx, surface, findJS, &f)
		if err == nil && len(f.Tracks) > 0 {
			return f.Tracks[:min(len(f.Tracks), MaxResults)], nil
		}
		select {
		case <-ctx.Done():
			if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				err = errors.New("the search page showed no tracks")
			}
			return nil, err
		case <-time.After(p.cfg.Tick):
		}
	}
}

func (p *Player) startURL() string {
	if p.src.URL != "" {
		return p.src.URL
	}
	return p.searchURL()
}

// RadioURL is the endless "radio" queue YouTube Music builds from one track.
func RadioURL(id string) string {
	return TrackURL(id) + "&list=" + radioPrefix + id
}

// TrackURL plays one track by itself.
func TrackURL(id string) string {
	return "https://music.youtube.com/watch?v=" + id
}

const radioPrefix = "RDAMVM"

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
	next := p.next
	p.next = false
	if p.src != p.stream {
		p.src, p.pick, p.seed = p.stream, 0, ""
	}
	p.mu.Unlock()
	if next {
		p.pending = true
	}
	if p.pausedNow && !paused {
		p.lastProgress = p.now() // resuming is not a stall
	}
	p.pausedNow = paused
	defer p.publish()

	// One rate change per metric change: every change costs an audible
	// hiccup in WebKit, so the rate is not ramped, and changes are kept at
	// least Settle apart.
	if target != p.rate && (p.rate == 0 || p.now().Sub(p.rateAt) >= p.cfg.Settle) {
		p.rate, p.rateAt = target, p.now()
	}

	var err error
	switch {
	case p.phase == phaseOpening:
		err = p.open(ctx)
	case p.pending:
		// Stays pending until the navigation succeeds, so a failure is
		// retried on the next step, and only then is the stream recorded.
		if err = p.restart(ctx); err == nil {
			p.pending = false
			p.saveSurface()
			p.log.Info("started another stream", "url", p.src.URL, "query", p.src.Query)
		}
	case p.phase == phaseSearching:
		err = p.search(ctx)
	case p.phase == phasePlaying:
		err = p.play(ctx)
	}
	p.note(ctx, err)
}

// errWaiting means the step did nothing but wait, so the last outcome stands.
var errWaiting = errors.New("waiting")

func (p *Player) open(ctx context.Context) error {
	if !p.triedSaved {
		p.triedSaved = true
		if p.adopt(ctx) {
			return nil
		}
	}
	if !p.lastOpen.IsZero() && p.now().Sub(p.lastOpen) < p.cfg.ReopenAfter {
		return errWaiting
	}
	p.lastOpen = p.now()
	surface, err := p.b.Open(ctx, p.startURL())
	if err != nil {
		return err
	}
	p.surface, p.pending = surface, false
	p.saveSurface()
	p.log.Info("opened browser surface", "surface", surface, "url", p.startURL())
	if p.src.URL != "" {
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
	surface, was, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	if err != nil || surface == "" {
		return false
	}
	var pg page
	w := want{Rate: p.rate, KeepPitch: p.cfg.KeepPitch, Play: !p.pausedNow}
	err = p.b.Eval(ctx, surface, tickScript(w), &pg)
	if err != nil {
		// The tab may be waiting on a dialog left from the last run.
		if ok, _ := p.b.AcceptDialog(ctx, surface); ok {
			err = p.b.Eval(ctx, surface, tickScript(w), &pg)
		}
	}
	if err != nil || !strings.HasSuffix(pg.Host, "youtube.com") {
		return false
	}
	p.surface = surface
	p.seed = strings.TrimPrefix(pg.List, radioPrefix)
	p.setPhase(phasePlaying)
	// Give it a few seconds to show it is playing before starting over.
	p.lastProgress = p.now().Add(5*time.Second - p.cfg.StallAfter)
	p.log.Info("reusing browser surface", "surface", surface)
	// The tab may be playing what an earlier run was asked for.
	p.pending = was != p.streamKey()
	return true
}

// streamKey identifies the stream in the surface file.
func (p *Player) streamKey() string {
	if p.src.URL != "" {
		return p.src.URL
	}
	return "search: " + p.src.Query
}

// saveSurface records the surface and the stream it was given.
func (p *Player) saveSurface() {
	if p.cfg.SurfaceFile == "" || p.surface == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.cfg.SurfaceFile), 0o755); err == nil {
		err = os.WriteFile(p.cfg.SurfaceFile, []byte(p.surface+"\n"+p.streamKey()+"\n"), 0o644)
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
	if len(f.Tracks) == 0 {
		if p.now().Sub(p.phaseSince) > p.cfg.SearchTimeout {
			p.log.Warn("search page shows no tracks; reloading", "query", p.src.Query)
			p.setPhase(phaseSearching)
			return p.b.Goto(ctx, p.surface, p.searchURL())
		}
		return nil
	}
	// Each restart moves on to the next of the top results, and never
	// straight back to the stream that was just playing.
	if p.cfg.OnResults != nil {
		p.cfg.OnResults(p.src.Query, f.Tracks[:min(len(f.Tracks), MaxResults)])
	}
	top := f.Tracks[:min(len(f.Tracks), 5)]
	id := top[p.pick%len(top)].ID
	if id == p.seed && len(top) > 1 {
		p.pick++
		id = top[p.pick%len(top)].ID
	}
	p.pick++
	p.seed = id
	if err := p.b.Goto(ctx, p.surface, RadioURL(id)); err != nil {
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
	switch {
	case pg.Ad && !p.page.Ad:
		p.log.Info("advert playing")
	case !pg.Ad && pg.Video && pg.Title != "" && pg.Title != p.playing:
		p.playing = pg.Title
		p.log.Info("now playing", "title", pg.Title, "artist", pg.Artist, "rate", p.rate)
	}
	p.page = pg
	p.track(pg)
	if pg.Video && pg.Time != p.lastPos {
		p.lastPos, p.lastProgress = pg.Time, p.now()
	}
	if p.pausedNow || p.now().Sub(p.lastProgress) <= p.cfg.StallAfter {
		return nil
	}
	p.log.Warn("playback stalled; restarting stream", "page", pg.Host+pg.Path)
	return p.restart(ctx)
}

// track tells OnTrack about a track once it has been heard: the page has
// shown the same track and title while playback moved forward on two checks
// running. A track that is loaded and never plays is not reported, and
// neither is the mix of old and new that the page shows for a moment when
// one track follows another.
func (p *Player) track(pg page) {
	if p.cfg.OnTrack == nil || pg.Ad || !pg.Video || pg.ID == "" || pg.Title == "" {
		return
	}
	t := Track{ID: pg.ID, Title: pg.Title, By: pg.Artist}
	if t != p.cand {
		p.cand, p.candSeen, p.candPos = t, 0, pg.Time
		return
	}
	moved := !pg.Paused && pg.Time > p.candPos
	p.candPos = pg.Time
	if !moved {
		return
	}
	if p.candSeen++; p.candSeen == 2 && t.ID != p.told {
		p.told = t.ID
		p.cfg.OnTrack(t)
	}
}

// restart loads a fresh stream in the same surface.
func (p *Player) restart(ctx context.Context) error {
	if p.src.URL != "" {
		p.setPhase(phasePlaying)
		return p.b.Goto(ctx, p.surface, p.src.URL)
	}
	p.setPhase(phaseSearching)
	return p.b.Goto(ctx, p.surface, p.searchURL())
}

// note records the outcome of a step. Three failures in a row that are not
// cmux being unreachable or a script error mean the surface is gone, so the
// player opens a new one.
func (p *Player) note(ctx context.Context, err error) {
	if err == nil {
		p.fails, p.lastErr = 0, ""
		return
	}
	if errors.Is(err, errWaiting) {
		return
	}
	if err.Error() != p.lastErr {
		p.log.Error("cmux", "error", err)
	}
	p.lastErr = err.Error()
	if errors.Is(err, cmux.ErrUnavailable) || errors.Is(err, context.Canceled) || p.surface == "" {
		return
	}
	// A page showing a dialog, such as "Leave site?", answers nothing else
	// until the dialog is dealt with.
	if ok, _ := p.b.AcceptDialog(ctx, p.surface); ok {
		p.fails = 0
		return
	}
	if errors.Is(err, cmux.ErrScript) {
		return
	}
	p.fails++
	if p.fails >= 3 && p.phase != phaseOpening {
		p.log.Warn("browser surface lost; opening a new one", "surface", p.surface)
		// Do not leave the old tab behind if it is still there.
		_ = p.b.Close(ctx, p.surface)
		p.fails, p.surface = 0, ""
		p.setPhase(phaseOpening)
	}
}
