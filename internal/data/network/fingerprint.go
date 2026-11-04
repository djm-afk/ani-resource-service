package data

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// contentHash is used for Res request idempotency and its own configuration
// identities. It is not an observation version or a network readiness test.
func contentHash(value any) string {
	body, _ := json.Marshal(value)
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
