// Package server is dubd's local HTTP API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/eldridgeh/dubd/internal/library"
	"github.com/eldridgeh/dubd/internal/metric"
	"github.com/eldridgeh/dubd/internal/player"
)

// Player is what the API controls.
type Player interface {
	SetPaused(paused bool)
	Next()
	SetStream(s player.Stream)
	Status() player.Status
	SetDip(d time.Duration) error
	// Search looks words up without disturbing what is playing.
	Search(ctx context.Context, query string) ([]player.Track, error)
}

// Info is what GET /status returns.
type Info struct {
	Version string `json:"version"`
	player.Status
	Counting Counting `json:"counting"`
}

// Counting says how the metric is being arrived at.
type Counting struct {
	// Mode is "total", "agents" or "landings".
	Mode    string         `json:"mode"`
	Combine metric.Combine `json:"combine"`
	Window  string         `json:"landings_window"`
	Agents  []metric.Agent `json:"agents"`
}

// New returns the API handler.
//
//	GET    /status           state as JSON
//	GET    /metric           the metric as plain text
//	PUT    /metric           set the whole metric; the body is an integer
//	PUT    /metric/{agent}   set one agent's number; the daemon combines the agents
//	DELETE /metric/{agent}   forget one agent
//	POST   /land             count one landing; the daemon works out landings per hour
//	POST   /land/{agent}     the same, credited to an agent
//
// POST works wherever PUT does. The body of a landing may give a count.
//
//	POST /pause, /play    pause or resume the stream
//	POST /next            move to another stream
//	PUT  /stream          play something else (POST also works); the body is a link,
//	                      search words, a saved name, or the number of a search result
//	PUT  /dip             how long the dip in volume around a change of tempo
//	                      lasts (POST also works); the body is a length of time, or 0
//
// The library:
//
//	GET    /search           the results of the last search
//	POST   /search           search; the body is the words
//	GET    /history          the tracks that have played
//	GET    /saved            the saved streams
//	PUT    /saved/{name}     save a stream under a name (POST also works); the body is
//	                         as for /stream, or empty for what is playing
//	DELETE /saved/{name}     remove a saved stream
func New(p Player, board *metric.Board, lib *library.Library, version string) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(v)
	}
	// text reads a short body; ok is false once an error was sent.
	text := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2048))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return "", false
		}
		return strings.TrimSpace(string(body)), true
	}
	status := func(w http.ResponseWriter) {
		reply(w, Info{Version: version, Status: p.Status(), Counting: Counting{
			Mode: board.Mode(), Combine: board.Combine(), Window: board.Window().String(), Agents: board.Agents(),
		}})
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) { status(w) })
	mux.HandleFunc("GET /metric", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, board.Total())
	})
	// number reads an integer body; ok is false once an error was sent.
	number := func(w http.ResponseWriter, r *http.Request, empty int, allowEmpty bool) (v int, ok bool) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return 0, false
		}
		if allowEmpty && strings.TrimSpace(string(body)) == "" {
			return empty, true
		}
		v, err = metric.Parse(string(body))
		if err != nil {
			http.Error(w, "the body must be an integer: "+err.Error(), http.StatusBadRequest)
			return 0, false
		}
		return v, true
	}
	setMetric := func(w http.ResponseWriter, r *http.Request) {
		v, ok := number(w, r, 0, false)
		if !ok {
			return
		}
		if agent := r.PathValue("agent"); agent != "" {
			if _, err := board.SetAgent(agent, v); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		} else {
			board.Set(v, "http")
		}
		status(w)
	}
	land := func(w http.ResponseWriter, r *http.Request) {
		n, ok := number(w, r, 1, true)
		if !ok {
			return
		}
		if _, err := board.Land(r.PathValue("agent"), n); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		status(w)
	}
	for _, method := range []string{"PUT", "POST"} {
		mux.HandleFunc(method+" /metric", setMetric)
		mux.HandleFunc(method+" /metric/{agent}", setMetric)
	}
	mux.HandleFunc("POST /land", land)
	mux.HandleFunc("POST /land/{agent}", land)
	mux.HandleFunc("DELETE /metric/{agent}", func(w http.ResponseWriter, r *http.Request) {
		if !board.Forget(r.PathValue("agent")) {
			http.Error(w, "no such agent", http.StatusNotFound)
			return
		}
		status(w)
	})
	mux.HandleFunc("POST /pause", func(w http.ResponseWriter, _ *http.Request) { p.SetPaused(true); status(w) })
	mux.HandleFunc("POST /play", func(w http.ResponseWriter, _ *http.Request) { p.SetPaused(false); status(w) })
	setStream := func(w http.ResponseWriter, r *http.Request) {
		body, ok := text(w, r)
		if !ok {
			return
		}
		s, err := lib.Resolve(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.SetStream(s)
		status(w)
	}
	mux.HandleFunc("PUT /stream", setStream)
	mux.HandleFunc("POST /stream", setStream)

	setDip := func(w http.ResponseWriter, r *http.Request) {
		body, ok := text(w, r)
		if !ok {
			return
		}
		d, err := time.ParseDuration(body)
		if err == nil {
			err = p.SetDip(d)
		}
		if err != nil {
			http.Error(w, "the body must be a length of time such as 300ms, or 0: "+err.Error(), http.StatusBadRequest)
			return
		}
		status(w)
	}
	mux.HandleFunc("PUT /dip", setDip)
	mux.HandleFunc("POST /dip", setDip)

	mux.HandleFunc("GET /search", func(w http.ResponseWriter, _ *http.Request) { reply(w, lib.Results()) })
	mux.HandleFunc("POST /search", func(w http.ResponseWriter, r *http.Request) {
		query, ok := text(w, r)
		if !ok {
			return
		}
		if query == "" {
			http.Error(w, "the body must be the words to search for", http.StatusBadRequest)
			return
		}
		tracks, err := p.Search(r.Context(), query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// Answer with this search, not with whatever is the last search by
		// now: the player records its own searches too.
		results, err := lib.SetResults(query, tracks)
		if err != nil {
			http.Error(w, "could not write the library: "+err.Error(), http.StatusInternalServerError)
			return
		}
		reply(w, results)
	})
	mux.HandleFunc("GET /history", func(w http.ResponseWriter, _ *http.Request) { reply(w, lib.History()) })
	mux.HandleFunc("GET /saved", func(w http.ResponseWriter, _ *http.Request) { reply(w, lib.Saved()) })
	save := func(w http.ResponseWriter, r *http.Request) {
		body, ok := text(w, r)
		if !ok {
			return
		}
		s := p.Status().Stream
		if body != "" {
			var err error
			if s, err = lib.Resolve(body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if err := lib.Save(r.PathValue("name"), s); err != nil {
			code := http.StatusInternalServerError
			if errors.As(err, new(library.Refused)) {
				code = http.StatusBadRequest
			}
			http.Error(w, err.Error(), code)
			return
		}
		reply(w, lib.Saved())
	}
	mux.HandleFunc("PUT /saved/{name}", save)
	mux.HandleFunc("POST /saved/{name}", save)
	mux.HandleFunc("DELETE /saved/{name}", func(w http.ResponseWriter, r *http.Request) {
		ok, err := lib.Unsave(r.PathValue("name"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no stream is saved under that name", http.StatusNotFound)
			return
		}
		reply(w, lib.Saved())
	})
	mux.HandleFunc("POST /next", func(w http.ResponseWriter, _ *http.Request) { p.Next(); status(w) })
	return localOnly(mux)
}

// localOnly refuses requests made by web pages, which carry an Origin
// header; command-line clients do not.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "requests from web pages are not accepted", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
