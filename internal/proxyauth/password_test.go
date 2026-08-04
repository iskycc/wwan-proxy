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
