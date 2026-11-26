// Package server is dubd's local HTTP API.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/eldridgeh/dubd/internal/metric"
	"github.com/eldridgeh/dubd/internal/player"
)

// Player is what the API controls.
type Player interface {
	SetPaused(paused bool)
	Next()
	SetStream(s player.Stream)
	Status() player.Status
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
//	PUT  /stream          play something else; the body is a link or search words (POST also works)
func New(p Player, board *metric.Board, version string) http.Handler {
	mux := http.NewServeMux()
	status := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(Info{Version: version, Status: p.Status(), Counting: Counting{
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
		body, err := io.ReadAll(io.LimitReader(r.Body, 2048))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s, err := player.ParseStream(string(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.SetStream(s)
		status(w)
	}
	mux.HandleFunc("PUT /stream", setStream)
	mux.HandleFunc("POST /stream", setStream)
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
