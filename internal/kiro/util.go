package kiro

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// kiroVersion is the KiroIDE client version advertised in AWS user-agent headers.
// Validated against the installed Kiro IDE and the provider's current client
// contract. The retired 0.11.63 client is rejected with FEATURE_NOT_SUPPORTED.
const kiroVersion = "1.4.121"

// uuidV4 returns a random RFC 4122 version-4 UUID string.
func uuidV4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively fatal; return a zero UUID.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sha256Hex returns the lowercase hex SHA-256 digest of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// machineID derives a stable per-credential machine identifier from the first
// non-empty of profileArn > clientId > fallback constant.
func machineID(cred kiroCredential) string {
	key := firstNonEmptyStr(cred.ProfileArn, cred.ClientID, "KIRO_DEFAULT_MACHINE")
	return sha256Hex(key)
}

// randomMessageID returns a Claude-style message id (msg_<hex>).
func randomMessageID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "msg_kiro"
	}
	return "msg_" + hex.EncodeToString(b[:])
}

// randomHexN returns a random lowercase hex string of 2*n characters.
func randomHexN(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", 2*n)
	}
	return hex.EncodeToString(b)
}

// parseKiroTime parses an expiresAt timestamp, tolerating RFC3339 with or
// without fractional seconds.
func parseKiroTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
