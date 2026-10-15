package catalog

import "testing"

func TestScore_MultiWordQuery(t *testing.T) {
	e := ToolEntry{
		Ref:     "ssh/ssh_exec",
		Server:  "ssh",
		Tool:    "ssh_exec",
		Summary: "Execute a command on a remote host via SSH",
		Tags:    []string{"ssh"},
	}
	if got := score("execute command", e); got <= 0 {
		t.Fatalf("expected match for multi-word query, got %v", got)
	}
	if got := score("execute nonsense", e); got != 0 {
		t.Fatalf("expected no match when a word is missing, got %v", got)
	}
	if got := score("ssh", e); got <= 0 {
		t.Fatalf("expected match for tool name, got %v", got)
	}
}

func TestScore_EmptyQueryMatchesAll(t *testing.T) {
	e := ToolEntry{Tool: "anything"}
	if got := score("", e); got <= 0 {
		t.Fatalf("empty query should match, got %v", got)
	}
}

