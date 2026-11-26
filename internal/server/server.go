// Package server is dubd's local HTTP API.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/eldridgeh/dubd/internal/metric"
	"github.com/eldridgeh/dubd/internal/player"
)

// Player is what the API controls.
type Player interface {
	SetMetric(v int, source string)
	SetPaused(paused bool)
	Next()
	SetStream(s player.Stream)
	Status() player.Status
}

// Info is what GET /status returns.
type Info struct {
	Version string `json:"version"`
	player.Status
}

// New returns the API handler.
//
//	GET  /status          state as JSON
//	GET  /metric          the metric as plain text
//	PUT  /metric          set the metric; the body is an integer (POST also works)
//	POST /pause, /play    pause or resume the stream
//	POST /next            move to another stream
//	PUT  /stream          play something else; the body is a link or search words (POST also works)
func New(p Player, version string) http.Handler {
	mux := http.NewServeMux()
	status := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(Info{Version: version, Status: p.Status()})
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) { status(w) })
	mux.HandleFunc("GET /metric", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, p.Status().Metric)
	})
	setMetric := func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		v, err := metric.Parse(string(body))
		if err != nil {
			http.Error(w, "the body must be an integer: "+err.Error(), http.StatusBadRequest)
			return
		}
		p.SetMetric(v, "http")
		status(w)
	}
	mux.HandleFunc("PUT /metric", setMetric)
	mux.HandleFunc("POST /metric", setMetric)
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
