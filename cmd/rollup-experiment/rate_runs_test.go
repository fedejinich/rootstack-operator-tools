package main

import (
	"testing"
)

func TestExpandExperimentRuns(t *testing.T) {
	b1 := BatcherRunConfig{MaxL1TxSize: 100}
	b2 := BatcherRunConfig{MaxL1TxSize: 200}

	t.Run("no rate runs", func(t *testing.T) {
		got := ExpandExperimentRuns([]BatcherRunConfig{b1, b2}, nil)
		if len(got) != 2 {
			t.Fatalf("len=%d want 2", len(got))
		}
		if got[0].SimpleTxRate != nil || got[1].SimpleTxRate != nil {
			t.Fatal("expected nil SimpleTxRate")
		}
		if got[0].Batcher.MaxL1TxSize != 100 || got[1].Batcher.MaxL1TxSize != 200 {
			t.Fatal("batcher mismatch")
		}
	})

	t.Run("cartesian product", func(t *testing.T) {
		rates := []float64{1.5, 2.5}
		got := ExpandExperimentRuns([]BatcherRunConfig{b1, b2}, rates)
		if len(got) != 4 {
			t.Fatalf("len=%d want 4", len(got))
		}
		want := []struct {
			txSize uint64
			rate   float64
		}{
			{100, 1.5}, {100, 2.5}, {200, 1.5}, {200, 2.5},
		}
		for i, g := range got {
			if g.Batcher.MaxL1TxSize != want[i].txSize {
				t.Errorf("[%d] txSize=%d want %d", i, g.Batcher.MaxL1TxSize, want[i].txSize)
			}
			if g.SimpleTxRate == nil || *g.SimpleTxRate != want[i].rate {
				t.Errorf("[%d] rate=%v want %g", i, g.SimpleTxRate, want[i].rate)
			}
		}
	})
}

func TestResolveSimulationRateRunsErrors(t *testing.T) {
	base := SimulationConfig{
		Length:   LengthValue{Raw: []string{"1m"}},
		Accounts: 1,
		SimpleTx: SimpleTxTOML{
			RateRuns: []float64{10, 20},
			Rate:     CurveValue{Values: []float64{5}},
		},
	}
	_, err := ResolveSimulation(base)
	if err == nil {
		t.Fatal("expected error when rate and rate_runs both non-zero")
	}

	adaptive := base
	adaptive.SimpleTx = SimpleTxTOML{
		RateRuns: []float64{10},
		Rate:     CurveValue{Adaptive: true},
	}
	_, err = ResolveSimulation(adaptive)
	if err == nil {
		t.Fatal("expected error for rate_runs + adaptive")
	}
}

func TestResolveSimulationRateRunsOK(t *testing.T) {
	sim := SimulationConfig{
		Length:   LengthValue{Raw: []string{"1m"}},
		Accounts: 1,
		SimpleTx: SimpleTxTOML{
			RateRuns: []float64{12, 24},
			Rate:     CurveValue{Values: []float64{0}},
		},
	}
	r, err := ResolveSimulation(sim)
	if err != nil {
		t.Fatal(err)
	}
	if !r.TxRate.IsConstant() || r.TxRate.Start() != 12 {
		t.Fatalf("resolved TxRate: constant=%v start=%g want 12", r.TxRate.IsConstant(), r.TxRate.Start())
	}
	if r.TxRateAdaptive {
		t.Fatal("TxRateAdaptive should be false")
	}

	r2 := r.WithSimpleTxRate(99)
	if r2.TxRate.Start() != 99 {
		t.Fatalf("WithSimpleTxRate: got %g", r2.TxRate.Start())
	}
	if r2.TxRateAdaptive {
		t.Fatal("adaptive off after WithSimpleTxRate")
	}
	if r.TxRate.Start() != 12 {
		t.Fatal("original mutated")
	}
}
