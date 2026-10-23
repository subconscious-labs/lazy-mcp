package session

import (
	"context"
	"testing"
	"time"
)

func TestSession_CanSeeServer(t *testing.T) {
	admin := New("a", "root", "admin", nil)
	if !admin.CanSeeServer("anything") {
		t.Fatalf("admin should see any server")
	}

	agent := New("b", "elsa", "agent", []string{"git", "tasks"})
	if !agent.CanSeeServer("git") || !agent.CanSeeServer("tasks") {
		t.Fatalf("allowed servers should be visible")
	}
	if agent.CanSeeServer("ssh") {
		t.Fatalf("non-allowed server must not be visible")
	}
}

func TestSession_Context(t *testing.T) {
	s := LocalAdmin()
	ctx := WithContext(context.Background(), s)
	got, ok := FromContext(ctx)
	if !ok || got != s {
		t.Fatalf("session not round-tripped through context")
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatalf("empty context should return ok=false")
	}
}

func TestSession_Touch(t *testing.T) {
	s := New("x", "u", "agent", nil)
	t0 := s.LastSeen()
	time.Sleep(5 * time.Millisecond)
	s.Touch()
	if !s.LastSeen().After(t0) {
		t.Fatalf("Touch should update lastSeen")
	}
}
