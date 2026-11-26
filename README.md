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
2. Once a second it sets the playback rate of the page's media element to
   match the metric, gliding to a new rate instead of jumping.
3. It keeps the stream alive: it resumes playback if paused, answers the
   "still listening?" prompt, restarts with another track if playback stalls,
   and opens a new tab if the old one was closed.
4. On exit it pauses playback.

## Requirements

- macOS with [cmux](https://cmux.com) installed and running
- Go 1.22 or later to build

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

Pitch is held steady by default, so only the tempo moves. With `-tape` the
pitch follows the tempo, like slowing down a tape machine, which makes the
difference easier to hear from across the room.

## Flags for `dubd run`

| Flag           | Default          | Meaning                                                              |
| -------------- | ---------------- | -------------------------------------------------------------------- |
| `-query`       | `ai dub reggae`  | YouTube Music search; the stream is the radio of a top result        |
| `-url`         |                  | Play this YouTube Music URL (a playlist, say) instead of searching   |
| `-metric`      | `0`              | Metric value to start with                                           |
| `-metric-min`  | `0`              | Metric value that plays at `-rate-min`                               |
| `-metric-max`  | `12`             | Metric value that plays at `-rate-max`                               |
| `-rate-min`    | `0.6`            | Slowest playback rate                                                |
| `-rate-max`    | `1.6`            | Fastest playback rate                                                |
| `-tape`        | off              | Let pitch follow tempo                                               |
| `-glide`       | `0.05`           | Largest playback-rate change per second; `0` jumps                   |
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

## Notes

- **Not a container.** cmux is a macOS app and its control socket checks that
  the caller was started inside a cmux terminal, so the daemon runs on the
  host. To run it from somewhere else, such as launchd, cmux has to be
  started with `CMUX_SOCKET_MODE=allowAll`, which lets any local process
  control it.
- **YouTube Music account.** Playback works signed out, with ads. Sign in to
  YouTube Music in the cmux browser tab for a Premium account's ad-free
  playback.
- **The tab is remembered.** A restarted daemon takes over the tab it opened
  before instead of opening another.

## Development

```sh
go test ./...
```

The tests run against a fake cmux, so they need neither cmux nor a network.

## License

MIT
