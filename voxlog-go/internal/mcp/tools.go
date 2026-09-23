package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"voxlog-go/internal/history"
	"voxlog-go/internal/task"
)

// Deps is everything the server may read. Nothing is opened here: these are
// the stores the app already holds open, so an enabled server costs a
// listener and nothing else.
type Deps struct {
	Notes    *history.Store
	Meetings *history.MeetingStore
	Tasks    *task.Store
}

// Limits on every list. Not politeness: the meetings database is opened with
// SetMaxOpenConns(1) (see internal/history/db.go), so an unbounded dump
// competes with a live meeting's own writes -- and the client reading it has
// a context window.
const (
	defaultLimit = 20
	maxLimit     = 100
	previewRunes = 400
	defaultTurns = 300
)

// tool is one entry in the advertised list. inputSchema is hand-written JSON
// rather than reflected off a struct: there are a dozen of them, they change
// when the store changes, and a schema you can read in the file that serves
// it is worth more here than one generated somewhere else.
type tool struct {
	Name        string
	Description string
	InputSchema string
	Write       bool
	Run         func(d Deps, args json.RawMessage) (any, error)
}

// tools is the whole surface, in the order it is advertised.
var tools = []tool{
	{
		Name:        "list_notes",
		Description: "List dictated notes, newest first. A note is one transcript: something the user dictated, or something Voxlog heard them say to themselves.",
		InputSchema: `{"type":"object","properties":{
			"day":{"type":"string","description":"A single day, YYYY-MM-DD, in local time."},
			"since":{"type":"string","description":"RFC3339 timestamp; only notes at or after it."},
			"until":{"type":"string","description":"RFC3339 timestamp; only notes before it."},
			"limit":{"type":"integer","description":"How many to return. Default 20, maximum 100."}}}`,
		Run: listNotes,
	},
	{
		Name:        "get_note",
		Description: "One dictated note in full, by the id list_notes reports.",
		InputSchema: `{"type":"object","properties":{
			"id":{"type":"string","description":"The note's id: its timestamp, RFC3339 with nanoseconds."}},
			"required":["id"]}`,
		Run: getNote,
	},
	{
		Name:        "list_meetings",
		Description: "List recorded meetings, newest first, with their summary and who spoke in them.",
		InputSchema: `{"type":"object","properties":{
			"since":{"type":"string","description":"RFC3339 timestamp; only meetings starting at or after it."},
			"until":{"type":"string","description":"RFC3339 timestamp; only meetings starting before it."},
			"entity":{"type":"string","description":"Only meetings filed under this project."},
			"limit":{"type":"integer","description":"How many to return. Default 20, maximum 100."}}}`,
		Run: listMeetings,
	},
	{
		Name:        "get_meeting",
		Description: "One meeting in full: its summary, its speakers, and its transcript as per-speaker turns.",
		InputSchema: `{"type":"object","properties":{
			"id":{"type":"string","description":"The meeting's id: its start time, RFC3339 with nanoseconds."},
			"include_turns":{"type":"boolean","description":"Include the per-speaker transcript. Default true."},
			"turn_offset":{"type":"integer","description":"Skip this many turns, for reading a long meeting in parts."},
			"max_turns":{"type":"integer","description":"How many turns to return. Default 300."}},
			"required":["id"]}`,
		Run: getMeeting,
	},
	{
		Name:        "search_transcripts",
		Description: "Search every meeting transcript at once. Words are matched by prefix, so a half-remembered word still finds the conversation.",
		InputSchema: `{"type":"object","properties":{
			"query":{"type":"string","description":"What to look for."},
			"limit":{"type":"integer","description":"How many hits to return. Default 20, maximum 100."}},
			"required":["query"]}`,
		Run: searchTranscripts,
	},
	{
		Name:        "list_people",
		Description: "Who the user actually meets with: every named voice, how many meetings it appeared in, and how much it talked.",
		InputSchema: `{"type":"object","properties":{}}`,
		Run:         listPeople,
	},
	{
		Name:        "list_tasks",
		Description: "List tasks, newest first.",
		InputSchema: `{"type":"object","properties":{
			"status":{"type":"string","enum":["todo","in_progress","blocked","done"],"description":"Only tasks in this state."},
			"entity":{"type":"string","description":"Only tasks filed under this project."},
			"limit":{"type":"integer","description":"How many to return. Default 20, maximum 100."}}}`,
		Run: listTasks,
	},
	{
		Name:        "get_task",
		Description: "One task in full, including its notes and what it came from.",
		InputSchema: `{"type":"object","properties":{
			"id":{"type":"string","description":"The task's id."}},
			"required":["id"]}`,
		Run: getTask,
	},
	{
		Name:        "list_projects",
		Description: "The projects tasks can be filed under.",
		InputSchema: `{"type":"object","properties":{}}`,
		Run:         listProjects,
	},
	{
		Name:        "create_task",
		Description: "Add a task.",
		InputSchema: `{"type":"object","properties":{
			"text":{"type":"string","description":"What has to be done."},
			"entity":{"type":"string","description":"The project it belongs to."},
			"notes":{"type":"string","description":"Anything else worth keeping with it."},
			"status":{"type":"string","enum":["todo","in_progress","blocked","done"],"description":"Defaults to todo."}},
			"required":["text"]}`,
		Write: true,
		Run:   createTask,
	},
	{
		Name:        "update_task_status",
		Description: "Move a task to another state.",
		InputSchema: `{"type":"object","properties":{
			"id":{"type":"string","description":"The task's id."},
			"status":{"type":"string","enum":["todo","in_progress","blocked","done"],"description":"The state to move it to."}},
			"required":["id","status"]}`,
		Write: true,
		Run:   updateTaskStatus,
	},
}

// The JSON rows. Field names and shapes follow internal/ui's own meeting
// bindings, so the two outward-facing surfaces describe the same data the
// same way instead of drifting apart.
//
// No audio_path or system_audio_path anywhere: what goes out of here is what
// was said, not where the recording of it sits on this machine's disk.

type noteJSON struct {
	ID          string  `json:"id"`
	At          string  `json:"at"`
	Seconds     float64 `json:"seconds"`
	Text        string  `json:"text"`
	Truncated   bool    `json:"truncated,omitempty"`
	AutoStarted bool    `json:"auto_started,omitempty"`
}

type meetingJSON struct {
	ID          string     `json:"id"`
	Start       string     `json:"start"`
	Seconds     float64    `json:"seconds"`
	Summary     string     `json:"summary,omitempty"`
	Entity      string     `json:"entity,omitempty"`
	Speakers    []string   `json:"speakers,omitempty"`
	Transcribed bool       `json:"transcribed"`
	Text        string     `json:"text,omitempty"`
	Truncated   bool       `json:"truncated,omitempty"`
	AutoStarted bool       `json:"auto_started,omitempty"`
	Turns       []turnJSON `json:"turns,omitempty"`
	TurnsTotal  int        `json:"turns_total,omitempty"`
	TurnsOffset int        `json:"turns_offset,omitempty"`
}

type turnJSON struct {
	Speaker   string  `json:"speaker"`
	StartSecs float64 `json:"start_secs"`
	EndSecs   float64 `json:"end_secs"`
	Text      string  `json:"text"`
}

type hitJSON struct {
	MeetingID string  `json:"meeting_id"`
	Start     string  `json:"start"`
	Speaker   string  `json:"speaker"`
	StartSecs float64 `json:"start_secs"`
	Text      string  `json:"text"`
	Snippet   string  `json:"snippet"`
	// Entity, like meetingJSON's and taskJSON's, is the project this was
	// said in. Without it a search result could not be placed: every other
	// tool here can be filtered by project, and only this one could not say
	// which one a hit belongs to.
	Entity string `json:"entity,omitempty"`
}

type personJSON struct {
	Name      string  `json:"name"`
	Meetings  int     `json:"meetings"`
	TalkSecs  float64 `json:"talk_secs"`
	TurnCount int     `json:"turn_count"`
	LastSeen  string  `json:"last_seen"`
}

type taskJSON struct {
	ID         string `json:"id"`
	Text       string `json:"text"`
	Entity     string `json:"entity,omitempty"`
	Status     string `json:"status"`
	Notes      string `json:"notes,omitempty"`
	Created    string `json:"created"`
	SourceKind string `json:"source_kind,omitempty"`
	SourceKey  string `json:"source_key,omitempty"`
}

// ---------------------------------------------------------------- arguments

type listArgs struct {
	Day    string `json:"day"`
	Since  string `json:"since"`
	Until  string `json:"until"`
	Entity string `json:"entity"`
	Status string `json:"status"`
	Query  string `json:"query"`
	Limit  int    `json:"limit"`
}

type idArgs struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	IncludeTurns *bool  `json:"include_turns"`
	TurnOffset   int    `json:"turn_offset"`
	MaxTurns     int    `json:"max_turns"`
}

type createArgs struct {
	Text   string `json:"text"`
	Entity string `json:"entity"`
	Notes  string `json:"notes"`
	Status string `json:"status"`
}

func decode(args json.RawMessage, into any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, into); err != nil {
		return fmt.Errorf("could not read the arguments: %w", err)
	}
	return nil
}

func capLimit(n int) int {
	if n <= 0 {
		return defaultLimit
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}

// window turns the since/until pair into a test. An unparseable timestamp is
// an error rather than an ignored filter: silently returning everything when
// the caller asked for a week is worse than saying the week was unreadable.
func window(since, until string) (func(time.Time) bool, error) {
	var from, to time.Time
	var err error
	if since != "" {
		if from, err = time.Parse(time.RFC3339, since); err != nil {
			return nil, fmt.Errorf("since is not an RFC3339 timestamp: %w", err)
		}
	}
	if until != "" {
		if to, err = time.Parse(time.RFC3339, until); err != nil {
			return nil, fmt.Errorf("until is not an RFC3339 timestamp: %w", err)
		}
	}
	return func(at time.Time) bool {
		if !from.IsZero() && at.Before(from) {
			return false
		}
		if !to.IsZero() && !at.Before(to) {
			return false
		}
		return true
	}, nil
}

// preview shortens a transcript for a list. Runes, not bytes: cutting a
// Ukrainian transcript at a byte offset lands in the middle of a character.
func preview(text string) (string, bool) {
	r := []rune(text)
	if len(r) <= previewRunes {
		return text, false
	}
	return strings.TrimSpace(string(r[:previewRunes])) + "…", true
}

func noteID(at time.Time) string { return at.Format(time.RFC3339Nano) }

// --------------------------------------------------------------------- notes

func listNotes(d Deps, args json.RawMessage) (any, error) {
	var a listArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}

	var entries []history.Entry
	var err error
	if a.Day != "" {
		day, parseErr := time.ParseInLocation("2006-01-02", a.Day, time.Local)
		if parseErr != nil {
			return nil, fmt.Errorf("day is not a YYYY-MM-DD date: %w", parseErr)
		}
		entries, err = d.Notes.EntriesForDay(day)
	} else {
		entries, err = d.Notes.AllEntries()
	}
	if err != nil {
		return nil, err
	}

	within, err := window(a.Since, a.Until)
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp.After(entries[j].Timestamp) })

	limit := capLimit(a.Limit)
	out := make([]noteJSON, 0, limit)
	for _, e := range entries {
		if !within(e.Timestamp) {
			continue
		}
		text, cut := preview(e.Text)
		out = append(out, noteJSON{
			ID:          noteID(e.Timestamp),
			At:          e.Timestamp.Format(time.RFC3339),
			Seconds:     e.RecordingSeconds,
			Text:        text,
			Truncated:   cut,
			AutoStarted: e.AutoStarted,
		})
		if len(out) == limit {
			break
		}
	}
	return map[string]any{"notes": out}, nil
}

func getNote(d Deps, args json.RawMessage) (any, error) {
	var a idArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	at, err := time.Parse(time.RFC3339Nano, a.ID)
	if err != nil {
		return nil, fmt.Errorf("id is not a timestamp: %w", err)
	}
	entries, err := d.Notes.EntriesForDay(at)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Timestamp.Equal(at) {
			return noteJSON{
				ID:          noteID(e.Timestamp),
				At:          e.Timestamp.Format(time.RFC3339),
				Seconds:     e.RecordingSeconds,
				Text:        e.Text,
				AutoStarted: e.AutoStarted,
			}, nil
		}
	}
	return nil, fmt.Errorf("no note at %s", a.ID)
}

// ------------------------------------------------------------------ meetings

func speakerNames(speakers []history.MeetingSpeaker) []string {
	var names []string
	for _, sp := range speakers {
		switch {
		case sp.Name != "":
			names = append(names, sp.Name)
		case sp.LocalID == history.YouSpeaker:
			names = append(names, "You")
		default:
			names = append(names, fmt.Sprintf("Speaker %d", sp.LocalID))
		}
	}
	return names
}

func listMeetings(d Deps, args json.RawMessage) (any, error) {
	var a listArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	within, err := window(a.Since, a.Until)
	if err != nil {
		return nil, err
	}
	all, err := d.Meetings.All()
	if err != nil {
		return nil, err
	}
	speakers, err := d.Meetings.SpeakersByMeeting()
	if err != nil {
		// Names are a nicety; a meeting list without them is still a meeting
		// list, and failing the whole call over it would be worse.
		speakers = nil
	}

	limit := capLimit(a.Limit)
	out := make([]meetingJSON, 0, limit)
	for _, m := range all {
		if !within(m.Start) {
			continue
		}
		if a.Entity != "" && !strings.EqualFold(a.Entity, m.Entity) {
			continue
		}
		text, cut := preview(m.Text)
		out = append(out, meetingJSON{
			ID:          noteID(m.Start),
			Start:       m.Start.Format(time.RFC3339),
			Seconds:     m.RecordingSeconds,
			Summary:     m.Summary,
			Entity:      m.Entity,
			Speakers:    speakerNames(speakers[m.Start.UnixNano()]),
			Transcribed: m.Text != "",
			Text:        text,
			Truncated:   cut,
			AutoStarted: m.AutoStarted,
		})
		if len(out) == limit {
			break
		}
	}
	return map[string]any{"meetings": out}, nil
}

func getMeeting(d Deps, args json.RawMessage) (any, error) {
	var a idArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	start, err := time.Parse(time.RFC3339Nano, a.ID)
	if err != nil {
		return nil, fmt.Errorf("id is not a timestamp: %w", err)
	}
	m, err := d.Meetings.Get(start)
	if err != nil {
		return nil, err
	}
	speakers, err := d.Meetings.Speakers(start)
	if err != nil {
		return nil, err
	}

	out := meetingJSON{
		ID:          noteID(m.Start),
		Start:       m.Start.Format(time.RFC3339),
		Seconds:     m.RecordingSeconds,
		Summary:     m.Summary,
		Entity:      m.Entity,
		Speakers:    speakerNames(speakers),
		Transcribed: m.Text != "",
		Text:        m.Text,
		AutoStarted: m.AutoStarted,
	}

	if a.IncludeTurns != nil && !*a.IncludeTurns {
		return out, nil
	}

	turns, err := d.Meetings.Turns(start)
	if err != nil {
		return nil, err
	}
	out.TurnsTotal = len(turns)
	out.TurnsOffset = a.TurnOffset
	if a.TurnOffset > 0 {
		if a.TurnOffset >= len(turns) {
			turns = nil
		} else {
			turns = turns[a.TurnOffset:]
		}
	}
	max := a.MaxTurns
	if max <= 0 {
		max = defaultTurns
	}
	if len(turns) > max {
		turns = turns[:max]
	}
	names := speakerNames(speakers)
	byLocal := map[int]string{}
	for i, sp := range speakers {
		byLocal[sp.LocalID] = names[i]
	}
	for _, t := range turns {
		name := t.Name
		if name == "" {
			if known, ok := byLocal[t.LocalID]; ok {
				name = known
			} else if t.LocalID == history.YouSpeaker {
				name = "You"
			} else {
				name = fmt.Sprintf("Speaker %d", t.LocalID)
			}
		}
		out.Turns = append(out.Turns, turnJSON{
			Speaker:   name,
			StartSecs: t.StartSecs,
			EndSecs:   t.EndSecs,
			Text:      t.Text,
		})
	}
	return out, nil
}

func searchTranscripts(d Deps, args json.RawMessage) (any, error) {
	var a listArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Query) == "" {
		return nil, fmt.Errorf("query is required")
	}
	hits, err := d.Meetings.SearchTurns(a.Query)
	if err != nil {
		return nil, err
	}
	limit := capLimit(a.Limit)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]hitJSON, 0, len(hits))
	for _, h := range hits {
		name := h.Name
		if name == "" {
			if h.LocalID == history.YouSpeaker {
				name = "You"
			} else {
				name = fmt.Sprintf("Speaker %d", h.LocalID)
			}
		}
		out = append(out, hitJSON{
			MeetingID: noteID(h.Start),
			Start:     h.Start.Format(time.RFC3339),
			Speaker:   name,
			StartSecs: h.StartSecs,
			Text:      h.Text,
			Snippet:   h.Snippet,
			Entity:    h.Entity,
		})
	}
	return map[string]any{"hits": out}, nil
}

func listPeople(d Deps, _ json.RawMessage) (any, error) {
	people, err := d.Meetings.People()
	if err != nil {
		return nil, err
	}
	out := make([]personJSON, 0, len(people))
	for _, p := range people {
		out = append(out, personJSON{
			Name:      p.Name,
			Meetings:  p.Meetings,
			TalkSecs:  p.TalkSecs,
			TurnCount: p.TurnCount,
			LastSeen:  p.LastSeen.Format(time.RFC3339),
		})
	}
	return map[string]any{"people": out}, nil
}

// --------------------------------------------------------------------- tasks

func toTaskJSON(t task.Task) taskJSON {
	return taskJSON{
		ID:         t.ID,
		Text:       t.Text,
		Entity:     t.Entity,
		Status:     string(t.Status),
		Notes:      t.Notes,
		Created:    t.Created.Format(time.RFC3339),
		SourceKind: t.SourceKind,
		SourceKey:  t.SourceKey,
	}
}

func listTasks(d Deps, args json.RawMessage) (any, error) {
	var a listArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Status != "" {
		if _, err := parseStatus(a.Status); err != nil {
			return nil, err
		}
	}
	all, err := d.Tasks.All()
	if err != nil {
		return nil, err
	}
	limit := capLimit(a.Limit)
	out := make([]taskJSON, 0, limit)
	for _, t := range all {
		if a.Status != "" && string(t.Status) != a.Status {
			continue
		}
		if a.Entity != "" && !strings.EqualFold(a.Entity, t.Entity) {
			continue
		}
		out = append(out, toTaskJSON(t))
		if len(out) == limit {
			break
		}
	}
	return map[string]any{"tasks": out}, nil
}

func getTask(d Deps, args json.RawMessage) (any, error) {
	var a idArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	t, err := d.Tasks.Get(a.ID)
	if err != nil {
		return nil, err
	}
	return toTaskJSON(t), nil
}

func listProjects(d Deps, _ json.RawMessage) (any, error) {
	names, err := d.Tasks.EntityNames()
	if err != nil {
		return nil, err
	}
	if names == nil {
		names = []string{}
	}
	return map[string]any{"projects": names}, nil
}

func parseStatus(s string) (task.Status, error) {
	switch task.Status(s) {
	case task.StatusTodo, task.StatusInProgress, task.StatusBlocked, task.StatusDone:
		return task.Status(s), nil
	}
	return "", fmt.Errorf("%q is not a task status: use todo, in_progress, blocked or done", s)
}

func createTask(d Deps, args json.RawMessage) (any, error) {
	var a createArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Text) == "" {
		return nil, fmt.Errorf("text is required")
	}
	status := task.StatusTodo
	if a.Status != "" {
		parsed, err := parseStatus(a.Status)
		if err != nil {
			return nil, err
		}
		status = parsed
	}
	t := task.Task{
		ID: task.NewID(),
		// SourceKind says where a task came from, and "mcp" is a source the
		// classifier never produces -- so a task added from outside is
		// distinguishable from one Voxlog heard in a transcript.
		SourceKind: "mcp",
		Text:       strings.TrimSpace(a.Text),
		Entity:     a.Entity,
		Status:     status,
		Notes:      a.Notes,
		Created:    time.Now(),
	}
	if err := d.Tasks.Append(t); err != nil {
		return nil, err
	}
	return toTaskJSON(t), nil
}

func updateTaskStatus(d Deps, args json.RawMessage) (any, error) {
	var a idArgs
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	status, err := parseStatus(a.Status)
	if err != nil {
		return nil, err
	}
	if err := d.Tasks.Update(a.ID, func(t *task.Task) { t.Status = status }); err != nil {
		return nil, err
	}
	t, err := d.Tasks.Get(a.ID)
	if err != nil {
		return nil, err
	}
	return toTaskJSON(t), nil
}
