package webhook

import (
	"testing"
	"time"
)

func TestSignPayload_Deterministic(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	s1 := SignPayload("secret", ts, []byte(`{"a":1}`))
	s2 := SignPayload("secret", ts, []byte(`{"a":1}`))
	if s1 != s2 {
		t.Fatal("expected identical inputs to produce identical signatures")
	}
}

func TestSignPayload_DifferentTimestampDifferentSignature(t *testing.T) {
	payload := []byte(`{"a":1}`)
	s1 := SignPayload("secret", time.Unix(1700000000, 0), payload)
	s2 := SignPayload("secret", time.Unix(1700000001, 0), payload)
	if s1 == s2 {
		t.Fatal("expected different timestamps to produce different signatures -- this is what makes replay detection possible")
	}
}

func TestSignPayload_DifferentPayloadDifferentSignature(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	s1 := SignPayload("secret", ts, []byte(`{"a":1}`))
	s2 := SignPayload("secret", ts, []byte(`{"a":2}`))
	if s1 == s2 {
		t.Fatal("expected different payloads to produce different signatures")
	}
}

func TestSignPayload_DifferentSecretDifferentSignature(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	payload := []byte(`{"a":1}`)
	s1 := SignPayload("secret-a", ts, payload)
	s2 := SignPayload("secret-b", ts, payload)
	if s1 == s2 {
		t.Fatal("expected different secrets to produce different signatures")
	}
}

func TestVerifySignature_ValidSignatureAccepted(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	payload := []byte(`{"a":1}`)
	sig := SignPayload("secret", ts, payload)
	if !VerifySignature("secret", ts, payload, sig) {
		t.Fatal("expected a correctly computed signature to verify")
	}
}

func TestVerifySignature_TamperedPayloadRejected(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	sig := SignPayload("secret", ts, []byte(`{"a":1}`))
	if VerifySignature("secret", ts, []byte(`{"a":999}`), sig) {
		t.Fatal("expected a signature computed over a different payload to be rejected")
	}
}

func TestVerifySignature_WrongSecretRejected(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	payload := []byte(`{"a":1}`)
	sig := SignPayload("secret-a", ts, payload)
	if VerifySignature("secret-b", ts, payload, sig) {
		t.Fatal("expected a signature computed with a different secret to be rejected")
	}
}

func TestVerifySignature_TamperedTimestampRejected(t *testing.T) {
	payload := []byte(`{"a":1}`)
	sig := SignPayload("secret", time.Unix(1700000000, 0), payload)
	// Same signature, but verifying against a different timestamp than
	// the one it was actually computed with -- simulates a replayed
	// request where an attacker changed the timestamp header.
	if VerifySignature("secret", time.Unix(1700000001, 0), payload, sig) {
		t.Fatal("expected verification against a different timestamp to fail")
	}
}