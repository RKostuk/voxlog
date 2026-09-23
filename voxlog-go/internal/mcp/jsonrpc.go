package mcp

import "encoding/json"

// The JSON-RPC 2.0 envelope, hand-written rather than taken from a library.
// What MCP needs of it is five methods and four error codes; an SDK for that
// would be a pre-1.0 dependency, a schema-reflection tree and a second owner
// of the server's lifecycle, in a module that has four direct dependencies
// and a listener that has to appear and disappear behind a checkbox.

const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether this request wants no answer. A JSON-RPC
// notification has no id, and answering one is a protocol error -- which is
// how "notifications/initialized" must be treated.
func (r request) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func result(id json.RawMessage, v any) response {
	return response{JSONRPC: "2.0", ID: id, Result: v}
}

func failure(id json.RawMessage, code int, message string) response {
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}
