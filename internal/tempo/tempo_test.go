package tempo

import "testing"

func TestRate(t *testing.T) {
	c := Curve{MetricMin: 0, MetricMax: 10, RateMin: 0.5, RateMax: 1.5}
	for _, tc := range []struct {
		metric int
		want   float64
	}{{-3, 0.5}, {0, 0.5}, {5, 1.0}, {7, 1.2}, {10, 1.5}, {99, 1.5}} {
		if got := c.Rate(tc.metric); got != tc.want {
			t.Errorf("Rate(%d) = %v, want %v", tc.metric, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	good := Curve{MetricMin: 0, MetricMax: 12, RateMin: 0.6, RateMax: 1.6}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid curve rejected: %v", err)
	}
	for name, c := range map[string]Curve{
		"flat metric":   {MetricMin: 5, MetricMax: 5, RateMin: 0.6, RateMax: 1.6},
		"inverted rate": {MetricMin: 0, MetricMax: 12, RateMin: 1.6, RateMax: 0.6},
		"rate too low":  {MetricMin: 0, MetricMax: 12, RateMin: 0.1, RateMax: 1},
		"rate too high": {MetricMin: 0, MetricMax: 12, RateMin: 1, RateMax: 9},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestStep(t *testing.T) {
	for _, tc := range []struct{ cur, target, max, want float64 }{
		{1.0, 1.5, 0.05, 1.05},
		{1.0, 0.5, 0.05, 0.95},
		{1.0, 1.02, 0.05, 1.02},
		{1.0, 1.5, 0, 1.5},
	} {
		if got := Step(tc.cur, tc.target, tc.max); got != tc.want {
			t.Errorf("Step(%v,%v,%v) = %v, want %v", tc.cur, tc.target, tc.max, got, tc.want)
		}
	}
}
