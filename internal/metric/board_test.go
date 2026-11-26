package metric

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

type change struct {
	total  int
	source string
}

func newBoard(start int, c Combine, ttl time.Duration) (*Board, *[]change, *time.Time) {
	b := NewBoard(start, c, ttl, time.Hour)
	now := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return now }
	var got []change
	b.OnChange = func(total int, source string) { got = append(got, change{total, source}) }
	return b, &got, &now
}

func TestWholeNumber(t *testing.T) {
	b, got, _ := newBoard(4, Sum, 0)
	if b.Total() != 4 || len(b.Agents()) != 0 {
		t.Fatalf("start: total %d agents %v", b.Total(), b.Agents())
	}
	b.Set(9, "http")
	if b.Total() != 9 || len(*got) != 1 || (*got)[0] != (change{9, "http"}) {
		t.Fatalf("after Set: total %d changes %v", b.Total(), *got)
	}
}

func TestAgentsAreAddedUp(t *testing.T) {
	b, got, _ := newBoard(4, Sum, 0)
	for name, v := range map[string]int{"a": 2, "b": 3, "c": 5} {
		if _, err := b.SetAgent(name, v); err != nil {
			t.Fatal(err)
		}
	}
	if b.Total() != 10 {
		t.Fatalf("sum = %d, want 10 (the whole number 4 must not be counted)", b.Total())
	}
	if total, _ := b.SetAgent("b", 1); total != 8 {
		t.Fatalf("after b reports again: %d, want 8", total)
	}
	last := (*got)[len(*got)-1]
	if last != (change{8, "agents"}) {
		t.Fatalf("last change = %v", last)
	}
	agents := b.Agents()
	if len(agents) != 3 || agents[0].Name != "a" || agents[1].Value != 1 || agents[2].Name != "c" {
		t.Fatalf("agents = %v", agents)
	}
}

func TestLastWayUsedWins(t *testing.T) {
	b, _, _ := newBoard(0, Sum, 0)
	b.SetAgent("a", 2)
	b.SetAgent("b", 3)
	b.Set(7, "http")
	if b.Total() != 7 || len(b.Agents()) != 0 {
		t.Fatalf("whole number after agents: total %d agents %v", b.Total(), b.Agents())
	}
	b.SetAgent("a", 2)
	if b.Total() != 2 {
		t.Fatalf("agent after whole number: total %d, want 2", b.Total())
	}
}

func TestCombine(t *testing.T) {
	for c, want := range map[Combine]int{Sum: 12, Avg: 4, Max: 7} {
		b, _, _ := newBoard(0, c, 0)
		b.SetAgent("a", 2)
		b.SetAgent("b", 3)
		b.SetAgent("c", 7)
		if b.Total() != want {
			t.Errorf("%s = %d, want %d", c, b.Total(), want)
		}
	}
	b, _, _ := newBoard(0, Avg, 0)
	b.SetAgent("a", 1)
	b.SetAgent("b", 2)
	if b.Total() != 2 {
		t.Errorf("avg of 1 and 2 = %d, want 2 (rounded)", b.Total())
	}
	if _, err := ParseCombine("median"); err == nil {
		t.Error("ParseCombine accepted an unknown way")
	}
	if c, err := ParseCombine("max"); err != nil || c != Max {
		t.Errorf("ParseCombine(max) = %v, %v", c, err)
	}
}

func TestForgetAndExpire(t *testing.T) {
	b, got, now := newBoard(6, Sum, time.Minute)
	b.SetAgent("a", 2)
	*now = now.Add(40 * time.Second)
	b.SetAgent("b", 3)
	*now = now.Add(40 * time.Second) // a is 80s old, b is 40s old
	b.Expire()
	if b.Total() != 3 || len(b.Agents()) != 1 {
		t.Fatalf("after a expired: total %d agents %v", b.Total(), b.Agents())
	}
	if last := (*got)[len(*got)-1]; last != (change{3, "agents"}) {
		t.Fatalf("expiry was not reported: %v", last)
	}
	n := len(*got)
	b.Expire()
	if len(*got) != n {
		t.Fatal("Expire reported a change when nothing expired")
	}
	if !b.Forget("b") || b.Forget("b") {
		t.Fatal("Forget did not report whether the agent was known")
	}
	if b.Total() != 0 {
		t.Fatalf("with every agent gone the metric is %d, want 0", b.Total())
	}
}

func TestAgentNamesAndLimit(t *testing.T) {
	b, _, _ := newBoard(0, Sum, 0)
	for _, bad := range []string{"", "a/b", "../x", "has space", ".hidden"} {
		if _, err := b.SetAgent(bad, 1); err == nil {
			t.Errorf("agent name %q was accepted", bad)
		}
	}
	for i := 0; i < MaxAgents; i++ {
		if _, err := b.SetAgent(fmt.Sprintf("agent-%d", i), 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.SetAgent("one-too-many", 1); err == nil {
		t.Fatal("the agent limit was not enforced")
	}
	if _, err := b.SetAgent("agent-0", 5); err != nil {
		t.Fatalf("a known agent was refused at the limit: %v", err)
	}
}

func TestThirtyAgentsAtOnce(t *testing.T) {
	b := NewBoard(0, Sum, 0, time.Hour)
	var mu sync.Mutex
	last := 0
	b.OnChange = func(total int, _ string) { mu.Lock(); last = total; mu.Unlock() }
	var wg sync.WaitGroup
	for a := 0; a < 30; a++ {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			for i := 0; i <= 20; i++ {
				b.SetAgent(fmt.Sprintf("agent-%d", a), i%3)
			}
		}(a)
	}
	wg.Wait()
	// Every agent's last report was 20 % 3 = 2.
	if b.Total() != 60 || len(b.Agents()) != 30 {
		t.Fatalf("total %d agents %d, want 60 and 30", b.Total(), len(b.Agents()))
	}
	_ = last
}

func TestLandingsAreCountedPerHour(t *testing.T) {
	b, got, now := newBoard(5, Sum, 0)
	for i := 0; i < 3; i++ {
		if _, err := b.Land("a", 1); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(10 * time.Minute)
	}
	if total, _ := b.Land("b", 2); total != 5 || b.Mode() != ModeLandings {
		t.Fatalf("after 3 + 2 landings: %d in mode %s, want 5 (the whole number must not be counted)", total, b.Mode())
	}
	if total, _ := b.Land("", 1); total != 6 {
		t.Fatalf("a landing with no agent: %d, want 6", total)
	}
	agents := b.Agents()
	if len(agents) != 2 || agents[0] != (Agent{Name: "a", Value: 3, Updated: agents[0].Updated}) || agents[1].Value != 2 {
		t.Fatalf("agents = %v", agents)
	}

	// 35 minutes on, the first landing (65 minutes old) has left the hour.
	*now = now.Add(35 * time.Minute)
	n := len(*got)
	b.Expire()
	if b.Total() != 5 || len(*got) != n+1 || (*got)[n] != (change{5, ModeLandings}) {
		t.Fatalf("after one landing aged out: total %d changes %v", b.Total(), (*got)[n:])
	}
	b.Expire()
	if len(*got) != n+1 {
		t.Fatal("Expire reported a change when nothing aged out")
	}
	*now = now.Add(2 * time.Hour)
	b.Expire()
	if b.Total() != 0 {
		t.Fatalf("with every landing aged out: %d, want 0", b.Total())
	}
}

func TestLandingsWindowScalesToPerHour(t *testing.T) {
	b := NewBoard(0, Sum, 0, 15*time.Minute)
	b.Land("a", 3)
	if b.Total() != 12 {
		t.Fatalf("3 landings in a 15 minute window = %d per hour, want 12", b.Total())
	}
}

func TestSwitchingWaysForgetsTheOthers(t *testing.T) {
	b, _, _ := newBoard(0, Sum, 0)
	b.Land("a", 4)
	b.SetAgent("x", 2)
	if b.Total() != 2 || b.Mode() != ModeAgents {
		t.Fatalf("agents after landings: %d in %s", b.Total(), b.Mode())
	}
	b.Land("a", 1)
	if b.Total() != 1 || len(b.Agents()) != 1 || b.Agents()[0].Name != "a" {
		t.Fatalf("landings after agents: %d %v", b.Total(), b.Agents())
	}
	b.Set(9, "http")
	if b.Total() != 9 || b.Mode() != ModeTotal || len(b.Agents()) != 0 {
		t.Fatalf("whole number after landings: %d in %s %v", b.Total(), b.Mode(), b.Agents())
	}
}

func TestLandRefusesBadInput(t *testing.T) {
	b, _, _ := newBoard(3, Sum, 0)
	for _, n := range []int{0, -1, 1001} {
		if _, err := b.Land("a", n); err == nil {
			t.Errorf("Land accepted a count of %d", n)
		}
	}
	if _, err := b.Land("../x", 1); err == nil {
		t.Error("Land accepted a bad agent name")
	}
	if b.Total() != 3 || b.Mode() != ModeTotal {
		t.Fatalf("refused input changed the board: %d in %s", b.Total(), b.Mode())
	}
}
