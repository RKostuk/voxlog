package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"voxlog-go/internal/history"
	"voxlog-go/internal/task"
)

const testToken = "0123456789abcdef0123456789abcdef"

// testServer starts a server over empty stores. Tests that need data seed it
// through the stores themselves, the way the app does.
func testServer(t *testing.T, allowWrite bool) (*Server, Deps) {
	t.Helper()
	deps := Deps{
		Notes:    history.NewStore(t.TempDir()),
		Meetings: history.NewMeetingStore(t.TempDir()),
		Tasks:    task.NewStore(t.TempDir()),
	}
	srv, err := Start(Config{AllowWrite: func() bool { return allowWrite }}, deps, testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, deps
}

// post sends one JSON-RPC call with the right token and returns the decoded
// reply along with the status code.
func post(t *testing.T, srv *Server, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL(), bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	json.NewDecoder(res.Body).Decode(&decoded)
	return res.StatusCode, decoded
}

// call runs one tool and returns its result object.
func call(t *testing.T, srv *Server, name, args string) map[string]any {
	t.Helper()
	if args == "" {
		args = "{}"
	}
	_, reply := post(t, srv, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args))
	res, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("calling %s produced no result: %v", name, reply)
	}
	return res
}

// payload pulls the JSON a tool returned out of its text content block.
func payload(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("the result carries no content: %v", res)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("the result is not JSON: %v\n%s", err, text)
	}
	return out
}

// Same rule as the window's page server: a caller must not be able to tell a
// wrong token from a server that was never there.
func TestAWrongTokenIsIndistinguishableFromNoServer(t *testing.T) {
	srv, _ := testServer(t, false)

	req, _ := http.NewRequest(http.MethodPost, srv.URL(), bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer not-the-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d for a wrong token, want 404", res.StatusCode)
	}
}

func TestNoTokenAtAllIsAlso404(t *testing.T) {
	srv, _ := testServer(t, false)

	res, err := http.Post(srv.URL(), "application/json", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d with no token, want 404", res.StatusCode)
	}
}

func TestTheTokenIsAlsoAcceptedInThePath(t *testing.T) {
	srv, _ := testServer(t, false)

	url := fmt.Sprintf("http://127.0.0.1:%d/%s/mcp", srv.Port(), testToken)
	res, err := http.Post(url, "application/json", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("got %d for the token in the path, want 200", res.StatusCode)
	}
}

// A page in a browser must not reach this server by resolving a name to
// 127.0.0.1.
func TestAForeignOriginIsRefused(t *testing.T) {
	srv, _ := testServer(t, false)

	req, _ := http.NewRequest(http.MethodPost, srv.URL(), bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Origin", "https://example.com")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d for a foreign Origin, want 404", res.StatusCode)
	}
}

func TestAGetHasNothingToOpen(t *testing.T) {
	srv, _ := testServer(t, false)

	req, _ := http.NewRequest(http.MethodGet, srv.URL(), nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("got %d for a GET, want 405", res.StatusCode)
	}
}

func TestInitializeAdvertisesTools(t *testing.T) {
	srv, _ := testServer(t, false)

	_, reply := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+protocolVersion+`"}}`)
	res, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize produced no result: %v", reply)
	}
	if res["protocolVersion"] != protocolVersion {
		t.Fatalf("got protocol version %v, want %s", res["protocolVersion"], protocolVersion)
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("initialize does not advertise tools: %v", caps)
	}
}

func toolNames(t *testing.T, srv *Server) map[string]bool {
	t.Helper()
	_, reply := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	res, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list produced no result: %v", reply)
	}
	list, _ := res["tools"].([]any)
	names := map[string]bool{}
	for _, entry := range list {
		name, _ := entry.(map[string]any)["name"].(string)
		names[name] = true
	}
	return names
}

func TestToolsListCarriesEveryReadingTool(t *testing.T) {
	srv, _ := testServer(t, false)

	names := toolNames(t, srv)
	for _, want := range []string{
		"list_notes", "get_note", "list_meetings", "get_meeting",
		"search_transcripts", "list_people", "list_tasks", "get_task", "list_projects",
	} {
		if !names[want] {
			t.Errorf("tools/list does not offer %s", want)
		}
	}
}

// Hidden, not refusing: a read-only install should have no write tool to
// call rather than one that answers with an apology.
func TestTheWritingToolsAppearOnlyWhenWritesAreAllowed(t *testing.T) {
	off, _ := testServer(t, false)
	if names := toolNames(t, off); names["create_task"] || names["update_task_status"] {
		t.Error("the writing tools are offered while writes are turned off")
	}

	on, _ := testServer(t, true)
	names := toolNames(t, on)
	if !names["create_task"] || !names["update_task_status"] {
		t.Error("the writing tools are missing while writes are turned on")
	}
}

func TestAnUnknownMethodIsMethodNotFound(t *testing.T) {
	srv, _ := testServer(t, false)

	_, reply := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/invent"}`)
	failure, ok := reply["error"].(map[string]any)
	if !ok {
		t.Fatalf("an unknown method produced no error: %v", reply)
	}
	if int(failure["code"].(float64)) != codeMethodNotFound {
		t.Fatalf("got code %v, want %d", failure["code"], codeMethodNotFound)
	}
}

func TestABodyThatIsNotJSONIsAParseError(t *testing.T) {
	srv, _ := testServer(t, false)

	_, reply := post(t, srv, `not json at all`)
	failure, ok := reply["error"].(map[string]any)
	if !ok {
		t.Fatalf("a malformed body produced no error: %v", reply)
	}
	if int(failure["code"].(float64)) != codeParseError {
		t.Fatalf("got code %v, want %d", failure["code"], codeParseError)
	}
}

// Answering a notification is itself a protocol error, and
// notifications/initialized is the one every client sends.
func TestANotificationGetsNoAnswer(t *testing.T) {
	srv, _ := testServer(t, false)

	req, _ := http.NewRequest(http.MethodPost, srv.URL(),
		bytes.NewBufferString(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("got %d for a notification, want 202", res.StatusCode)
	}
	if res.ContentLength > 0 {
		t.Fatalf("a notification was answered with %d bytes", res.ContentLength)
	}
}

func TestCloseStopsAnswering(t *testing.T) {
	srv, _ := testServer(t, false)
	url := srv.URL()
	srv.Close()

	if _, err := http.Post(url, "application/json", bytes.NewBufferString(`{}`)); err == nil {
		t.Fatal("the server still answered after Close")
	}
}

// The remembered port is a preference, not a requirement: something else
// holding it must not leave the user with a server that never came up.
func TestATakenPortFallsBackToAFreeOne(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	taken := blocker.Addr().(*net.TCPAddr).Port

	deps := Deps{
		Notes:    history.NewStore(t.TempDir()),
		Meetings: history.NewMeetingStore(t.TempDir()),
		Tasks:    task.NewStore(t.TempDir()),
	}
	srv, err := Start(Config{Port: taken}, deps, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.Port() == taken {
		t.Fatalf("the server bound %d, which was already taken", taken)
	}
}

func TestServingWithoutATokenIsRefused(t *testing.T) {
	if _, err := Start(Config{}, Deps{}, ""); err == nil {
		t.Fatal("a server with no token started anyway")
	}
}

// ------------------------------------------------------------------- tools

func TestListNotesHonoursDayAndLimit(t *testing.T) {
	srv, deps := testServer(t, false)
	day := time.Date(2026, 9, 20, 9, 0, 0, 0, time.Local)
	for i := 0; i < 3; i++ {
		if err := deps.Notes.Append(history.Entry{
			Timestamp:        day.Add(time.Duration(i) * time.Hour),
			RecordingSeconds: 4,
			Text:             fmt.Sprintf("note %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A note on another day, which the day filter must leave out.
	if err := deps.Notes.Append(history.Entry{
		Timestamp: day.AddDate(0, 0, 1), RecordingSeconds: 4, Text: "tomorrow",
	}); err != nil {
		t.Fatal(err)
	}

	out := payload(t, call(t, srv, "list_notes", `{"day":"2026-09-20"}`))
	notes, _ := out["notes"].([]any)
	if len(notes) != 3 {
		t.Fatalf("got %d notes for the day, want 3: %v", len(notes), notes)
	}

	out = payload(t, call(t, srv, "list_notes", `{"day":"2026-09-20","limit":2}`))
	notes, _ = out["notes"].([]any)
	if len(notes) != 2 {
		t.Fatalf("got %d notes with limit 2", len(notes))
	}
}

// Transcripts go out, the paths of the recordings behind them do not.
func TestNotesCarryNoAudioPaths(t *testing.T) {
	srv, deps := testServer(t, false)
	at := time.Now()
	if err := deps.Notes.Append(history.Entry{
		Timestamp: at, RecordingSeconds: 3, Text: "said something", AudioPath: "/tmp/secret.wav",
	}); err != nil {
		t.Fatal(err)
	}

	res := call(t, srv, "list_notes", "{}")
	content, _ := res["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	if bytes.Contains([]byte(text), []byte("secret.wav")) {
		t.Fatalf("the note list gave out an audio path:\n%s", text)
	}
}

func TestGetMeetingCarriesSpeakersAndTurns(t *testing.T) {
	srv, deps := testServer(t, false)
	start := time.Date(2026, 9, 21, 15, 0, 0, 0, time.Local)
	if err := deps.Meetings.Append(history.Meeting{
		Start: start, RecordingSeconds: 600, Text: "a call", Summary: "decided to ship",
	}); err != nil {
		t.Fatal(err)
	}
	speakers := []history.MeetingSpeaker{
		{LocalID: history.YouSpeaker, TalkSecs: 30, TurnCount: 1},
		{LocalID: 1, TalkSecs: 20, TurnCount: 1, Embed: []float32{1, 0, 0}},
	}
	turns := []history.Turn{
		{Channel: history.ChannelMic, StartSecs: 0, EndSecs: 5, LocalID: history.YouSpeaker, Text: "shall we ship"},
		{Channel: history.ChannelSystem, StartSecs: 5, EndSecs: 9, LocalID: 1, Text: "yes, on Friday"},
	}
	if err := deps.Meetings.ReplaceTurns(start, speakers, turns, 1); err != nil {
		t.Fatal(err)
	}

	out := payload(t, call(t, srv, "get_meeting",
		fmt.Sprintf(`{"id":%q}`, start.Format(time.RFC3339Nano))))
	if out["summary"] != "decided to ship" {
		t.Fatalf("got summary %v", out["summary"])
	}
	names, _ := out["speakers"].([]any)
	if len(names) != 2 {
		t.Fatalf("got %d speakers, want 2: %v", len(names), names)
	}
	got, _ := out["turns"].([]any)
	if len(got) != 2 {
		t.Fatalf("got %d turns, want 2: %v", len(got), got)
	}
	first, _ := got[0].(map[string]any)
	if first["speaker"] != "You" {
		t.Fatalf("the microphone's own turn is attributed to %v, want You", first["speaker"])
	}
}

func TestSearchTranscriptsFindsATurn(t *testing.T) {
	srv, deps := testServer(t, false)
	start := time.Date(2026, 9, 21, 15, 0, 0, 0, time.Local)
	if err := deps.Meetings.Append(history.Meeting{Start: start, RecordingSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Meetings.ReplaceTurns(start,
		[]history.MeetingSpeaker{{LocalID: history.YouSpeaker, TalkSecs: 5, TurnCount: 1}},
		[]history.Turn{{Channel: history.ChannelMic, StartSecs: 0, EndSecs: 5, LocalID: history.YouSpeaker, Text: "send the invoice on Friday"}},
		1); err != nil {
		t.Fatal(err)
	}

	out := payload(t, call(t, srv, "search_transcripts", `{"query":"invoi"}`))
	hits, _ := out["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("got %d hits for a prefix search, want 1: %v", len(hits), hits)
	}
}

func TestListTasksFiltersByStatus(t *testing.T) {
	srv, deps := testServer(t, false)
	for _, st := range []task.Status{task.StatusTodo, task.StatusDone, task.StatusDone} {
		if err := deps.Tasks.Append(task.Task{
			ID: task.NewID(), Text: "something", Status: st, Created: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}

	out := payload(t, call(t, srv, "list_tasks", `{"status":"done"}`))
	tasks, _ := out["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("got %d done tasks, want 2: %v", len(tasks), tasks)
	}
}

// A tool that cannot do its job says so in the RESULT: the client is meant
// to read it and try something else, which a transport error denies it.
func TestAnUnknownIdIsAToolErrorNotATransportError(t *testing.T) {
	srv, _ := testServer(t, false)

	res := call(t, srv, "get_task", `{"id":"nope"}`)
	if res["isError"] != true {
		t.Fatalf("an unknown task id did not come back as a tool error: %v", res)
	}
}

func TestCreateTaskIsRefusedWhenWritesAreOff(t *testing.T) {
	srv, deps := testServer(t, false)

	res := call(t, srv, "create_task", `{"text":"do the thing"}`)
	if res["isError"] != true {
		t.Fatalf("create_task was allowed with writes off: %v", res)
	}
	all, err := deps.Tasks.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("a task was written anyway: %v", all)
	}
}

func TestCreateTaskWritesWhenAllowed(t *testing.T) {
	srv, deps := testServer(t, true)

	out := payload(t, call(t, srv, "create_task", `{"text":"do the thing","entity":"voxlog"}`))
	if out["text"] != "do the thing" {
		t.Fatalf("got %v back", out["text"])
	}
	all, err := deps.Tasks.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Text != "do the thing" {
		t.Fatalf("the task did not land in the store: %v", all)
	}
	// A task added from outside must be distinguishable from one the
	// classifier pulled out of a transcript.
	if all[0].SourceKind != "mcp" {
		t.Fatalf("got source kind %q, want mcp", all[0].SourceKind)
	}
}

func TestUpdateTaskStatusMovesTheTask(t *testing.T) {
	srv, deps := testServer(t, true)
	id := task.NewID()
	if err := deps.Tasks.Append(task.Task{ID: id, Text: "ship it", Status: task.StatusTodo, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}

	out := payload(t, call(t, srv, "update_task_status", fmt.Sprintf(`{"id":%q,"status":"done"}`, id)))
	if out["status"] != "done" {
		t.Fatalf("got status %v, want done", out["status"])
	}
	stored, err := deps.Tasks.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != task.StatusDone {
		t.Fatalf("the store still says %q", stored.Status)
	}
}

func TestAnInventedStatusIsRefused(t *testing.T) {
	srv, _ := testServer(t, false)

	res := call(t, srv, "list_tasks", `{"status":"maybe"}`)
	if res["isError"] != true {
		t.Fatalf("an invented status was accepted: %v", res)
	}
}

// Lists carry a preview, not the whole transcript: the meetings database is
// opened with one connection, and the client reading this has a context
// window.
func TestLongNotesAreTruncatedInAList(t *testing.T) {
	srv, deps := testServer(t, false)
	long := bytes.Repeat([]byte("слово "), 400)
	if err := deps.Notes.Append(history.Entry{
		Timestamp: time.Now(), RecordingSeconds: 60, Text: string(long),
	}); err != nil {
		t.Fatal(err)
	}

	out := payload(t, call(t, srv, "list_notes", "{}"))
	notes, _ := out["notes"].([]any)
	if len(notes) != 1 {
		t.Fatalf("got %d notes", len(notes))
	}
	first, _ := notes[0].(map[string]any)
	if first["truncated"] != true {
		t.Fatalf("a long note was not marked truncated: %v", first)
	}
	if len([]rune(first["text"].(string))) > previewRunes+1 {
		t.Fatalf("the preview is %d runes long", len([]rune(first["text"].(string))))
	}
}

func TestSearchTranscriptsReportsTheProject(t *testing.T) {
	srv, deps := testServer(t, false)
	start := time.Date(2026, 9, 22, 11, 0, 0, 0, time.Local)
	if err := deps.Meetings.Append(history.Meeting{Start: start, RecordingSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Meetings.SetEntity(start, "Northwind"); err != nil {
		t.Fatal(err)
	}
	if err := deps.Meetings.ReplaceTurns(start,
		[]history.MeetingSpeaker{{LocalID: history.YouSpeaker, TalkSecs: 5, TurnCount: 1}},
		[]history.Turn{{Channel: history.ChannelMic, StartSecs: 0, EndSecs: 5, LocalID: history.YouSpeaker, Text: "send the invoice on Friday"}},
		1); err != nil {
		t.Fatal(err)
	}

	out := payload(t, call(t, srv, "search_transcripts", `{"query":"invoice"}`))
	hits, _ := out["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %v", len(hits), hits)
	}
	hit, _ := hits[0].(map[string]any)
	if hit["entity"] != "Northwind" {
		t.Fatalf("hit reports entity %v, want Northwind", hit["entity"])
	}
}
