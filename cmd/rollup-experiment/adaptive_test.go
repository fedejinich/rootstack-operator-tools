package main

import (
	"log/slog"
	"math"
	"testing"
	"time"
)

const testEpsilon = 1e-9

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < testEpsilon
}

func TestCurveValueAdaptive_StringMax(t *testing.T) {
	var cv CurveValue
	if err := cv.UnmarshalTOML("max"); err != nil {
		t.Fatalf("UnmarshalTOML(\"max\") error: %v", err)
	}
	if !cv.IsAdaptive() {
		t.Error("expected IsAdaptive()=true for rate=\"max\"")
	}
	if cv.TargetPending != 0 {
		t.Errorf("expected TargetPending=0 (use defaults), got %d", cv.TargetPending)
	}
	if cv.MaxRate != 0 {
		t.Errorf("expected MaxRate=0 (use defaults), got %f", cv.MaxRate)
	}

	c := cv.ToCurve()
	if c.Start() != adaptiveSeedRate {
		t.Errorf("expected seed rate=%f, got %f", adaptiveSeedRate, c.Start())
	}
}

func TestCurveValueAdaptive_TableDefaults(t *testing.T) {
	m := map[string]interface{}{
		"adaptive": true,
	}
	var cv CurveValue
	if err := cv.UnmarshalTOML(m); err != nil {
		t.Fatalf("UnmarshalTOML error: %v", err)
	}
	if !cv.IsAdaptive() {
		t.Error("expected IsAdaptive()=true")
	}
	if cv.TargetPending != 0 {
		t.Errorf("expected TargetPending=0 (defaults applied later), got %d", cv.TargetPending)
	}
}

func TestCurveValueAdaptive_TableCustom(t *testing.T) {
	m := map[string]interface{}{
		"adaptive":       true,
		"target_pending": int64(200),
		"max_rate":       float64(3000),
	}
	var cv CurveValue
	if err := cv.UnmarshalTOML(m); err != nil {
		t.Fatalf("UnmarshalTOML error: %v", err)
	}
	if !cv.IsAdaptive() {
		t.Error("expected IsAdaptive()=true")
	}
	if cv.TargetPending != 200 {
		t.Errorf("expected TargetPending=200, got %d", cv.TargetPending)
	}
	if cv.MaxRate != 3000 {
		t.Errorf("expected MaxRate=3000, got %f", cv.MaxRate)
	}
}

func TestCurveValueAdaptive_NotSet(t *testing.T) {
	var cv CurveValue
	if err := cv.UnmarshalTOML(float64(42)); err != nil {
		t.Fatalf("UnmarshalTOML error: %v", err)
	}
	if cv.IsAdaptive() {
		t.Error("expected IsAdaptive()=false for constant value")
	}
}

func TestResolveSimulation_AdaptiveDefaults(t *testing.T) {
	sim := SimulationConfig{
		Length:         LengthValue{Raw: []string{"1m"}},
		Accounts:       10,
		SampleInterval: "1s",
		SimpleTx: SimpleTxTOML{
			Rate: CurveValue{Adaptive: true},
		},
	}
	resolved, err := ResolveSimulation(sim)
	if err != nil {
		t.Fatalf("ResolveSimulation error: %v", err)
	}
	if !resolved.TxRateAdaptive {
		t.Error("expected TxRateAdaptive=true")
	}
	if resolved.TxRateTargetPending != adaptiveDefaultTargetPending {
		t.Errorf("expected target_pending=%d, got %d", adaptiveDefaultTargetPending, resolved.TxRateTargetPending)
	}
	if resolved.TxRateMaxRate != adaptiveDefaultMaxRate {
		t.Errorf("expected max_rate=%f, got %f", adaptiveDefaultMaxRate, resolved.TxRateMaxRate)
	}
	if resolved.TxRate.Start() != adaptiveSeedRate {
		t.Errorf("expected seed rate=%f, got %f", adaptiveSeedRate, resolved.TxRate.Start())
	}
}

func TestResolveSimulation_AdaptiveCustom(t *testing.T) {
	sim := SimulationConfig{
		Length:         LengthValue{Raw: []string{"1m"}},
		Accounts:       10,
		SampleInterval: "1s",
		SimpleTx: SimpleTxTOML{
			Rate: CurveValue{Adaptive: true, TargetPending: 50, MaxRate: 2000},
		},
	}
	resolved, err := ResolveSimulation(sim)
	if err != nil {
		t.Fatalf("ResolveSimulation error: %v", err)
	}
	if resolved.TxRateTargetPending != 50 {
		t.Errorf("expected target_pending=50, got %d", resolved.TxRateTargetPending)
	}
	if resolved.TxRateMaxRate != 2000 {
		t.Errorf("expected max_rate=2000, got %f", resolved.TxRateMaxRate)
	}
}

// newTestRunner creates a Runner with adaptive config and a stale decision
// timestamp so the AIMD decision fires immediately (no throttle).
func newTestRunner(targetPending int, maxRate float64) *Runner {
	return &Runner{
		logger: slog.Default(),
		sim: &ResolvedSimConfig{
			TxRateAdaptive:      true,
			TxRateTargetPending: targetPending,
			TxRateMaxRate:       maxRate,
		},
		adaptiveLastDecision: time.Now().Add(-2 * adaptiveDefaultDecisionInterval),
		adaptiveEMAInit:      false,
	}
}

func TestAdaptiveRate_RampUp(t *testing.T) {
	r := newTestRunner(100, 5000)

	mv := map[string]float64{"txpool_pending": 0}
	rate := r.adaptiveRate(100, mv, nil)
	expected := float64(100) * 1.3
	if !approxEqual(rate, expected) {
		t.Errorf("expected rate≈%.1f (100*1.3), got %f", expected, rate)
	}
}

func TestAdaptiveRate_ProportionalIncrease(t *testing.T) {
	r := newTestRunner(100, 5000)

	mv := map[string]float64{"txpool_pending": 60}
	rate := r.adaptiveRate(200, mv, nil)
	expected := float64(200) * 1.1
	if !approxEqual(rate, expected) {
		t.Errorf("expected rate≈%.1f (200*1.1), got %f", expected, rate)
	}
}

func TestAdaptiveRate_GentleDecrease(t *testing.T) {
	r := newTestRunner(100, 5000)

	mv := map[string]float64{"txpool_pending": 150}
	rate := r.adaptiveRate(500, mv, nil)
	expected := float64(500) * 0.8
	if !approxEqual(rate, expected) {
		t.Errorf("expected rate≈%f (500*0.8), got %f", expected, rate)
	}
}

func TestAdaptiveRate_HardCut(t *testing.T) {
	r := newTestRunner(100, 5000)

	mv := map[string]float64{"txpool_pending": 250}
	rate := r.adaptiveRate(1000, mv, nil)
	expected := float64(1000) * 0.5
	if !approxEqual(rate, expected) {
		t.Errorf("expected rate≈%f (1000*0.5), got %f", expected, rate)
	}
}

func TestAdaptiveRate_MaxRateCap(t *testing.T) {
	r := newTestRunner(100, 200)

	mv := map[string]float64{"txpool_pending": 0}
	rate := r.adaptiveRate(180, mv, nil)
	if !approxEqual(rate, 200) {
		t.Errorf("expected rate≈200 (capped at max_rate), got %f", rate)
	}
}

func TestAdaptiveRate_SeedFromZero(t *testing.T) {
	r := newTestRunner(100, 5000)

	mv := map[string]float64{"txpool_pending": 0}
	rate := r.adaptiveRate(0, mv, nil)
	expected := adaptiveSeedRate * 1.3
	if !approxEqual(rate, expected) {
		t.Errorf("expected rate≈%f (seed*1.3), got %f", expected, rate)
	}
}

func TestAdaptiveRate_MinimumRate(t *testing.T) {
	r := newTestRunner(100, 5000)

	mv := map[string]float64{"txpool_pending": 500}
	rate := r.adaptiveRate(1.5, mv, nil)
	if !approxEqual(rate, 1) {
		t.Errorf("expected rate≈1 (minimum), got %f", rate)
	}
}

func TestAdaptiveRate_Throttled(t *testing.T) {
	r := newTestRunner(100, 5000)

	// First call — decision fires (timer is stale).
	mv := map[string]float64{"txpool_pending": 0}
	rate1 := r.adaptiveRate(100, mv, nil)
	if rate1 != 130 {
		t.Fatalf("expected first decision rate=130, got %f", rate1)
	}

	// Second call immediately after — should be throttled, returning prev rate unchanged.
	mv["txpool_pending"] = 0
	rate2 := r.adaptiveRate(rate1, mv, nil)
	if rate2 != rate1 {
		t.Errorf("expected throttled rate=%f (unchanged), got %f", rate1, rate2)
	}
}

func TestAdaptiveRate_EMASmoothing(t *testing.T) {
	r := newTestRunner(100, 5000)

	// First call with pending=0 initialises EMA.
	mv := map[string]float64{"txpool_pending": 0}
	r.adaptiveRate(100, mv, nil)

	// Advance past throttle.
	r.adaptiveLastDecision = time.Now().Add(-2 * adaptiveDefaultDecisionInterval)

	// Second call with pending=300 — EMA should be smoothed, not raw 300.
	// EMA = 0.3*300 + 0.7*0 = 90, which is < target(100) but >= target*0.5(50) → *1.1.
	mv["txpool_pending"] = 300
	rate := r.adaptiveRate(100, mv, nil)
	expectedEMA := adaptiveEMAlpha*300 + (1-adaptiveEMAlpha)*0
	if math.Abs(r.adaptivePendingEMA-expectedEMA) > 0.01 {
		t.Errorf("expected EMA=%.1f, got %.1f", expectedEMA, r.adaptivePendingEMA)
	}
	// EMA=90, target=100, target*0.5=50 → 50 <= 90 < 100 → *1.1
	if !approxEqual(rate, float64(100)*1.1) {
		t.Errorf("expected rate≈110 (100*1.1 via EMA-smoothed pending=90), got %f", rate)
	}
}

func TestAdaptiveRate_ConvergenceOverMultipleDecisions(t *testing.T) {
	r := newTestRunner(100, 5000)

	// Simulate a scenario where pending settles at ~80.
	// Rate should ramp up and stabilise rather than oscillate wildly.
	rate := 10.0
	for i := 0; i < 20; i++ {
		r.adaptiveLastDecision = time.Now().Add(-2 * adaptiveDefaultDecisionInterval)
		mv := map[string]float64{"txpool_pending": 80}
		rate = r.adaptiveRate(rate, mv, nil)
	}
	// With constant pending=80 (< target, >= target*0.5), rate should increase
	// multiplicatively by 1.1 each decision: 10 * 1.1^20 ≈ 67.3
	expected := 10.0 * math.Pow(1.1, 20)
	if math.Abs(rate-expected) > 1.0 {
		t.Errorf("expected rate≈%.1f after 20 decisions with pending=80, got %.1f", expected, rate)
	}
}
