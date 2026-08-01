package ledger

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestSum(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	entries := []EntryInput{
		{AccountID: a, AmountMinor: -500},
		{AccountID: b, AmountMinor: 500},
	}
	if got := Sum(entries); got != 0 {
		t.Fatalf("Sum() = %d, want 0", got)
	}
}
func TestValidateEntries_RejectsUnbalanced(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	entries := []EntryInput{
		{AccountID: a, AmountMinor: -500},
		{AccountID: b, AmountMinor: 400}, // short by 100 — should be rejected
	}
	err := validateEntries(entries)
	if !errors.Is(err, ErrUnbalancedEntries) {
		t.Fatalf("validateEntries() = %v, want ErrUnbalancedEntries", err)
	}
}
func TestValidateEntries_RejectsSingleEntry(t *testing.T) {
	a := uuid.New()
	entries := []EntryInput{{AccountID: a, AmountMinor: 100}}
	err := validateEntries(entries)
	if !errors.Is(err, ErrEmptyEntries) {
		t.Fatalf("validateEntries() = %v, want ErrEmptyEntries", err)
	}
}

func TestValidateEntries_RejectsZeroAmount(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	// Mirrors chk_entry_amount_nonzero: a zero-amount entry must be
	// rejected even if the overall set still sums to zero.
	entries := []EntryInput{
		{AccountID: a, AmountMinor: 0},
		{AccountID: b, AmountMinor: 0},
	}
	err := validateEntries(entries)
	if !errors.Is(err, ErrZeroAmountEntry) {
		t.Fatalf("validateEntries() = %v, want ErrZeroAmountEntry", err)
	}
}

func TestValidateEntries_AcceptsBalancedMultiLeg(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	// One debit split across two credits — still must net to zero.
	entries := []EntryInput{
		{AccountID: a, AmountMinor: -1000},
		{AccountID: b, AmountMinor: 600},
		{AccountID: c, AmountMinor: 400},
	}
	if err := validateEntries(entries); err != nil {
		t.Fatalf("validateEntries() = %v, want nil", err)
	}
}
func TestUniqueAccountIDs_Deduplicates(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	entries := []EntryInput{
		{AccountID: a, AmountMinor: -1000},
		{AccountID: b, AmountMinor: 600},
		{AccountID: b, AmountMinor: 400}, // b appears twice
	}
	ids := uniqueAccountIDs(entries)
	if len(ids) != 2 {
		t.Fatalf("uniqueAccountIDs() returned %d ids, want 2", len(ids))
	}
}
