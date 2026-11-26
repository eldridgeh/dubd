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
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/eldridgeh/dubd/internal/cmux"
	"github.com/eldridgeh/dubd/internal/metric"
	"github.com/eldridgeh/dubd/internal/player"
	"github.com/eldridgeh/dubd/internal/server"
	"github.com/eldridgeh/dubd/internal/tempo"
)

const defaultAddr = "127.0.0.1:8382"

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
  dubd set <integer>   tell the running daemon the metric
  dubd status          print the running daemon's state
  dubd pause | play    pause or resume the stream
  dubd next            move to another stream
  dubd stream <link | search words>
                       play something else: a YouTube Music link, or a search
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
	case "set":
		err = client(args, "set")
	case "status":
		err = client(args, "status")
	case "pause":
		err = client(args, "pause")
	case "play":
		err = client(args, "play")
	case "next":
		err = client(args, "next")
	case "stream":
		err = client(args, "stream")
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
		addr       = fs.String("addr", defaultAddr, "address the local HTTP API listens on")
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
		cmuxBin    = fs.String("cmux", "", "path to the cmux CLI (default: $PATH, then the cmux app)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	curve := tempo.Curve{MetricMin: *metricMin, MetricMax: *metricMax, RateMin: *rateMin, RateMax: *rateMax}
	if err := curve.Validate(); err != nil {
		return err
	}
	switch {
	case *metricCmd != "" && *metricFile != "":
		return errors.New("use -metric-cmd or -metric-file, not both")
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
		StaleAfter: *staleAfter, SurfaceFile: surfaceFile(),
	}
	p := player.New(cfg, browser, log, *start)
	browser.OnDialog = p.NoteDialog

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("%w (is dubd already running?)", err)
	}
	srv := &http.Server{Handler: server.New(p, buildVersion()), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "error", err)
		}
	}()
	log.Info("dubd running", "version", buildVersion(), "api", "http://"+ln.Addr().String(),
		"metric", *start, "rate", curve.Rate(*start))

	var src metric.Source
	name := ""
	switch {
	case *metricCmd != "":
		src, name = metric.Command(*metricCmd), "cmd"
	case *metricFile != "":
		src, name = metric.File(*metricFile), "file"
	}
	if src != nil {
		go metric.Poll(ctx, src, *interval,
			func(v int) { p.SetMetric(v, name) },
			func(err error) { log.Error("reading metric", "source", name, "error", err) })
	}

	p.Run(ctx) // returns once ctx ends and playback is paused
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	log.Info("dubd stopped; playback paused")
	return nil
}

// surfaceFile is where the daemon remembers its browser tab between runs.
func surfaceFile() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "dubd", "surface")
}

// client sends one request to the running daemon and prints the answer.
func client(args []string, cmd string) error {
	fs := flag.NewFlagSet("dubd "+cmd, flag.ContinueOnError)
	addr := fs.String("addr", defaultAddr, "address of the running daemon")
	if err := fs.Parse(args); err != nil {
		return err
	}
	method, path, body := http.MethodPost, "/"+cmd, ""
	switch cmd {
	case "status":
		method = http.MethodGet
	case "set":
		if fs.NArg() != 1 {
			return errors.New("usage: dubd set [-addr host:port] <integer>")
		}
		if _, err := metric.Parse(fs.Arg(0)); err != nil {
			return err
		}
		method, path, body = http.MethodPut, "/metric", fs.Arg(0)
	case "stream":
		body = strings.Join(fs.Args(), " ")
		if _, err := player.ParseStream(body); err != nil {
			return fmt.Errorf("usage: dubd stream [-addr host:port] <link | search words>: %w", err)
		}
		method = http.MethodPut
	}
	if cmd != "set" && cmd != "stream" && fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	req, err := http.NewRequest(method, "http://"+*addr+path, strings.NewReader(body))
	if err != nil {
		return err
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("no dubd daemon answering at %s: %w", *addr, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(out)))
	}
	_, err = os.Stdout.Write(out)
	return err
}
