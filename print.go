package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/eldridgeh/dubd/internal/library"
	"github.com/eldridgeh/dubd/internal/player"
)

// trackLine is a track as one line: its title, then who it is by.
func trackLine(t player.Track) string {
	if t.By == "" {
		return t.Title
	}
	return t.Title + " — " + t.By
}

// streamLine is a stream as one line: its link, or the words searched for.
func streamLine(s player.Stream) string {
	if s.URL != "" {
		return s.URL
	}
	return "search: " + s.Query
}

// printResults turns the daemon's answer to a search into numbered lines.
func printResults(raw []byte) ([]byte, error) {
	var r library.Results
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if len(r.Tracks) == 0 {
		b.WriteString("No search yet. Run: dubd search <words>\n")
		return b.Bytes(), nil
	}
	fmt.Fprintf(&b, "Results for %q:\n", r.Query)
	for i, t := range r.Tracks {
		fmt.Fprintf(&b, "%3d  %s\n", i+1, trackLine(t))
	}
	b.WriteString("Play one with: dubd stream <number>\n")
	return b.Bytes(), nil
}

// printHistory lists the latest count tracks, oldest first; a count of 0
// lists them all.
func printHistory(raw []byte, count int) ([]byte, error) {
	var plays []library.Play
	if err := json.Unmarshal(raw, &plays); err != nil {
		return nil, err
	}
	if count > 0 && len(plays) > count {
		plays = plays[len(plays)-count:]
	}
	var b bytes.Buffer
	if len(plays) == 0 {
		b.WriteString("Nothing has played yet.\n")
	}
	for _, p := range plays {
		fmt.Fprintf(&b, "%s  %s\n                  %s\n", p.At.In(time.Local).Format("2006-01-02 15:04"), trackLine(p.Track), p.Link)
	}
	return b.Bytes(), nil
}

// printSaved lists the saved streams.
func printSaved(raw []byte) ([]byte, error) {
	var saved []library.Named
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if len(saved) == 0 {
		b.WriteString("No streams are saved. Save what is playing with: dubd save <name>\n")
	}
	width := 0
	for _, s := range saved {
		width = max(width, len(s.Name))
	}
	for _, s := range saved {
		fmt.Fprintf(&b, "%-*s  %s\n", width, s.Name, streamLine(s.Stream))
	}
	return b.Bytes(), nil
}
