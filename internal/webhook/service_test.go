package webhook

import (
	"testing"
	"time"
)

func testServiceForBackoff(base, max time.Duration) *Service {
	return &Service{baseBackoff: base, maxBackoff: max}
}

func TestBackoffForAttempt_IncreasesWithEachAttempt(t *testing.T) {
	// base=1s, so attempt ranges (with +/-20% jitter) are:
	//   attempt 1: [0.8s, 1.2s]
	//   attempt 2: [1.6s, 2.4s]
	//   attempt 3: [3.2s, 4.8s]
	// These ranges don't overlap, so backoff is genuinely monotonically
	// increasing across attempts, not just "usually" due to jitter luck.
	s := testServiceForBackoff(time.Second, time.Hour)

	b1 := s.backoffForAttempt(1)
	b2 := s.backoffForAttempt(2)
	b3 := s.backoffForAttempt(3)

	if b1 >= b2 {
		t.Fatalf("attempt 1 backoff (%v) should be less than attempt 2 (%v)", b1, b2)
	}
	if b2 >= b3 {
		t.Fatalf("attempt 2 backoff (%v) should be less than attempt 3 (%v)", b2, b3)
	}
}

func TestBackoffForAttempt_CapsAtMaxBackoff(t *testing.T) {
	s := testServiceForBackoff(time.Second, 10*time.Second)

	// A large attempt number would compute an enormous exponential
	// value without the cap -- confirm it's actually bounded.
	b := s.backoffForAttempt(20)
	lower := time.Duration(float64(10*time.Second) * 0.8)
	upper := time.Duration(float64(10*time.Second) * 1.2)
	if b < lower || b > upper {
		t.Fatalf("backoff for a large attempt = %v, want within [%v, %v] (jittered max)", b, lower, upper)
	}
}

func TestBackoffForAttempt_FirstAttemptNearBase(t *testing.T) {
	s := testServiceForBackoff(30*time.Second, time.Hour)
	b := s.backoffForAttempt(1)
	lower := time.Duration(float64(30*time.Second) * 0.8)
	upper := time.Duration(float64(30*time.Second) * 1.2)
	if b < lower || b > upper {
		t.Fatalf("first attempt backoff = %v, want within [%v, %v]", b, lower, upper)
	}
}

func TestJitter_WithinTwentyPercentRange(t *testing.T) {
	d := 100 * time.Second
	lower := time.Duration(float64(d) * 0.8)
	upper := time.Duration(float64(d) * 1.2)
	for i := 0; i < 100; i++ {
		got := jitter(d)
		if got < lower || got > upper {
			t.Fatalf("jitter(%v) = %v, want within [%v, %v]", d, got, lower, upper)
		}
	}
}

func TestJitter_ZeroDurationReturnsZero(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Fatalf("jitter(0) = %v, want 0", got)
	}
}

func TestJitter_ProducesVariation(t *testing.T) {
	// Not every call should return the identical value -- confirms
	// jitter is actually randomizing, not silently returning d unchanged.
	d := 100 * time.Second
	seen := make(map[time.Duration]bool)
	for i := 0; i < 20; i++ {
		seen[jitter(d)] = true
	}
	if len(seen) < 2 {
		t.Fatal("expected jitter to produce varying results across repeated calls")
	}
}

func TestNewService_AppliesDefaults(t *testing.T) {
	s := NewService(&Repository{})
	if s.maxAttempts != DefaultMaxAttempts {
		t.Errorf("maxAttempts = %d, want %d", s.maxAttempts, DefaultMaxAttempts)
	}
	if s.baseBackoff != DefaultBaseBackoff {
		t.Errorf("baseBackoff = %v, want %v", s.baseBackoff, DefaultBaseBackoff)
	}
	if s.maxBackoff != DefaultMaxBackoff {
		t.Errorf("maxBackoff = %v, want %v", s.maxBackoff, DefaultMaxBackoff)
	}
}

func TestNewService_OptionsOverrideDefaults(t *testing.T) {
	s := NewService(&Repository{}, WithMaxAttempts(3), WithBaseBackoff(time.Second), WithMaxBackoff(time.Minute))
	if s.maxAttempts != 3 {
		t.Errorf("maxAttempts = %d, want 3", s.maxAttempts)
	}
	if s.baseBackoff != time.Second {
		t.Errorf("baseBackoff = %v, want 1s", s.baseBackoff)
	}
	if s.maxBackoff != time.Minute {
		t.Errorf("maxBackoff = %v, want 1m", s.maxBackoff)
	}
}