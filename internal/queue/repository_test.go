package queue

import "testing"

func TestNewRepository_AcceptsPlainIdentifiers(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic for valid table names: %v", r)
		}
	}()
	NewRepository(nil, "webhook_events", "dead_letter_events")
}

func TestNewRepository_PanicsOnInvalidQueueTableName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for an invalid queue table name")
		}
	}()
	NewRepository(nil, "webhook_events; DROP TABLE users;--", "dead_letter_events")
}

func TestNewRepository_PanicsOnInvalidDeadLetterTableName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for an invalid dead-letter table name")
		}
	}()
	NewRepository(nil, "webhook_events", "dead letter events")
}
