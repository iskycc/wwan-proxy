package proxyauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const (
	hashPrefix = "$wwan-bcrypt-sha256$"
	Redacted   = "********"
	// dummyHash is a valid cost-10 bcrypt hash used when a username does not
	// exist. Comparing against it keeps the failure path from becoming a cheap
	// username-enumeration timing oracle.
	dummyHash = hashPrefix + "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
)

func Hash(password string) (string, error) {
	digest := sha256.Sum256([]byte(password))
	hash, err := bcrypt.GenerateFromPassword(digest[:], bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return hashPrefix + string(hash), nil
}

func IsHash(value string) bool {
	if !strings.HasPrefix(value, hashPrefix) {
		return false
	}
	_, err := bcrypt.Cost([]byte(strings.TrimPrefix(value, hashPrefix)))
	return err == nil
}

func Verify(stored, password string) bool {
	if IsHash(stored) {
		digest := sha256.Sum256([]byte(password))
		return bcrypt.CompareHashAndPassword([]byte(strings.TrimPrefix(stored, hashPrefix)), digest[:]) == nil
	}
	// This fallback keeps a running instance compatible while a legacy
	// plaintext database is being opened and migrated.
	return subtle.ConstantTimeCompare([]byte(stored), []byte(password)) == 1
}

// VerifyUser performs a password check with comparable work for present and
// absent usernames. The boolean is true only for an existing matching user.
func VerifyUser(users map[string]string, username, password string) bool {
	stored, exists := users[username]
	if !exists {
		stored = dummyHash
	}
	valid := Verify(stored, password)
	return exists && valid
}
