package mcp

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// protocolVersion is the MCP revision this server implements. Echoed back to
// a client that asks for the same one; a client asking for anything else is
// told what we speak and left to decide.
const protocolVersion = "2025-06-18"

// maxBody is the ceiling on a request body. A JSON-RPC call has no business
// being larger, and unlike the window's page server this one takes a body at
// all.
const maxBody = 1 << 20

// inFlight bounds how many calls are served at once. The meetings database
// is opened with SetMaxOpenConns(1), so a chatty client would otherwise
// queue behind itself in front of a live meeting's own writes.
const inFlight = 2

// handler serves the whole protocol on one route.
type handler struct {
	deps       Deps
	token      string
	allowWrite func() bool
	slots      chan struct{}
}

func newHandler(deps Deps, token string, allowWrite func() bool) *handler {
	if allowWrite == nil {
		allowWrite = func() bool { return false }
	}
	return &handler{
		deps:       deps,
		token:      token,
		allowWrite: allowWrite,
		slots:      make(chan struct{}, inFlight),
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Order matters: nothing about this request is trusted until the token
	// matches, and a mismatch must look exactly like a route that was never
	// registered -- the same rule internal/ui's page server keeps, so a
	// caller cannot tell "wrong token" from "no such server" by status code.
	if !h.authorized(r) {
		http.NotFound(w, r)
		return
	}
	if !originAllowed(r.Header.Get("Origin")) {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.post(w, r)
	case http.MethodDelete:
		// Ending a session, for clients that send it. There are no sessions
		// here, so there is nothing to end and nothing to report.
		w.WriteHeader(http.StatusOK)
	default:
		// No server-initiated streams, so there is nothing for a GET to open.
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// authorized accepts the token in the Authorization header, or as the first
// path segment for clients that cannot set headers.
func (h *handler) authorized(r *http.Request) bool {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if tokensMatch(bearer, h.token) {
			return true
		}
	}
	segment, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return tokensMatch(segment, h.token)
}

func tokensMatch(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// originAllowed is the DNS-rebinding defence the MCP spec asks for: a page in
// a browser must not be able to reach this server by resolving a hostname to
// 127.0.0.1. A request with no Origin at all is a program, not a page.
func originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	rest, ok := strings.CutPrefix(origin, "http://")
	if !ok {
		return false
	}
	host, _, _ := strings.Cut(rest, ":")
	return host == "127.0.0.1" || host == "localhost"
}

func (h *handler) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeJSON(w, http.StatusOK, failure(nil, codeParseError, "the request body could not be read"))
		return
	}

	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusOK, failure(nil, codeParseError, "the request is not JSON"))
		return
	}
	if req.Method == "" {
		writeJSON(w, http.StatusOK, failure(req.ID, codeInvalidRequest, "the request names no method"))
		return
	}

	// A notification wants no answer at all, and answering one is itself a
	// protocol error. "notifications/initialized" is the one that matters.
	if req.isNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		writeJSON(w, http.StatusOK, failure(req.ID, codeInternalError, "Voxlog is busy; try again in a moment"))
		return
	}

	writeJSON(w, http.StatusOK, h.dispatch(req))
}

func (h *handler) dispatch(req request) response {
	switch req.Method {
	case "initialize":
		return result(req.ID, h.initialize(req.Params))
	case "ping":
		return result(req.ID, map[string]any{})
	case "tools/list":
		return result(req.ID, map[string]any{"tools": h.toolList()})
	case "tools/call":
		return h.call(req)
	default:
		return failure(req.ID, codeMethodNotFound, fmt.Sprintf("%q is not a method this server has", req.Method))
	}
}

func (h *handler) initialize(params json.RawMessage) map[string]any {
	version := protocolVersion
	var asked struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &asked); err == nil && asked.ProtocolVersion == protocolVersion {
		version = asked.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "voxlog", "version": "0.1.0"},
		"instructions": "Voxlog is a local dictation and meeting recorder. " +
			"Notes are things the user dictated; meetings are recorded calls with " +
			"per-speaker transcripts. Everything here is private to this machine.",
	}
}

// toolList hides the writing tools entirely when writes are off, rather than
// advertising tools that refuse: a read-only install should have no write
// tool to call.
func (h *handler) toolList() []map[string]any {
	writable := h.allowWrite()
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Write && !writable {
			continue
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": json.RawMessage(t.InputSchema),
		})
	}
	return out
}

func (h *handler) call(req request) response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return failure(req.ID, codeInvalidParams, "the call names no tool")
	}

	for _, t := range tools {
		if t.Name != params.Name {
			continue
		}
		if t.Write && !h.allowWrite() {
			return result(req.ID, toolError("Writing is turned off. Enable it in Voxlog's Settings, under MCP."))
		}
		value, err := t.Run(h.deps, params.Arguments)
		if err != nil {
			// A tool that could not do its job reports that as a RESULT, not
			// as a transport error: the client is meant to read it, reason
			// about it and try something else, which it cannot do with a
			// JSON-RPC error.
			log.Printf("mcp: %s: %v", t.Name, err)
			return result(req.ID, toolError(err.Error()))
		}
		payload, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return result(req.ID, toolError("the answer could not be encoded"))
		}
		return result(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(payload)}},
		})
	}
	return failure(req.ID, codeInvalidParams, fmt.Sprintf("%q is not a tool this server has", params.Name))
}

func toolError(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("mcp: writing a response: %v", err)
	}
}
