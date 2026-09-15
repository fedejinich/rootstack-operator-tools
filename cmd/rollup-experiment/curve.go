package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"time"
)

// Curve represents a parameter that can change over time during a simulation.
// It supports five forms:
//   - Constant: a single value held throughout
//   - Ramp (minmax): two values, linearly interpolated from first to second
//   - Multi-point: N values placed at equidistant points, linearly interpolated between them
//   - Random: each evaluation returns a uniform random value in [min, max]
//   - Jitter: a base curve (any of the above) with additive uniform noise in [-jitter, +jitter]
type Curve struct {
	points []float64
	random bool    // if true, ignore points and return random values
	min    float64 // lower bound for random mode
	max    float64 // upper bound for random mode
	jitter float64 // if > 0, add uniform noise in [-jitter, +jitter] to the base value
}

// NewConstantCurve creates a curve that returns the same value at all times.
func NewConstantCurve(v float64) Curve {
	return Curve{points: []float64{v}}
}

// NewCurve creates a curve from a slice of points.
// If the slice is empty, the curve returns 0. A single-element slice is constant.
// Two elements form a linear ramp. More elements are interpolated piecewise-linearly.
func NewCurve(points []float64) Curve {
	if len(points) == 0 {
		return Curve{points: []float64{0}}
	}
	return Curve{points: points}
}

// NewRandomCurve creates a curve that returns a uniform random value in [min, max]
// on every evaluation.
func NewRandomCurve(min, max float64) Curve {
	if min > max {
		min, max = max, min
	}
	return Curve{random: true, min: min, max: max}
}

// WithJitter returns a copy of the curve with additive jitter enabled.
// Each evaluation adds a uniform random offset in [-jitter, +jitter] to the
// base value, clamped to [0, +Inf).
func (c Curve) WithJitter(jitter float64) Curve {
	c.jitter = math.Abs(jitter)
	return c
}

// At returns the value at progress t in [0, 1], where 0 is the start and 1
// is the end of the simulation.
//
// For random curves, t is ignored and a fresh random value in [min, max] is
// returned. For deterministic curves with jitter, the interpolated base value
// is perturbed by uniform noise in [-jitter, +jitter] and clamped to >= 0.
func (c Curve) At(t float64) float64 {
	if c.random {
		return c.min + rand.Float64()*(c.max-c.min)
	}

	var base float64
	switch len(c.points) {
	case 0:
		base = 0
	case 1:
		base = c.points[0]
	default:
		t = math.Max(0, math.Min(1, t))
		n := len(c.points)
		pos := t * float64(n-1)
		lo := int(math.Floor(pos))
		hi := lo + 1
		if hi >= n {
			base = c.points[n-1]
		} else {
			frac := pos - float64(lo)
			base = c.points[lo]*(1-frac) + c.points[hi]*frac
		}
	}

	if c.jitter > 0 {
		noise := (rand.Float64()*2 - 1) * c.jitter // uniform in [-jitter, +jitter]
		base = math.Max(0, base+noise)
	}

	return base
}

// AtTime returns the value at elapsed time within a total duration.
func (c Curve) AtTime(elapsed, total time.Duration) float64 {
	if total <= 0 {
		return c.At(0)
	}
	return c.At(float64(elapsed) / float64(total))
}

// IsConstant returns true if the curve is deterministic and has a single point.
func (c Curve) IsConstant() bool {
	return !c.random && c.jitter == 0 && len(c.points) <= 1
}

// IsRandom returns true if the curve produces random values.
func (c Curve) IsRandom() bool {
	return c.random
}

// HasJitter returns true if the curve has additive noise enabled.
func (c Curve) HasJitter() bool {
	return c.jitter > 0
}

// Start returns the value at t=0 (a single sample — may differ on next call if random/jitter).
func (c Curve) Start() float64 {
	return c.At(0)
}

// End returns the value at t=1 (a single sample).
func (c Curve) End() float64 {
	return c.At(1)
}

// PeakValue returns a conservative upper bound for the curve's output.
// For random curves this is the max bound; for deterministic curves it is the
// maximum across all interpolation points. Jitter is added in both cases.
func (c Curve) PeakValue() float64 {
	if c.random {
		return c.max + c.jitter
	}
	peak := 0.0
	for _, p := range c.points {
		if p > peak {
			peak = p
		}
	}
	return peak + c.jitter
}

// Describe returns a human-readable description of the curve for logging.
func (c Curve) Describe() string {
	fmtVal := func(v float64) string {
		s := strconv.FormatFloat(v, 'f', -1, 64)
		return s
	}
	if c.random {
		return fmt.Sprintf("random[%s, %s]", fmtVal(c.min), fmtVal(c.max))
	}
	if len(c.points) == 1 && c.jitter == 0 {
		return fmtVal(c.points[0])
	}
	if len(c.points) == 1 && c.jitter > 0 {
		return fmt.Sprintf("%s ± %s", fmtVal(c.points[0]), fmtVal(c.jitter))
	}
	desc := "["
	for i, p := range c.points {
		if i > 0 {
			desc += ", "
		}
		desc += fmtVal(p)
	}
	desc += "]"
	if c.jitter > 0 {
		desc += fmt.Sprintf(" ± %s", fmtVal(c.jitter))
	}
	return desc
}
