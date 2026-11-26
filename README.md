# dubd

[![CI](https://github.com/eldridgeh/dubd/actions/workflows/ci.yml/badge.svg)](https://github.com/eldridgeh/dubd/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/eldridgeh/dubd.svg)](https://pkg.go.dev/github.com/eldridgeh/dubd)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A small Go daemon that plays a continuous stream of dub in a
[cmux](https://cmux.com) browser tab and scales its tempo to an integer you
give it, such as code merges landed per hour. A slow drag means not much is
landing. A quick skank means the queue is moving. Silence means the daemon
has stopped.

## Quick start

In a terminal tab inside cmux:

```sh
go install github.com/eldridgeh/dubd@latest
dubd run -metric-cmd 'your-command-that-prints-landings-per-hour'
```

A browser tab opens beside your terminal and the dub starts within about 15
seconds. `dubd` runs the command every 30 seconds and the tempo follows the
number it prints. No command to hand? Run `dubd run` and push the number in
with `dubd set 7` or an HTTP `PUT` (see [Run](#run)).

One binary, no dependencies outside the Go standard library, no
configuration file.

## How it works

1. `dubd run` opens a browser tab in cmux (without taking focus), searches
   YouTube Music, and starts the "radio" of a top result, which is an endless
   queue.
2. Once a second it checks the page's media element and holds its playback
   rate to the one the metric calls for.
3. It keeps the stream alive: it resumes playback if paused, answers the
   "still listening?" prompt, restarts with another track if playback stalls,
   and opens a new tab if the old one was closed.
4. On exit it pauses playback.

This has been run against cmux 0.65 on macOS 14.

## Requirements

- macOS with [cmux](https://cmux.com) installed and running
- Go 1.25 or later to build

## Install

```sh
go install github.com/eldridgeh/dubd@latest
```

Or from a checkout: `go build -o dubd .`

## Run

Start it **from a terminal tab inside cmux**. By default cmux only accepts
commands from processes started in its own terminals.

```sh
dubd run
```

Then feed it the number, whichever way suits:

```sh
dubd set 7                                   # from any terminal
curl -X PUT -d 7 http://127.0.0.1:8382/metric   # from anything that speaks HTTP
dubd run -metric-cmd 'landings-per-hour'     # dubd runs a command every 30s
dubd run -metric-file ~/.landings            # dubd reads a file every 30s
```

`dubd status` prints the metric, the playback rate and what is playing.
`dubd pause` and `dubd play` stop and resume the stream.

## Choosing the music

By default `dubd` searches YouTube Music for "ai dub reggae" and plays the
radio of a top result. Play anything else, at start or while it runs:

```sh
dubd run -query 'heavy dub techno'                 # a different search
dubd run -url 'https://music.youtube.com/playlist?list=...'   # your playlist

dubd stream heavy dub techno                       # switch while running
dubd stream 'https://music.youtube.com/playlist?list=...'
dubd next                                          # another result of the same search
```

A link can be a playlist, an album or a single track on YouTube Music or
YouTube. A playlist that ends starts again from the top. A private playlist
needs the tab signed in to the account that owns it.

## Adverts and signing in

Signed out, YouTube Music plays adverts between tracks. `dubd` clicks "Skip"
when YouTube offers it and shows `"ad": true` in `dubd status` while one
plays, but the real fix is a YouTube Premium account:

- **Sign in once in the tab.** Click "Sign in" in the YouTube Music tab that
  `dubd` opened. cmux keeps the login, so it survives restarts of `dubd` and
  of cmux.
- **Or bring your login over from another browser.** In a cmux terminal, run
  `cmux browser import --from chrome` (or `firefox`, `arc` and so on), or
  `cmux browser import --interactive` to pick from the browsers cmux finds.
  Add `--domain youtube.com` to bring over only YouTube's cookies. Safari is
  not supported by cmux's importer.

`dubd status` shows `"signed_in": true` once the tab is signed in. `dubd`
never sees or stores your credentials.

## Tempo

The metric maps linearly onto a playback rate and is clamped at both ends.
The defaults:

| Landings per hour | Playback rate |
| ----------------- | ------------- |
| 0                 | 0.6x          |
| 5                 | about 1.0x    |
| 12 or more        | 1.6x          |

Change the range with `-metric-min`, `-metric-max`, `-rate-min` and
`-rate-max`. Rates from 0.25x to 4x are accepted.

The rate changes once per change of the metric, with a half-second dip in
volume around it. The browser engine cmux uses (WebKit) drops the audio for a
moment whenever the playback rate changes; the dip puts that drop in silence,
so a tempo change sounds like a short swell and not a skip.

Pitch is held steady by default, so only the tempo moves. With `-tape` the
pitch follows the tempo, like slowing down a tape machine, which makes the
difference easier to hear from across the room.

## Flags for `dubd run`

| Flag           | Default          | Meaning                                                              |
| -------------- | ---------------- | -------------------------------------------------------------------- |
| `-query`       | `ai dub reggae`  | YouTube Music search; the stream is the radio of a top result        |
| `-url`         |                  | Play this YouTube Music link (playlist, album, track), not a search  |
| `-metric`      | `0`              | Metric value to start with                                           |
| `-metric-min`  | `0`              | Metric value that plays at `-rate-min`                               |
| `-metric-max`  | `12`             | Metric value that plays at `-rate-max`                               |
| `-rate-min`    | `0.6`            | Slowest playback rate                                                |
| `-rate-max`    | `1.6`            | Fastest playback rate                                                |
| `-tape`        | off              | Let pitch follow tempo                                               |
| `-metric-cmd`  |                  | Shell command that prints the metric                                 |
| `-metric-file` |                  | File that holds the metric                                           |
| `-interval`    | `30s`            | How often the command or file is read                                |
| `-stale-after` | `0` (never)      | Drop to `-rate-min` when the metric has not been updated this long   |
| `-addr`        | `127.0.0.1:8382` | Address the HTTP API listens on                                      |
| `-cmux`        |                  | Path to the cmux CLI; by default `$PATH`, then the cmux app          |

## HTTP API

The API listens on localhost only.

| Request              | Effect                                             |
| -------------------- | -------------------------------------------------- |
| `GET /status`        | State as JSON                                      |
| `GET /metric`        | The metric as plain text                           |
| `PUT /metric`        | Set the metric; the body is an integer (or `POST`) |
| `POST /pause`        | Pause the stream                                   |
| `POST /play`         | Resume the stream                                  |
| `POST /next`         | Move to another stream                             |
| `PUT /stream`        | Play something else; the body is a link or search words (or `POST`) |

## Notes

- **Not a container.** cmux is a macOS app and its control socket checks that
  the caller was started inside a cmux terminal, so the daemon runs on the
  host. To run it from somewhere else, such as launchd, cmux has to be
  started with `CMUX_SOCKET_MODE=allowAll`, which lets any local process
  control it.
- **The tab is remembered.** A restarted daemon takes over the tab it opened
  before instead of opening another.

## Development

```sh
go test ./...
```

The tests run against a fake cmux, so they need neither cmux nor a network.

## License

MIT
