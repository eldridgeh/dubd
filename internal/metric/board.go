package metric

import (
	"errors"
	"fmt"
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
	// every report, and when agents expire.
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
	last     int // the metric as last reported, to notice landings ageing out
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
	b.use(ModeTotal)
	b.whole, b.last = v, v
	b.mu.Unlock()
	b.changed(v, source)
}

// use switches to mode, forgetting what the other ways recorded. Caller
// holds mu.
func (b *Board) use(mode string) {
	if b.mode == mode {
		return
	}
	b.mode, b.whole, b.landings = mode, 0, nil
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
	b.use(ModeLandings)
	b.prune()
	if len(b.landings) >= MaxLandings {
		b.mu.Unlock()
		return 0, fmt.Errorf("too many landings in the window (limit %d reports)", MaxLandings)
	}
	b.landings = append(b.landings, landing{at: b.now(), n: n, agent: agent})
	total := b.total()
	b.last = total
	b.mu.Unlock()
	b.changed(total, ModeLandings)
	return total, nil
}

// prune drops landings that have left the window. Caller holds mu.
func (b *Board) prune() {
	cut := b.now().Add(-b.window)
	i := 0
	for i < len(b.landings) && !b.landings[i].at.After(cut) {
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
	b.use(ModeAgents)
	if _, known := b.agents[name]; !known && len(b.agents) >= MaxAgents {
		b.mu.Unlock()
		return 0, fmt.Errorf("too many agents (limit %d)", MaxAgents)
	}
	b.agents[name] = Agent{Name: name, Value: v, Updated: b.now()}
	total := b.total()
	b.last = total
	b.mu.Unlock()
	b.changed(total, ModeAgents)
	return total, nil
}

// Forget drops one agent and reports whether it was known.
func (b *Board) Forget(name string) bool {
	b.mu.Lock()
	_, known := b.agents[name]
	delete(b.agents, name)
	total := b.total()
	b.last = total
	b.mu.Unlock()
	if known {
		b.changed(total, ModeAgents)
	}
	return known
}

// Expire drops agents that have gone quiet and landings that have left the
// window, and reports the metric if that changed it. Call it now and then.
func (b *Board) Expire() {
	b.mu.Lock()
	if b.ttl > 0 {
		for name, a := range b.agents {
			if b.now().Sub(a.Updated) > b.ttl {
				delete(b.agents, name)
			}
		}
	}
	b.prune()
	total, mode := b.total(), b.mode
	moved := total != b.last
	b.last = total
	b.mu.Unlock()
	if moved {
		b.changed(total, mode)
	}
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
		count := 0
		for _, l := range b.landings {
			count += l.n
		}
		// Per hour, whatever the window, rounded to the nearest whole.
		return int((int64(count)*int64(time.Hour) + int64(b.window)/2) / int64(b.window))
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
