package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)


func SignPayload(secret string, timestamp time.Time, payload []byte) string {
	signedContent := strconv.FormatInt(timestamp.Unix(), 10) + "." + string(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signedContent))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature recomputes the expected signature and compares it
// against the one presented, using a constant-time comparison so the
// check itself doesn't leak timing information about how many bytes
// matched -- the same reasoning as comparing password hashes, applied
// here because a receiver's verification code is the reference
// implementation other integrators will copy. Exported so this package
// can also serve as the canonical example of how to verify a Transacta
// webhook, not just how to send one.
func VerifySignature(secret string, timestamp time.Time, payload []byte, signature string) bool {
	expected := SignPayload(secret, timestamp, payload)
	return hmac.Equal([]byte(expected), []byte(signature))
}