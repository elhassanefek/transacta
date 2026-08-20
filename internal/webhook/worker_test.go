package webhook

import (
	"testing"
	"time"
)

func TestNewWorker_AppliesDefaults(t *testing.T) {
	w := NewWorker(&Repository{}, &Service{})
	if w.batchSize != DefaultBatchSize {
		t.Errorf("batchSize = %d, want %d", w.batchSize, DefaultBatchSize)
	}
	if w.pollInterval != DefaultPollInterval {
		t.Errorf("pollInterval = %v, want %v", w.pollInterval, DefaultPollInterval)
	}
}

func TestNewWorker_OptionsOverrideDefaults(t *testing.T) {
	w := NewWorker(&Repository{}, &Service{}, WithBatchSize(5), WithPollInterval(time.Second))
	if w.batchSize != 5 {
		t.Errorf("batchSize = %d, want 5", w.batchSize)
	}
	if w.pollInterval != time.Second {
		t.Errorf("pollInterval = %v, want 1s", w.pollInterval)
	}
}