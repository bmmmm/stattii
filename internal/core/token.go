// SPDX-License-Identifier: GPL-3.0-or-later

package core

import (
	"crypto/rand"
	"encoding/hex"
)

// NewToken returns a 128-bit random token, hex-encoded (32 chars).
// Tokens are looked up in the store, never decoded — no JWT, trivially
// revocable.
func NewToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return hex.EncodeToString(b)
}

// NewID returns a short prefixed identifier, e.g. "ev_a1b2c3d4e5f6".
func NewID(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}
