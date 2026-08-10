package tenants

import "testing"

func TestHashAPIKey_Deterministic(t *testing.T) {
	h1 := HashAPIKey("sk_sometestkey")
	h2 := HashAPIKey("sk_sometestkey")
	if h1 != h2 {
		t.Fatal("expected identical inputs to hash identically")
	}
}

func TestHashAPIKey_DifferentInputsDifferentHashes(t *testing.T) {
	h1 := HashAPIKey("sk_keyone")
	h2 := HashAPIKey("sk_keytwo")
	if h1 == h2 {
		t.Fatal("expected different inputs to hash differently")
	}
}