package store

import (
	"context"
	"testing"
	"time"
)

func TestVohiveEventCRUD(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	serverID := int64(7)
	event := VohiveEvent{
		Type:      VohiveEventDegraded,
		DeviceID:  "modem-001",
		ServerID:  &serverID,
		Message:   "link quality dropped",
		Details:   map[string]any{"rsrp": -110, "band": "n78"},
		CreatedAt: time.Now().UTC(),
	}
	id, err := s.SaveVohiveEvent(context.Background(), event)
	if err != nil {
		t.Fatalf("save event: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero event id")
	}

	events, err := s.ListVohiveEvents(context.Background(), ListVohiveEventsOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	got := events[0]
	if got.ID != id || got.Type != VohiveEventDegraded || got.DeviceID != "modem-001" || got.Message != "link quality dropped" {
		t.Fatalf("event mismatch: %+v", got)
	}
	if got.ServerID == nil || *got.ServerID != serverID {
		t.Fatalf("server_id mismatch: %+v", got.ServerID)
	}
	if got.Details["rsrp"] != float64(-110) || got.Details["band"] != "n78" {
		t.Fatalf("details round-trip failed: %+v", got.Details)
	}
}

func TestListVohiveEventsFilters(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	base := time.Now().UTC()
	seed := []VohiveEvent{
		{Type: VohiveEventDegraded, DeviceID: "dev-a", Message: "a degraded", Details: map[string]any{}, CreatedAt: base.Add(-4 * time.Minute)},
		{Type: VohiveEventRecovered, DeviceID: "dev-a", Message: "a recovered", Details: map[string]any{}, CreatedAt: base.Add(-3 * time.Minute)},
		{Type: VohiveEventDegraded, DeviceID: "dev-b", Message: "b degraded", Details: map[string]any{}, CreatedAt: base.Add(-2 * time.Minute)},
		{Type: VohiveEventRecoveryStarted, DeviceID: "dev-b", Message: "b recovery", Details: map[string]any{}, CreatedAt: base.Add(-1 * time.Minute)},
	}
	for _, e := range seed {
		if _, err := s.SaveVohiveEvent(context.Background(), e); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	byDevice, err := s.ListVohiveEvents(context.Background(), ListVohiveEventsOptions{DeviceID: "dev-a"})
	if err != nil {
		t.Fatalf("filter by device: %v", err)
	}
	if len(byDevice) != 2 {
		t.Fatalf("expected 2 events for dev-a, got %d", len(byDevice))
	}
	for _, e := range byDevice {
		if e.DeviceID != "dev-a" {
			t.Fatalf("unexpected device in filter result: %+v", e)
		}
	}

	byType, err := s.ListVohiveEvents(context.Background(), ListVohiveEventsOptions{Type: VohiveEventDegraded})
	if err != nil {
		t.Fatalf("filter by type: %v", err)
	}
	if len(byType) != 2 {
		t.Fatalf("expected 2 degraded events, got %d", len(byType))
	}
	for _, e := range byType {
		if e.Type != VohiveEventDegraded {
			t.Fatalf("unexpected type in filter result: %+v", e)
		}
	}
}

func TestListVohiveEventsBeforeID(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	base := time.Now().UTC()
	ids := make([]int64, 3)
	for i := range ids {
		e := VohiveEvent{
			Type:      VohiveEventDegraded,
			DeviceID:  "dev-paged",
			Message:   "event",
			Details:   map[string]any{},
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		id, err := s.SaveVohiveEvent(context.Background(), e)
		if err != nil {
			t.Fatalf("seed event: %v", err)
		}
		ids[i] = id
	}

	// Before the largest id should return the two earlier events, ordered newest first.
	events, err := s.ListVohiveEvents(context.Background(), ListVohiveEventsOptions{BeforeID: ids[2]})
	if err != nil {
		t.Fatalf("list before id: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events before id %d, got %d", ids[2], len(events))
	}
	if events[0].ID != ids[1] || events[1].ID != ids[0] {
		t.Fatalf("unexpected order or ids: %+v", events)
	}
}

func TestPruneVohiveEvents(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()

	oldEvent := VohiveEvent{
		Type:      VohiveEventDegraded,
		DeviceID:  "dev-prune",
		Message:   "old event",
		Details:   map[string]any{},
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour),
	}
	recentEvent := VohiveEvent{
		Type:      VohiveEventRecovered,
		DeviceID:  "dev-prune",
		Message:   "recent event",
		Details:   map[string]any{},
		CreatedAt: time.Now().UTC().Add(-30 * time.Minute),
	}
	if _, err := s.SaveVohiveEvent(context.Background(), oldEvent); err != nil {
		t.Fatalf("save old event: %v", err)
	}
	if _, err := s.SaveVohiveEvent(context.Background(), recentEvent); err != nil {
		t.Fatalf("save recent event: %v", err)
	}

	pruned, err := s.PruneVohiveEvents(context.Background(), 1*time.Hour)
	if err != nil {
		t.Fatalf("prune events: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 pruned event, got %d", pruned)
	}

	remaining, err := s.ListVohiveEvents(context.Background(), ListVohiveEventsOptions{})
	if err != nil {
		t.Fatalf("list after prune: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected 1 remaining event, got %d", len(remaining))
	}
	if remaining[0].Message != recentEvent.Message {
		t.Fatalf("wrong event survived prune: %+v", remaining[0])
	}
}
