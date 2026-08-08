package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdminAndSessionPersistence(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	initialized, err := s.AdminInitialized(ctx)
	if err != nil || initialized {
		t.Fatalf("initialized=%v err=%v", initialized, err)
	}
	if err := s.CreateAdmin(ctx, "administrator", []byte("bcrypt-hash")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAdmin(ctx, "other", []byte("hash")); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("unexpected duplicate error %v", err)
	}
	admin, err := s.Admin(ctx)
	if err != nil || admin.Username != "administrator" {
		t.Fatalf("admin=%+v err=%v", admin, err)
	}
	if err := s.CreateSession(ctx, "token-hash", "127.0.0.1", "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	username, valid, err := s.ValidateSession(ctx, "token-hash", time.Now())
	if err != nil || !valid || username != "administrator" {
		t.Fatalf("user=%q valid=%v err=%v", username, valid, err)
	}
	if err := s.DeleteSession(ctx, "token-hash"); err != nil {
		t.Fatal(err)
	}
	_, valid, err = s.ValidateSession(ctx, "token-hash", time.Now())
	if err != nil || valid {
		t.Fatalf("deleted session valid=%v err=%v", valid, err)
	}
}

func TestApplySessionLifetimeUpdatesAndExpiresExistingSessions(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	if err := s.CreateAdmin(ctx, "administrator", []byte("bcrypt-hash")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateSession(ctx, "recent", "127.0.0.1", "test", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "old", "127.0.0.1", "test", now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE web_sessions SET created_at=? WHERE token_hash=?`, now.Add(-10*time.Minute).Format(time.RFC3339Nano), "old"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplySessionLifetime(ctx, now, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	_, recentExpiry, valid, err := s.ValidateSessionExpiry(ctx, "recent", now)
	if err != nil || !valid {
		t.Fatalf("recent valid=%v err=%v", valid, err)
	}
	if delta := recentExpiry.Sub(now); delta < 4*time.Minute || delta > 6*time.Minute {
		t.Fatalf("recent expiry delta=%v", delta)
	}
	if _, _, valid, err := s.ValidateSessionExpiry(ctx, "old", now); err != nil || valid {
		t.Fatalf("old valid=%v err=%v", valid, err)
	}
}
