package mcp

import "testing"

func TestParseEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		kind    MessageKind
		wantErr bool
	}{
		{"request with int id", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, KindRequest, false},
		{"request with string id", `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`, KindRequest, false},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, KindNotification, false},
		{"response", `{"jsonrpc":"2.0","id":1,"result":{}}`, KindResponse, false},

		{"bad jsonrpc", `{"jsonrpc":"1.0","id":1,"method":"x"}`, 0, true},
		{"null id", `{"jsonrpc":"2.0","id":null,"method":"x"}`, 0, true},
		{"object id", `{"jsonrpc":"2.0","id":{},"method":"x"}`, 0, true},
		{"array id", `{"jsonrpc":"2.0","id":[],"method":"x"}`, 0, true},
		{"bool id", `{"jsonrpc":"2.0","id":true,"method":"x"}`, 0, true},
		{"empty object", `{}`, 0, true},
		{"parse error", `not json`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, kind, rpcErr := ParseEnvelope([]byte(tt.in))
			if tt.wantErr {
				if rpcErr == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if rpcErr != nil {
				t.Fatalf("unexpected error: %v", rpcErr)
			}
			if kind != tt.kind {
				t.Fatalf("kind: got %d want %d", kind, tt.kind)
			}
		})
	}
}
