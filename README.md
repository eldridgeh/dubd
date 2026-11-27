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

Then feed it the number. There are three ways, and one instance takes all
of them; whichever was used last is the one in force.

**1. You have the total.** Send one number:

```sh
dubd set 7                                   # from any terminal
curl -X PUT -d 7 http://127.0.0.1:8382/metric   # from anything that speaks HTTP
dubd run -metric-cmd 'landings-per-hour'     # dubd runs a command every 30s
dubd run -metric-file ~/.landings            # dubd reads a file every 30s
```

**2. Each agent has its own number.** Every agent reports its own and
`dubd` adds them up:

```sh
dubd set 3 -from agent-12
curl -X PUT -d 3 http://127.0.0.1:8382/metric/agent-12
dubd forget agent-12                         # drop an agent
```

`-combine avg` or `-combine max` on `dubd run` averages or takes the highest
instead of adding. `-agent-ttl 15m` drops an agent that has not reported for
that long.

**3. Nobody has a number.** Every agent reports each landing as it happens,
and `dubd` works out landings per hour over the last hour:

```sh
dubd land -from agent-12                     # one landing
dubd land                                    # no agent named
curl -X POST http://127.0.0.1:8382/land/agent-12
```

`-window 15m` on `dubd run` counts over a shorter span, still as a rate per
hour.

`dubd status` shows which way is in force and what each agent contributes.
Many agents can report at once. The tempo changes at most once every
`-settle` (10 seconds by default), so a burst of reports is one change.

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

## Search, save and history

`dubd` keeps a small library in one JSON file: the results of the last
search, streams you saved under a name, and the tracks that have played.

**Pick from a search.** `dubd search` lists what YouTube Music finds, in a
tab of its own that it closes again, so the music carries on. A number then
plays the radio of that result:

```sh
dubd search heavy dub techno
#   1  Deep Dub Techno Mix — Video • Some Channel • 1.2M views
#   2  ...
dubd stream 2                # play result 2
dubd search                  # show the last results again
```

**Save a stream under a name** and play it by that name later:

```sh
dubd save friday             # what is playing now
dubd save steppers 2         # result 2 of the last search
dubd save mine 'https://music.youtube.com/playlist?list=...'
dubd stream friday           # play it
dubd saved                   # list the names
dubd unsave friday
```

**See what played.** Every track is recorded with a link to that one song:

```sh
dubd history                 # the latest 20; -n 0 lists all that are kept
# 2026-11-26 18:30  Track title — Artist
#                   https://music.youtube.com/watch?v=...
```

With `dubd stream`, a number is always a result of the last search and a
saved name wins over search words. The file is
`library.json` in `dubd`'s folder under your configuration directory
(`~/Library/Application Support/dubd/` on macOS); `dubd run` prints its path
at start and `-library` puts it somewhere else. It is plain JSON, so you can
read or edit it while `dubd` is stopped. It keeps the latest 500 tracks, and
each named instance has its own file.

## Several instances

To give each agent, team or project its own music, run one instance each.
Give every instance its own name and port; a name keeps that instance's
browser tab separate from the others'.

```sh
dubd run -name alpha -addr 127.0.0.1:8401 -query 'ai dub reggae'
dubd run -name beta  -addr 127.0.0.1:8402 -query 'heavy dub techno'
```

Point each sender at its own instance with `-addr`, or once with the
`DUBD_ADDR` environment variable:

```sh
dubd set 7 -addr 127.0.0.1:8401
DUBD_ADDR=127.0.0.1:8402 dubd set 3
```

Each instance is one playing tab, so instances on the same machine are all
heard at once.

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

The rate changes in one step when the metric changes, with a half-second dip in
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
| `-settle`      | `10s`            | Shortest time between tempo changes                                  |
| `-combine`     | `sum`            | How agents' numbers become the metric: `sum`, `avg` or `max`         |
| `-agent-ttl`   | `0` (never)      | Drop an agent that has not reported for this long                    |
| `-window`      | `1h`             | Span over which reported landings are counted                        |
| `-addr`        | `127.0.0.1:8382` | Address the HTTP API listens on; `$DUBD_ADDR` changes the default    |
| `-name`        |                  | Name of this instance when running several; keeps its tab separate   |
| `-library`     |                  | JSON file for saved streams, the last search and the tracks played   |
| `-cmux`        |                  | Path to the cmux CLI; by default `$PATH`, then the cmux app          |

## HTTP API

The API listens on localhost only.

| Request              | Effect                                             |
| -------------------- | -------------------------------------------------- |
| `GET /status`        | State as JSON                                      |
| `GET /metric`        | The metric as plain text                           |
| `PUT /metric`        | Set the whole metric; the body is an integer (or `POST`) |
| `PUT /metric/{agent}` | Set one agent's number (or `POST`)                |
| `DELETE /metric/{agent}` | Forget one agent                               |
| `POST /land`         | Count one landing; a body gives a count            |
| `POST /land/{agent}` | Count a landing for an agent                       |
| `POST /pause`        | Pause the stream                                   |
| `POST /play`         | Resume the stream                                  |
| `POST /next`         | Move to another stream                             |
| `PUT /stream`        | Play something else; the body is a link, search words, a saved name or a result number (or `POST`) |
| `GET /search`        | The results of the last search                     |
| `POST /search`       | Search; the body is the words                      |
| `GET /history`       | The tracks that have played                        |
| `GET /saved`         | The saved streams                                  |
| `PUT /saved/{name}`  | Save a stream; the body is as for `/stream`, or empty for what is playing (or `POST`) |
| `DELETE /saved/{name}` | Remove a saved stream                            |

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
