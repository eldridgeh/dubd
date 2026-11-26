// Package tempo turns an integer metric into a playback rate.
package tempo

import (
	"errors"
	"math"
)

// Curve maps a metric range onto a playback-rate range, linearly, clamped at
// both ends.
type Curve struct {
	MetricMin, MetricMax int
	RateMin, RateMax     float64
}

// Browsers accept playback rates in roughly this range; outside it the
// media element throws or goes silent.
const (
	LowestRate  = 0.25
	HighestRate = 4.0
)

func (c Curve) Validate() error {
	switch {
	case c.MetricMax <= c.MetricMin:
		return errors.New("metric-max must be greater than metric-min")
	case c.RateMax < c.RateMin:
		return errors.New("rate-max must not be less than rate-min")
	case c.RateMin < LowestRate || c.RateMax > HighestRate:
		return errors.New("rates must be between 0.25 and 4.0")
	}
	return nil
}

// Rate returns the playback rate for a metric value.
func (c Curve) Rate(metric int) float64 {
	if metric <= c.MetricMin {
		return c.RateMin
	}
	if metric >= c.MetricMax {
		return c.RateMax
	}
	frac := float64(metric-c.MetricMin) / float64(c.MetricMax-c.MetricMin)
	return round3(c.RateMin + frac*(c.RateMax-c.RateMin))
}

// Step moves current toward target by at most maxStep. A maxStep of zero or
// less jumps straight to the target.
func Step(current, target, maxStep float64) float64 {
	if maxStep <= 0 || math.Abs(target-current) <= maxStep {
		return target
	}
	if target > current {
		return round3(current + maxStep)
	}
	return round3(current - maxStep)
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
