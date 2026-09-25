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
	if !strings.Contains(with, `"is_task"`) {
		t.Error("the JSON format went missing when rules were added")
	}

	long := strings.Repeat("x", maxTaskRulesChars+500)
	if got := buildPrompt("t", nil, nil, long, now); strings.Contains(got, long) {
		t.Error("an overlong rule set was not trimmed")
	}
}
