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
