package catalog

import "testing"

func TestScoreWord_Name(t *testing.T) {
	e := ToolEntry{
		Ref:     "ssh/ssh_exec",
		Server:  "ssh",
		Tool:    "ssh_exec",
		Summary: "Execute a command on a remote host via SSH",
		Tags:    []string{"ssh"},
	}
	if got := scoreWord("ssh_exec", e); got <= 0 {
		t.Fatalf("exact name should match, got %v", got)
	}
	if got := scoreWord("zzz", e); got != 0 {
		t.Fatalf("no match expected, got %v", got)
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
