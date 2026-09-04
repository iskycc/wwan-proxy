package proxyauth

import (
	"strings"
	"testing"
)

func TestHashAndVerifyLongPassword(t *testing.T) {
	password := strings.Repeat("p", 200)
	hash, err := Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	if !IsHash(hash) || !Verify(hash, password) || Verify(hash, password+"x") {
		t.Fatal("password hash verification failed")
	}
}

func TestLegacyPlaintextFallback(t *testing.T) {
	if !Verify("legacy", "legacy") || Verify("legacy", "wrong") {
		t.Fatal("legacy verification failed")
	}
}

func TestVerifyUser(t *testing.T) {
	hash, err := Hash("secret")
	if err != nil {
		t.Fatal(err)
	}
	users := map[string]string{"alice": hash}
	if !VerifyUser(users, "alice", "secret") || VerifyUser(users, "alice", "wrong") || VerifyUser(users, "missing", "secret") {
		t.Fatal("user verification returned an invalid result")
	}
}

func TestVerifierCachesOnlySuccessfulCredentials(t *testing.T) {
	hash, err := Hash("secret")
	if err != nil {
		t.Fatal(err)
	}
	users := map[string]string{"alice": hash}
	verifier := NewVerifier(users)
	if !verifier.Verify("alice", "secret") {
		t.Fatal("valid credential was rejected")
	}
	if verifier.Verify("alice", "wrong") || verifier.Verify("missing", "secret") {
		t.Fatal("invalid credential was accepted")
	}

	// A successful check is served by the generation-local cache. Replacing the
	// configured hash demonstrates that a second bcrypt check was not performed.
	users["alice"] = dummyHash
	if !verifier.Verify("alice", "secret") {
		t.Fatal("successful credential was not cached")
	}
	if NewVerifier(users).Verify("alice", "secret") {
		t.Fatal("credential cache leaked into a new configuration generation")
	}
}

func BenchmarkVerifierCached(b *testing.B) {
	hash, err := Hash("benchmark-secret")
	if err != nil {
		b.Fatal(err)
	}
	verifier := NewVerifier(map[string]string{"alice": hash})
	if !verifier.Verify("alice", "benchmark-secret") {
		b.Fatal("credential warm-up failed")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !verifier.Verify("alice", "benchmark-secret") {
			b.Fatal("cached credential failed")
		}
	}
}
