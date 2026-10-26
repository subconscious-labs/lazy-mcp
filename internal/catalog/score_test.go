package catalog

import "testing"

func TestComputeLower_Idempotent(t *testing.T) {
	e := ToolEntry{
		Ref:     "SSH/SSH_Exec",
		Server:  "SSH",
		Tool:    "SSH_Exec",
		Summary: "Mixed Case Summary",
		Tags:    []string{"SSH", "Exec"},
	}
	e.computeLower()
	first := e.toolLower
	e.computeLower()
	if e.toolLower != first {
		t.Fatal("computeLower should be idempotent")
	}
	if e.toolLower != "ssh_exec" {
		t.Fatalf("unexpected lower: %q", e.toolLower)
	}
	if e.tagsLower[0] != "ssh" || e.tagsLower[1] != "exec" {
		t.Fatalf("tags not lowercased: %v", e.tagsLower)
	}
}

func TestScoreWord_Name(t *testing.T) {
	e := ToolEntry{
		Ref:     "ssh/ssh_exec",
		Server:  "ssh",
		Tool:    "ssh_exec",
		Summary: "Execute a command on a remote host via SSH",
		Tags:    []string{"ssh"},
	}
	e.computeLower()

	if got := scoreWord("ssh_exec", &e); got <= 0 {
		t.Fatalf("exact name should match, got %v", got)
	}
	if got := scoreWord("zzz", &e); got != 0 {
		t.Fatalf("no match expected, got %v", got)
	}
}

func TestScoreWord_MatchesSomething(t *testing.T) {
	e := ToolEntry{
		Ref:     "ssh/ssh_exec",
		Server:  "ssh",
		Tool:    "ssh_exec",
		Summary: "Execute a command on a remote host via SSH",
		Tags:    []string{"ssh", "exec"},
	}
	e.computeLower()

	yes := []string{"ssh", "ssh_exec", "exec", "command", "host", "remote"}
	for _, w := range yes {
		if scoreWord(w, &e) <= 0 {
			t.Errorf("scoreWord(%q) = 0, want > 0", w)
		}
	}
	no := []string{"zzz", "database", "totally-unrelated"}
	for _, w := range no {
		if scoreWord(w, &e) != 0 {
			t.Errorf("scoreWord(%q) > 0, want 0", w)
		}
	}
}

func TestWordMatchesServerMeta(t *testing.T) {
	tests := []struct {
		word        string
		serverName  string
		description string
		tags        []string
		want        bool
	}{
		{"stats", "monitor", "Alerts and stats for infrastructure", []string{"alerts"}, true},
		{"alerts", "monitor", "Alerts and stats for infrastructure", []string{"alerts"}, true},
		{"monitor", "monitor", "Alerts and stats for infrastructure", []string{"alerts"}, true},
		{"nonsense", "monitor", "Alerts and stats for infrastructure", []string{"alerts"}, false},
	}
	for _, tt := range tests {
		got := wordMatchesServerMeta(tt.word, tt.serverName, tt.description, tt.tags)
		if got != tt.want {
			t.Errorf("wordMatchesServerMeta(%q, %q, ...) = %v, want %v",
				tt.word, tt.serverName, got, tt.want)
		}
	}
}
