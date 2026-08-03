package webui

import (
	"testing"
	"time"
)

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < 5; i++ {
		l.failure("192.0.2.1")
	}
	if allowed, retry := l.allow("192.0.2.1"); allowed || retry <= 0 || retry > 5*time.Minute {
		t.Fatalf("allowed=%v retry=%v", allowed, retry)
	}
	l.success("192.0.2.1")
	if allowed, _ := l.allow("192.0.2.1"); !allowed {
		t.Fatal("successful login should clear limiter")
	}
}

func TestCredentialValidation(t *testing.T) {
	if err := validateCredentials(authRequest{Username: "ad", Password: "StrongPassword!42"}); err == nil {
		t.Fatal("short username accepted")
	}
	if err := validateCredentials(authRequest{Username: "admin", Password: "short"}); err == nil {
		t.Fatal("short password accepted")
	}
	if err := validateCredentials(authRequest{Username: "admin", Password: "StrongPassword!42"}); err != nil {
		t.Fatal(err)
	}
}
