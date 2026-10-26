package broker

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEScanner_SingleLine(t *testing.T) {
	r := strings.NewReader("data: {\"a\":1}\n\n")
	s := newSSEScanner(r, 1024)
	data, err := s.next()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"a":1}` {
		t.Fatalf("got %q", data)
	}
	if _, err := s.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestSSEScanner_MultiLine(t *testing.T) {
	r := strings.NewReader("data: line1\ndata: line2\n\n")
	s := newSSEScanner(r, 1024)
	data, err := s.next()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "line1\nline2" {
		t.Fatalf("got %q", data)
	}
}

func TestSSEScanner_SkipsNonData(t *testing.T) {
	r := strings.NewReader(": comment\nevent: message\nid: 1\ndata: payload\n\n")
	s := newSSEScanner(r, 1024)
	data, err := s.next()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "payload" {
		t.Fatalf("got %q", data)
	}
}

func TestSSEScanner_RejectsOversizedEvent(t *testing.T) {
	r := strings.NewReader("data: " + strings.Repeat("x", 2048) + "\n\n")
	s := newSSEScanner(r, 1024)
	if _, err := s.next(); err == nil {
		t.Fatalf("expected error for oversized event")
	}
}
