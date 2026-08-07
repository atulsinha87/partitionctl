package planner

import "testing"

func TestEstimatorBuildSeconds(t *testing.T) {
	e := DefaultEstimator()

	tests := []struct {
		name     string
		relPages int64
		want     int
	}{
		{"unanalyzed table falls back to the floor", 0, DefaultMinBuildSeconds},
		{"negative relpages is clamped, not propagated", -1, DefaultMinBuildSeconds},
		{"a tiny table still gets the floor", 100, DefaultMinBuildSeconds},
		// 1 GiB = 131072 pages. Two scans over 2 GiB at 50 MiB/s.
		{"one gibibyte", 131072, 2 * 1024 / 50},
		// 1 TiB.
		{"one tebibyte", 134217728, 2 * 1024 * 1024 / 50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.BuildSeconds(tc.relPages); got != tc.want {
				t.Errorf("BuildSeconds(%d) = %d, want %d", tc.relPages, got, tc.want)
			}
		})
	}
}

// TestEstimatorIsDeterministic is the property that matters: the same catalog
// must produce the same estimate in every process, or the plan digest is not
// reproducible (FR-PLANFILE-2).
func TestEstimatorIsDeterministic(t *testing.T) {
	e := DefaultEstimator()
	for _, pages := range []int64{0, 1, 7, 12345, 131072, 134217728, 1 << 30} {
		first := e.BuildSeconds(pages)
		for i := 0; i < 50; i++ {
			if got := e.BuildSeconds(pages); got != first {
				t.Fatalf("BuildSeconds(%d) returned %d then %d", pages, first, got)
			}
		}
	}
}

// TestEstimatorMonotonic: a bigger partition never estimates shorter.
func TestEstimatorMonotonic(t *testing.T) {
	e := DefaultEstimator()
	prev := e.BuildSeconds(0)
	for _, pages := range []int64{1, 1000, 131072, 262144, 134217728} {
		got := e.BuildSeconds(pages)
		if got < prev {
			t.Fatalf("BuildSeconds(%d) = %d, less than the previous %d", pages, got, prev)
		}
		prev = got
	}
}

func TestEstimatorZeroValueUsesDefaults(t *testing.T) {
	var zero Estimator
	def := DefaultEstimator()
	for _, pages := range []int64{0, 131072, 134217728} {
		if got, want := zero.BuildSeconds(pages), def.BuildSeconds(pages); got != want {
			t.Errorf("zero Estimator BuildSeconds(%d) = %d, want %d", pages, got, want)
		}
	}
	if got, want := zero.CatalogNodeSeconds(), DefaultCatalogSeconds; got != want {
		t.Errorf("CatalogNodeSeconds = %d, want %d", got, want)
	}
	if got, want := zero.Bytes(2), 2*DefaultPageBytes; got != want {
		t.Errorf("Bytes(2) = %d, want %d", got, want)
	}
}

func TestEstimatorOverrideOneField(t *testing.T) {
	// Overriding throughput alone must not zero out the other settings.
	e := Estimator{BuildBytesPerSecond: 100 << 20}
	fast := e.BuildSeconds(134217728)
	slow := DefaultEstimator().BuildSeconds(134217728)
	if fast >= slow {
		t.Errorf("doubling throughput did not halve the estimate: %d vs %d", fast, slow)
	}
	if got := e.CatalogNodeSeconds(); got != DefaultCatalogSeconds {
		t.Errorf("CatalogNodeSeconds = %d, want the default %d", got, DefaultCatalogSeconds)
	}
}

func TestEstimatorReindex(t *testing.T) {
	e := DefaultEstimator()

	// A reindex reads the table, so it is sized like a build (FR-PLAN-9).
	if got, want := e.ReindexSeconds(131072), e.BuildSeconds(131072); got != want {
		t.Errorf("ReindexSeconds = %d, want %d", got, want)
	}

	// FR-REIDX-7: peak *additional* storage is one more copy of the index.
	tests := []struct {
		indexPages int64
		want       int64
	}{
		{0, 0},
		{-5, 0},
		{1, DefaultPageBytes},
		{131072, 1 << 30},
	}
	for _, tc := range tests {
		if got := e.ReindexPeakBytes(tc.indexPages); got != tc.want {
			t.Errorf("ReindexPeakBytes(%d) = %d, want %d", tc.indexPages, got, tc.want)
		}
	}
}

func TestEstimatorWaitSeconds(t *testing.T) {
	e := DefaultEstimator()
	tests := []struct{ in, want int }{{0, 0}, {30, 30}, {-1, 0}}
	for _, tc := range tests {
		if got := e.WaitSeconds(tc.in); got != tc.want {
			t.Errorf("WaitSeconds(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
