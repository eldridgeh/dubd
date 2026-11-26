package metric

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Combine names how per-agent values become one metric.
type Combine string

const (
	Sum Combine = "sum"
	Avg Combine = "avg"
	Max Combine = "max"
)

// ParseCombine reads a Combine from a flag value.
func ParseCombine(s string) (Combine, error) {
	switch c := Combine(s); c {
	case Sum, Avg, Max:
		return c, nil
	}
	return "", fmt.Errorf("combine must be sum, avg or max, not %q", s)
}

// MaxAgents bounds how many agents a board keeps.
const MaxAgents = 1000

var agentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidAgent reports whether name can be used as an agent name.
func ValidAgent(name string) bool { return agentRE.MatchString(name) }

// Agent is one agent's reported value.
type Agent struct {
	Name    string    `json:"name"`
	Value   int       `json:"value"`
	Updated time.Time `json:"updated"`
}

// The ways a board can hold the metric.
const (
	ModeTotal    = "total"    // one caller sets the whole number
	ModeAgents   = "agents"   // agents report their own numbers; the board combines them
	ModeLandings = "landings" // callers report each landing; the board counts them per hour
)

// MaxLandings bounds how many landing reports a board keeps.
const MaxLandings = 100000

type landing struct {
	at    time.Time
	n     int
	agent string
}

// Board holds the metric in one of three ways: one caller sets the whole
// number; agents each report their own number and the board combines
// them; or callers report each landing and the board counts landings per
// hour over a sliding window. Whichever way was used last is the one in
// force, and switching forgets what the other ways had recorded.
type Board struct {
	// OnChange is called with the metric and where it came from after
	// every report, and when agents expire. Calls are made in the order
	// the changes happened, with the board locked, so OnChange must not
	// call back into the board.
	OnChange func(total int, source string)

	combine Combine
	ttl     time.Duration
	window  time.Duration
	now     func() time.Time

	mu       sync.Mutex
	mode     string
	whole    int
	agents   map[string]Agent
	landings []landing
	landed   int64 // landings in the window, kept in step with landings
	last     int   // the metric as last reported, to notice landings ageing out
}

// NewBoard returns a board holding the whole number start. Agents that
// have not reported for ttl are dropped; a ttl of zero keeps them. Landings
// are counted over window, which defaults to an hour.
func NewBoard(start int, combine Combine, ttl, window time.Duration) *Board {
	if window <= 0 {
		window = time.Hour
	}
	return &Board{combine: combine, ttl: ttl, window: window, now: time.Now,
		mode: ModeTotal, whole: start, last: start, agents: map[string]Agent{}}
}

// Set sets the whole number and forgets any agents and landings.
func (b *Board) Set(v int, source string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.use(ModeTotal)
	b.whole = v
	b.report(source, true)
}

// use switches to mode, forgetting what the other ways recorded. Caller
// holds mu.
func (b *Board) use(mode string) {
	if b.mode == mode {
		return
	}
	b.mode, b.whole, b.landings, b.landed = mode, 0, nil, 0
	clear(b.agents)
}

// Land records n landings by agent (which may be empty) and returns the
// metric: landings per hour over the window.
func (b *Board) Land(agent string, n int) (int, error) {
	if agent != "" && !ValidAgent(agent) {
		return 0, errAgentName
	}
	if n < 1 || n > 1000 {
		return 0, errors.New("the number of landings must be between 1 and 1000")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.use(ModeLandings)
	b.prune()
	if len(b.landings) >= MaxLandings {
		return 0, fmt.Errorf("too many landings in the window (limit %d reports)", MaxLandings)
	}
	b.landings = append(b.landings, landing{at: b.now(), n: n, agent: agent})
	b.landed += int64(n)
	return b.report(ModeLandings, true), nil
}

// prune drops landings that have left the window. Caller holds mu.
func (b *Board) prune() {
	cut := b.now().Add(-b.window)
	i := 0
	for i < len(b.landings) && !b.landings[i].at.After(cut) {
		b.landed -= int64(b.landings[i].n)
		i++
	}
	b.landings = b.landings[i:]
}

var errAgentName = errors.New("an agent name may use letters, digits, dot, dash and underscore, up to 64 characters")

// SetAgent records one agent's value and returns the combined metric.
func (b *Board) SetAgent(name string, v int) (int, error) {
	if !ValidAgent(name) {
		return 0, errAgentName
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.use(ModeAgents)
	if _, known := b.agents[name]; !known && len(b.agents) >= MaxAgents {
		return 0, fmt.Errorf("too many agents (limit %d)", MaxAgents)
	}
	b.agents[name] = Agent{Name: name, Value: v, Updated: b.now()}
	return b.report(ModeAgents, true), nil
}

// Forget drops one agent and reports whether it was known.
func (b *Board) Forget(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, known := b.agents[name]; !known {
		return false
	}
	delete(b.agents, name)
	b.report(ModeAgents, true)
	return true
}

// Expire drops agents that have gone quiet and landings that have left the
// window, and reports the metric if that changed it. Call it now and then.
func (b *Board) Expire() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ttl > 0 {
		for name, a := range b.agents {
			if b.now().Sub(a.Updated) > b.ttl {
				delete(b.agents, name)
			}
		}
	}
	b.report(b.mode, false)
}

// report works out the metric and passes it to OnChange: always after a
// report from outside, and otherwise only if it moved. The value last
// passed on changes nowhere else, so a move is never lost. Caller holds mu.
func (b *Board) report(source string, always bool) int {
	total := b.total()
	if always || total != b.last {
		b.last = total
		b.changed(total, source)
	}
	return total
}

// Total returns the metric.
func (b *Board) Total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total()
}

// Agents returns what each agent contributes, by name: its reported number,
// or, when landings are counted, its landings in the window. It is empty
// when the whole number was set directly.
func (b *Board) Agents() []Agent {
	b.mu.Lock()
	defer b.mu.Unlock()
	byName := map[string]Agent{}
	for _, a := range b.agents {
		byName[a.Name] = a
	}
	if b.mode == ModeLandings {
		b.prune()
		for _, l := range b.landings {
			if l.agent == "" {
				continue
			}
			a := byName[l.agent]
			byName[l.agent] = Agent{Name: l.agent, Value: a.Value + l.n, Updated: l.at}
		}
	}
	out := make([]Agent, 0, len(byName))
	for _, a := range byName {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Mode returns which way the metric is held now.
func (b *Board) Mode() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mode
}

// Window returns the span landings are counted over.
func (b *Board) Window() time.Duration { return b.window }

// Combine returns how agents are combined.
func (b *Board) Combine() Combine { return b.combine }

// total returns the metric for the mode in force. Caller holds mu.
func (b *Board) total() int {
	switch b.mode {
	case ModeTotal:
		return b.whole
	case ModeLandings:
		b.prune()
		count := b.landed
		// Per hour, whatever the window, rounded to the nearest whole.
		// Worked in floating point: the count times an hour in
		// nanoseconds does not fit in an integer for large counts.
		perHour := math.Round(float64(count) * float64(time.Hour) / float64(b.window))
		return int(math.Min(perHour, math.MaxInt32))
	}
	if len(b.agents) == 0 {
		return 0
	}
	sum, most, first := 0, 0, true
	for _, a := range b.agents {
		sum += a.Value
		if first || a.Value > most {
			most, first = a.Value, false
		}
	}
	switch b.combine {
	case Avg:
		// Round to the nearest whole number.
		n := len(b.agents)
		if sum >= 0 {
			return (sum + n/2) / n
		}
		return (sum - n/2) / n
	case Max:
		return most
	}
	return sum
}

func (b *Board) changed(total int, source string) {
	if b.OnChange != nil {
		b.OnChange(total, source)
	}
}
