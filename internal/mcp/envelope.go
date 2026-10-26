package mcp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the MCP revision this server implements. Clients
// may send it as the MCP-Protocol-Version header on every request
// after initialize; a mismatch is rejected by the hub. The same header
// is added to every outbound request to a downstream backend, using
// whatever version that backend announced during initialize.
const ProtocolVersion = "2025-11-25"

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// Envelope is a JSON-RPC 2.0 message: request, notification or response.
type Envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// MessageKind classifies an envelope.
type MessageKind int

const (
	KindRequest MessageKind = iota
	KindNotification
	KindResponse
)

// ParseEnvelope validates a JSON-RPC 2.0 envelope and classifies it.
// On parse or validation error it returns an RPCError suitable for
// sending back to the client with id:null.
func ParseEnvelope(raw []byte) (*Envelope, MessageKind, *RPCError) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, 0, &RPCError{Code: -32700, Message: "parse error"}
	}
	if env.JSONRPC != "2.0" {
		return nil, 0, &RPCError{Code: -32600, Message: `jsonrpc must be "2.0"`}
	}

	if env.Method != "" {
		if len(env.ID) > 0 {
			if !validID(env.ID) {
				return nil, 0, &RPCError{Code: -32600, Message: "id must be a string or number (not null/object/array/bool)"}
			}
			return &env, KindRequest, nil
		}
		return &env, KindNotification, nil
	}

	// Response: no method, has id.
	if len(env.ID) > 0 {
		if !validID(env.ID) {
			return nil, 0, &RPCError{Code: -32600, Message: "id must be a string or number"}
		}
		return &env, KindResponse, nil
	}
	return nil, 0, &RPCError{Code: -32600, Message: "message has neither method nor id"}
}

// validID reports whether raw is a valid JSON-RPC id: a string or a
// number. null, objects, arrays and booleans are rejected.
func validID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	if string(raw) == "null" {
		return false
	}
	switch raw[0] {
	case '"': // string
		return true
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return false
}

// isNotificationMethod reports whether an id-less request is a valid
// MCP notification (as opposed to a client mistake like tools/call
// without an id).
func isNotificationMethod(method string) bool {
	switch method {
	case "notifications/initialized",
		"notifications/cancelled",
		"notifications/progress",
		"notifications/roots/list_changed":
		return true
	}
	return false
}
