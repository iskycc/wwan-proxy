package proxyauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"hash"
	"strings"
	"sync"

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

// Verifier caches only successful credential checks for one immutable server
// configuration generation. The cached value is keyed with process memory
// entropy, so neither plaintext passwords nor reusable unsalted password
// digests are retained. Invalid credentials still take the full bcrypt path to
// avoid turning the cache into a username oracle.
type Verifier struct {
	users        map[string]string
	key          [32]byte
	cacheEnabled bool

	mu      sync.RWMutex
	allowed map[string][32]byte
	macs    sync.Pool
}

func NewVerifier(users map[string]string) *Verifier {
	v := &Verifier{users: users, allowed: make(map[string][32]byte, len(users))}
	_, err := rand.Read(v.key[:])
	v.cacheEnabled = err == nil
	if v.cacheEnabled {
		v.macs.New = func() any { return hmac.New(sha256.New, v.key[:]) }
	}
	return v
}

func (v *Verifier) Verify(username, password string) bool {
	if v == nil || !v.cacheEnabled {
		if v == nil {
			return false
		}
		return VerifyUser(v.users, username, password)
	}
	digest := v.credentialDigest(password)
	v.mu.RLock()
	cached, ok := v.allowed[username]
	v.mu.RUnlock()
	if ok && hmac.Equal(cached[:], digest[:]) {
		return true
	}
	if !VerifyUser(v.users, username, password) {
		return false
	}
	v.mu.Lock()
	v.allowed[username] = digest
	v.mu.Unlock()
	return true
}

func (v *Verifier) credentialDigest(password string) [32]byte {
	mac := v.macs.Get().(hash.Hash)
	mac.Reset()
	_, _ = mac.Write([]byte(password))
	var digest [32]byte
	_ = mac.Sum(digest[:0])
	v.macs.Put(mac)
	return digest
}
