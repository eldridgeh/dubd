// Package library keeps what dubd remembers about music in one JSON file:
// streams saved under a name, the results of the last search, and the
// tracks that have played.
package library

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eldridgeh/dubd/internal/player"
)

const (
	// MaxSaved is how many named streams are kept.
	MaxSaved = 500
	// MaxHistory is how many played tracks are kept; the oldest go first.
	MaxHistory = 500
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Results is what the last search returned.
type Results struct {
	Query  string         `json:"query"`
	At     time.Time      `json:"at"`
	Tracks []player.Track `json:"tracks"`
}

// Play is a track that played, and when it started.
type Play struct {
	At time.Time `json:"at"`
	player.Track
}

// Named is a stream saved under a name.
type Named struct {
	Name   string        `json:"name"`
	Stream player.Stream `json:"stream"`
}

// contents is the file.
type contents struct {
	Saved   map[string]player.Stream `json:"saved"`
	Results Results                  `json:"results"`
	History []Play                   `json:"history"`
}

// Library is the file, held in memory and written out on every change.
type Library struct {
	path string
	now  func() time.Time

	mu sync.Mutex
	c  contents
}

// Open reads the library at path. A file that does not exist yet is an
// empty library; one that cannot be read is an error, so that it is never
// written over. An empty path keeps the library in memory only.
func Open(path string) (*Library, error) {
	l := &Library{path: path, now: time.Now}
	l.c.Saved = map[string]player.Stream{}
	if path == "" {
		return l, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &l.c); err != nil {
		return nil, fmt.Errorf("%s is not a dubd library: %w", path, err)
	}
	if l.c.Saved == nil {
		l.c.Saved = map[string]player.Stream{}
	}
	return l, nil
}

// write puts the library on disk by way of a temporary file, so that a
// crash part way leaves the old file whole. Caller holds mu.
func (l *Library) write() error {
	if l.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(l.c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), filepath.Base(l.path)+".*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(raw, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), l.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

func linked(t player.Track) player.Track {
	t.Link = player.TrackURL(t.ID)
	return t
}

// SetResults records what a search returned.
func (l *Library) SetResults(query string, tracks []player.Track) error {
	kept := make([]player.Track, 0, len(tracks))
	for _, t := range tracks {
		kept = append(kept, linked(t))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.c.Results = Results{Query: query, At: l.now().UTC(), Tracks: kept}
	return l.write()
}

// Results returns what the last search returned.
func (l *Library) Results() Results {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.c.Results
	r.Tracks = append([]player.Track{}, r.Tracks...)
	return r
}

// Played records that a track started. The same track twice running is one
// entry.
func (l *Library) Played(t player.Track) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := len(l.c.History); n > 0 && l.c.History[n-1].ID == t.ID {
		return nil
	}
	l.c.History = append(l.c.History, Play{At: l.now().UTC(), Track: linked(t)})
	if n := len(l.c.History); n > MaxHistory {
		l.c.History = append([]Play{}, l.c.History[n-MaxHistory:]...)
	}
	return l.write()
}

// History returns the tracks that played, oldest first.
func (l *Library) History() []Play {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Play{}, l.c.History...)
}

// Refused is an error for something the library will not keep, as against
// something it could not write.
type Refused string

func (r Refused) Error() string { return string(r) }

// ValidName reports whether a stream can be saved under name. A number is
// not a name: it picks from the last search.
func ValidName(name string) bool {
	if _, err := strconv.Atoi(name); err == nil {
		return false
	}
	return nameRE.MatchString(name)
}

// Save keeps a stream under a name, replacing one already there.
func (l *Library) Save(name string, s player.Stream) error {
	if !ValidName(name) {
		return Refused("a name may use letters, digits, dot, dash and underscore, up to 64 characters, and is not a number")
	}
	if s.URL == "" && s.Query == "" {
		return Refused("nothing to save")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.c.Saved[name]; !ok && len(l.c.Saved) >= MaxSaved {
		return Refused(fmt.Sprintf("%d streams are saved already; remove one first", MaxSaved))
	}
	old, had := l.c.Saved[name]
	l.c.Saved[name] = s
	if err := l.write(); err != nil {
		// Memory and the file stay the same as each other.
		if had {
			l.c.Saved[name] = old
		} else {
			delete(l.c.Saved, name)
		}
		return err
	}
	return nil
}

// Unsave removes a saved stream and reports whether there was one.
func (l *Library) Unsave(name string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	old, ok := l.c.Saved[name]
	if !ok {
		return false, nil
	}
	delete(l.c.Saved, name)
	if err := l.write(); err != nil {
		l.c.Saved[name] = old
		return false, err
	}
	return true, nil
}

// Saved returns the saved streams, by name.
func (l *Library) Saved() []Named {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Named, 0, len(l.c.Saved))
	for name, s := range l.c.Saved {
		out = append(out, Named{Name: name, Stream: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve reads what to play from text: a number picks that result of the
// last search and plays its radio, a saved name plays what was saved, and
// anything else is a link or search words.
func (l *Library) Resolve(text string) (player.Stream, error) {
	text = strings.TrimSpace(text)
	if n, err := strconv.Atoi(text); err == nil {
		l.mu.Lock()
		defer l.mu.Unlock()
		tracks := l.c.Results.Tracks
		if len(tracks) == 0 {
			return player.Stream{}, errors.New("a number picks from the last search, and there is none yet: run dubd search <words>")
		}
		if n < 1 || n > len(tracks) {
			return player.Stream{}, fmt.Errorf("the last search has results 1 to %d", len(tracks))
		}
		return player.Stream{URL: player.RadioURL(tracks[n-1].ID)}, nil
	}
	l.mu.Lock()
	s, ok := l.c.Saved[text]
	l.mu.Unlock()
	if ok {
		return s, nil
	}
	return player.ParseStream(text)
}
