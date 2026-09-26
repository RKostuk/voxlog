package llm

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeEntityEnforcesClosedList(t *testing.T) {
	allowed := []string{"Acme", "Beta Client"}
	cases := []struct{ entity, want string }{
		{"Acme", "Acme"},
		{"acme", "Acme"}, // case-insensitive match, canonical casing wins
		{"Beta Client", "Beta Client"},
		{"Gamma", "Unfiltered"}, // not on the list -- model tried to invent one
		{"", "Unfiltered"},      // model left it blank
		{"Unfiltered", "Unfiltered"},
	}
	for _, c := range cases {
		if got := normalizeEntity(c.entity, allowed); got != c.want {
			t.Errorf("normalizeEntity(%q, %v) = %q, want %q", c.entity, allowed, got, c.want)
		}
	}
}

func TestNormalizeEntityEmptyListAlwaysUnfiltered(t *testing.T) {
	if got := normalizeEntity("Anything", nil); got != UnfilteredEntity {
		t.Errorf("got %q, want %q when no projects are configured", got, UnfilteredEntity)
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{`{"is_task":true}`, `{"is_task":true}`, false},
		{"Sure, here it is:\n```json\n{\"is_task\": false, \"text\": \"\"}\n```", `{"is_task": false, "text": ""}`, false},
		{`{"text":"has a } brace inside a string"}`, `{"text":"has a } brace inside a string"}`, false},
		{"no braces here", "", true},
		{"{unbalanced", "", true},
	}
	for _, c := range cases {
		got, err := extractJSONObject(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("extractJSONObject(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("extractJSONObject(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("extractJSONObject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The user's rules ride along with the built-in prompt, never in place of it:
// the JSON shape is read back by Classify, so it has to survive whatever the
// rules say.
func TestBuildPromptCarriesTheUsersRules(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	plain := buildPrompt("buy milk", nil, nil, "", now)
	if strings.Contains(plain, "Additional rules from the user") {
		t.Error("an empty rule set still added a rules block")
	}

	rule := `"створи задачу" always means a task`
	with := buildPrompt("buy milk", nil, nil, "  "+rule+"\n", now)
	i := strings.Index(with, rule)
	if i == -1 {
		t.Fatalf("prompt does not carry the rule:\n%s", with)
	}
	if j := strings.Index(with, "Transcript:"); j < i {
		t.Error("the rules come after the transcript; they belong with the instructions")
	}
	if !strings.Contains(with, `"tasks"`) {
		t.Error("the JSON format went missing when rules were added")
	}

	long := strings.Repeat("x", maxTaskRulesChars+500)
	if got := buildPrompt("t", nil, nil, long, now); strings.Contains(got, long) {
		t.Error("an overlong rule set was not trimmed")
	}
}

// A task dictated in Ukrainian came back as an English sentence: the prompt
// is in English and never said otherwise.
func TestBuildPromptKeepsTheTranscriptsLanguage(t *testing.T) {
	p := buildPrompt("нагадай подзвонити бухгалтеру", nil, nil, "", time.Now())
	if !strings.Contains(p, "transcript's own language") {
		t.Error("the prompt does not ask for the task text in the transcript's language")
	}
}

// A trigger the user names is the only way a task is made: dictating a
// message full of things to do must not turn into tasks and go unpasted.
func TestBuildPromptTreatsTheUsersTriggerAsTheOnlyWayIn(t *testing.T) {
	p := buildPrompt("треба оновити договір", nil, nil, `Задача -- тільки коли я кажу "заведи задачу"`, time.Now())
	if !strings.Contains(p, "ONLY way a task is made") {
		t.Error("the user's trigger is not the only way a task is made")
	}
}

// "By Friday" cannot be resolved from a bare date: the model has to be told
// what day today is.
func TestBuildPromptNamesTheWeekday(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) // a Saturday
	if p := buildPrompt("t", nil, nil, "", now); !strings.Contains(p, "Saturday, 2026-09-26") {
		t.Error("the prompt does not say which weekday it is")
	}
	if p := buildPrompt("t", nil, nil, "", now); !strings.Contains(p, "Friday 2026-10-02") {
		t.Error("the prompt does not give the coming Friday's date")
	}
}

// One dictation can hold several tasks, or none; each is kept on its own,
// with its description, and one without text is dropped.
func TestParseClassificationSeveralTasks(t *testing.T) {
	reply := "```json\n" + `{"tasks": [
		{"text": "Подзвонити бухгалтеру", "notes": "про звіт за вересень", "entity": "acme", "status": "todo", "reminder": "2026-09-28T10:00:00+03:00"},
		{"text": "Купити папір", "notes": "", "entity": "Other", "status": "weird", "reminder": null},
		{"text": "  ", "entity": "Acme"}
	]}` + "\n```"
	got, err := parseClassification(reply, []string{"Acme"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d tasks, want 2: %+v", len(got), got)
	}
	if got[0].Entity != "Acme" || got[0].Notes != "про звіт за вересень" || got[0].Reminder == nil {
		t.Errorf("first task = %+v", got[0])
	}
	if got[1].Entity != UnfilteredEntity || got[1].Status != "todo" || got[1].Reminder != nil {
		t.Errorf("second task = %+v", got[1])
	}

	none, err := parseClassification(`{"tasks": []}`, nil)
	if err != nil || len(none) != 0 {
		t.Errorf("no tasks: got %+v, %v", none, err)
	}
	if _, err := parseClassification("Sure! You should call the accountant.", nil); err == nil {
		t.Error("prose with no JSON was accepted")
	}
}
