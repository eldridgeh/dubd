// Command dubd plays a continuous YouTube Music stream in a cmux browser tab
// at a tempo that follows an integer metric.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/eldridgeh/dubd/internal/cmux"
	"github.com/eldridgeh/dubd/internal/library"
	"github.com/eldridgeh/dubd/internal/metric"
	"github.com/eldridgeh/dubd/internal/player"
	"github.com/eldridgeh/dubd/internal/server"
	"github.com/eldridgeh/dubd/internal/tempo"
)

const defaultAddr = "127.0.0.1:8382"

// addrDefault is the address used when -addr is not given: $DUBD_ADDR if
// set, so that a shell or an agent can be pointed at its own instance.
func addrDefault() string {
	if a := os.Getenv("DUBD_ADDR"); a != "" {
		return a
	}
	return defaultAddr
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// version is set at release time with -ldflags "-X main.version=...".
var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

const usage = `dubd plays a continuous YouTube Music stream in a cmux browser tab, at a
tempo that follows an integer metric.

Usage:
  dubd run [flags]     start the daemon (from a cmux terminal tab)
  dubd set <integer>   tell the running daemon the whole metric
  dubd set <integer> -from <agent>
                       report one agent's number; the daemon adds the agents up
  dubd land [-from <agent>] [count]
                       report a landing; the daemon works out landings per hour
  dubd forget <agent>  drop one agent's number
  dubd status          print the running daemon's state
  dubd pause | play    pause or resume the stream
  dubd next            move to another stream
  dubd dip <time>      how long the dip in volume around a change of tempo
                       lasts, such as 300ms; 0 for none
  dubd stream <link | search words>
                       play something else: a YouTube Music link, or a search
  dubd search <words>  list what a search finds, without changing the music
  dubd search          list the last search again
  dubd stream <number> play that result of the last search
  dubd save <name> [link | search words | number]
                       keep a stream under a name; with only a name, what is
                       playing now
  dubd stream <name>   play a saved stream
  dubd saved           list the saved streams
  dubd unsave <name>   remove a saved stream
  dubd history [-n 20] list the tracks that have played, with their links
  dubd version

Run "dubd run -h" for the daemon's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "run":
		err = run(args)
	case "set", "land", "forget":
		err = client(args, cmd)
	case "status":
		err = client(args, "status")
	case "pause":
		err = client(args, "pause")
	case "play":
		err = client(args, "play")
	case "next":
		err = client(args, "next")
	case "stream", "search", "history", "saved", "save", "unsave", "dip":
		err = client(args, cmd)
	case "version", "--version":
		fmt.Println("dubd", buildVersion())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "dubd: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dubd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("dubd run", flag.ContinueOnError)
	var (
		addr       = fs.String("addr", addrDefault(), "address the local HTTP API listens on; $DUBD_ADDR sets the default")
		name       = fs.String("name", "", "name of this instance, when running several; each keeps its own browser tab")
		query      = fs.String("query", "ai dub reggae", "YouTube Music search; the stream is the radio of a top result")
		streamURL  = fs.String("url", "", "play this YouTube Music link (a playlist, album or track) instead of searching")
		start      = fs.Int("metric", 0, "metric value to start with")
		metricMin  = fs.Int("metric-min", 0, "metric value that plays at rate-min")
		metricMax  = fs.Int("metric-max", 12, "metric value that plays at rate-max")
		rateMin    = fs.Float64("rate-min", 0.6, "slowest playback rate")
		rateMax    = fs.Float64("rate-max", 1.6, "fastest playback rate")
		tape       = fs.Bool("tape", false, "let pitch follow tempo, like a tape machine, instead of holding pitch")
		metricCmd  = fs.String("metric-cmd", "", "shell command that prints the metric; run every -interval")
		metricFile = fs.String("metric-file", "", "file that holds the metric; read every -interval")
		interval   = fs.Duration("interval", 30*time.Second, "how often -metric-cmd or -metric-file is read")
		staleAfter = fs.Duration("stale-after", 0, "play at rate-min when the metric has not been updated this long (0 never)")
		dip        = fs.Duration("dip", 550*time.Millisecond, "how long the dip in volume around a change of tempo lasts (0 for none)")
		settle     = fs.Duration("settle", 10*time.Second, "shortest time between tempo changes, so a burst of reports is one change")
		combine    = fs.String("combine", "sum", "how agents' numbers become the metric: sum, avg or max")
		agentTTL   = fs.Duration("agent-ttl", 0, "drop an agent that has not reported for this long (0 never)")
		window     = fs.Duration("window", time.Hour, "span over which reported landings are counted")
		libPath    = fs.String("library", "", "JSON file that keeps saved streams, the last search and the tracks played (default: in the user's configuration directory, one per -name)")
		cmuxBin    = fs.String("cmux", "", "path to the cmux CLI (default: $PATH, then the cmux app)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	how, err := metric.ParseCombine(*combine)
	if err != nil {
		return fmt.Errorf("-%w", err)
	}
	curve := tempo.Curve{MetricMin: *metricMin, MetricMax: *metricMax, RateMin: *rateMin, RateMax: *rateMax}
	if err := curve.Validate(); err != nil {
		return err
	}
	switch {
	case *metricCmd != "" && *metricFile != "":
		return errors.New("use -metric-cmd or -metric-file, not both")
	case *name != "" && !nameRE.MatchString(*name):
		return errors.New("-name may use letters, digits, dot, dash and underscore, up to 64 characters")
	case *dip < 0 || *dip > player.MaxDip:
		return fmt.Errorf("-dip must be from 0 to %s", player.MaxDip)
	case *settle < 0:
		return errors.New("-settle must not be negative")
	case *agentTTL < 0:
		return errors.New("-agent-ttl must not be negative")
	case *window < time.Minute:
		return errors.New("-window must be at least 1m")
	case *interval < time.Second:
		return errors.New("-interval must be at least 1s")
	}
	stream := player.Stream{Query: strings.TrimSpace(*query)}
	if *streamURL != "" {
		var err error
		if stream, err = player.ParseStream(*streamURL); err != nil {
			return fmt.Errorf("-url: %w", err)
		}
		if stream.URL == "" {
			return errors.New("-url must be a link; use -query for search words")
		}
	} else if stream.Query == "" {
		return errors.New("-query must not be empty")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *libPath == "" {
		*libPath = libraryFile(*name)
	}
	lib, err := library.Open(*libPath)
	if err != nil {
		return fmt.Errorf("-library: %w", err)
	}
	bin, err := cmux.Find(*cmuxBin)
	if err != nil {
		return err
	}
	browser := cmux.New(bin)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := browser.Check(ctx); err != nil {
		return err
	}

	cfg := player.Config{
		Stream: stream, Curve: curve, KeepPitch: !*tape,
		Dip: *dip, Settle: *settle, StaleAfter: *staleAfter, SurfaceFile: surfaceFile(*name),
		OnResults: func(query string, tracks []player.Track) {
			if _, err := lib.SetResults(query, tracks); err != nil {
				log.Error("writing the library", "error", err)
			}
		},
		OnTrack: func(t player.Track) {
			if err := lib.Played(t); err != nil {
				log.Error("writing the library", "error", err)
			}
		},
	}
	p := player.New(cfg, browser, log, *start)
	browser.OnDialog = p.NoteDialog
	board := metric.NewBoard(*start, how, *agentTTL, *window)
	board.OnChange = p.SetMetric

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("%w (is dubd already running? a second instance needs its own -addr and -name)", err)
	}
	srv := &http.Server{Handler: server.New(p, board, lib, buildVersion()), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "error", err)
		}
	}()
	log.Info("dubd running", "version", buildVersion(), "name", *name, "api", "http://"+ln.Addr().String(), "library", *libPath,
		"metric", *start, "rate", curve.Rate(*start))

	var src metric.Source
	source := ""
	switch {
	case *metricCmd != "":
		src, source = metric.Command(*metricCmd), "cmd"
	case *metricFile != "":
		src, source = metric.File(*metricFile), "file"
	}
	if src != nil {
		go metric.Poll(ctx, src, *interval,
			func(v int) { board.Set(v, source) },
			func(err error) { log.Error("reading metric", "source", source, "error", err) })
	}

	// Agents that go quiet and landings that leave the window change the
	// metric with nobody reporting.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				board.Expire()
			}
		}
	}()

	p.Run(ctx) // returns once ctx ends and playback is paused
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	log.Info("dubd stopped; playback paused")
	return nil
}

// surfaceFile is where an instance remembers its browser tab between runs.
// Each named instance has its own, so several can run side by side.
func surfaceFile(name string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	if name == "" {
		return filepath.Join(dir, "dubd", "surface")
	}
	return filepath.Join(dir, "dubd", "surface."+name)
}

// libraryFile is where an instance keeps its saved streams, last search and
// played tracks. Each named instance has its own.
func libraryFile(name string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	if name == "" {
		return filepath.Join(dir, "dubd", "library.json")
	}
	return filepath.Join(dir, "dubd", "library."+name+".json")
}

// flagsFirst moves flags ahead of the other arguments, so that
// "dubd set 3 -from a" works as well as "dubd set -from a 3". Every flag of
// the client commands takes a value.
func flagsFirst(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(append(flags, "--"), append(rest, args[i+1:]...)...)
		case len(a) > 1 && a[0] == '-' && !isNumber(a):
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			rest = append(rest, a)
		}
	}
	return append(flags, rest...)
}

func isNumber(s string) bool {
	_, err := metric.Parse(s)
	return err == nil
}

// client sends one request to the running daemon and prints the answer.
func client(args []string, cmd string) error {
	fs := flag.NewFlagSet("dubd "+cmd, flag.ContinueOnError)
	addr := fs.String("addr", addrDefault(), "address of the running daemon; $DUBD_ADDR sets the default")
	from := ""
	if cmd == "set" || cmd == "land" {
		fs.StringVar(&from, "from", "", "the agent reporting")
	}
	count := 20
	if cmd == "history" {
		fs.IntVar(&count, "n", count, "how many of the latest tracks to list; 0 lists all that are kept")
	}
	if cmd != "stream" && cmd != "search" && cmd != "save" { // words are taken as they come
		args = flagsFirst(args)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if from != "" && !metric.ValidAgent(from) {
		return errors.New("-from may use letters, digits, dot, dash and underscore, up to 64 characters")
	}
	method, path, body := http.MethodPost, "/"+cmd, ""
	timeout := 5 * time.Second
	switch cmd {
	case "status":
		method = http.MethodGet
	case "set":
		if fs.NArg() != 1 {
			return errors.New("usage: dubd set [-addr host:port] [-from agent] <integer>")
		}
		if _, err := metric.Parse(fs.Arg(0)); err != nil {
			return err
		}
		method, path, body = http.MethodPut, "/metric", fs.Arg(0)
		if from != "" {
			path += "/" + from
		}
	case "land":
		if fs.NArg() > 1 {
			return errors.New("usage: dubd land [-addr host:port] [-from agent] [count]")
		}
		if fs.NArg() == 1 {
			if _, err := metric.Parse(fs.Arg(0)); err != nil {
				return err
			}
			body = fs.Arg(0)
		}
		if from != "" {
			path += "/" + from
		}
	case "forget":
		if fs.NArg() != 1 || !metric.ValidAgent(fs.Arg(0)) {
			return errors.New("usage: dubd forget [-addr host:port] <agent>")
		}
		method, path = http.MethodDelete, "/metric/"+fs.Arg(0)
	case "stream":
		body = strings.Join(fs.Args(), " ")
		if _, err := player.ParseStream(body); err != nil {
			return fmt.Errorf("usage: dubd stream [-addr host:port] <link | search words>: %w", err)
		}
		method = http.MethodPut
	case "dip":
		if fs.NArg() != 1 {
			return errors.New("usage: dubd dip [-addr host:port] <time, such as 300ms, or 0>")
		}
		if _, err := time.ParseDuration(fs.Arg(0)); err != nil {
			return fmt.Errorf("usage: dubd dip <time, such as 300ms, or 0>: %w", err)
		}
		method, body = http.MethodPut, fs.Arg(0)
	case "search":
		// No words lists the last search again.
		if body = strings.Join(fs.Args(), " "); body == "" {
			method = http.MethodGet
		}
		timeout = time.Minute // the daemon loads a page
	case "history", "saved":
		method = http.MethodGet
		if count < 0 {
			return errors.New("-n must not be negative")
		}
	case "save":
		if fs.NArg() < 1 || !library.ValidName(fs.Arg(0)) {
			return errors.New("usage: dubd save [-addr host:port] <name> [link | search words | number]; a name uses letters, digits, dot, dash and underscore, and is not a number")
		}
		method, path, body = http.MethodPut, "/saved/"+fs.Arg(0), strings.Join(fs.Args()[1:], " ")
	case "unsave":
		if fs.NArg() != 1 || !library.ValidName(fs.Arg(0)) {
			return errors.New("usage: dubd unsave [-addr host:port] <name>")
		}
		method, path = http.MethodDelete, "/saved/"+fs.Arg(0)
	}
	switch cmd {
	case "status", "pause", "play", "next", "history", "saved":
		if fs.NArg() > 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
	}
	req, err := http.NewRequest(method, "http://"+*addr+path, strings.NewReader(body))
	if err != nil {
		return err
	}
	httpClient := &http.Client{Timeout: timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("no dubd daemon answering at %s: %w", *addr, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(out)))
	}
	// The library's answers are lists, printed as lines; the rest is the
	// daemon's state, printed as it comes.
	switch cmd {
	case "search":
		out, err = printResults(out)
	case "history":
		out, err = printHistory(out, count)
	case "saved", "save", "unsave":
		out, err = printSaved(out)
	}
	if err != nil {
		return fmt.Errorf("unexpected answer from the daemon: %w", err)
	}
	_, err = os.Stdout.Write(out)
	return err
}
