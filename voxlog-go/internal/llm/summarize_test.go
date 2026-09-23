package llm

import (
	"strings"
	"testing"
)

func TestSummaryLengthChangesWhatIsAskedFor(t *testing.T) {
	brief := buildSummaryPrompt("we talked", nil, SummaryOptions{Length: LengthBrief})
	normal := buildSummaryPrompt("we talked", nil, SummaryOptions{Length: LengthNormal})
	detailed := buildSummaryPrompt("we talked", nil, SummaryOptions{Length: LengthDetailed})

	if brief == normal || normal == detailed || brief == detailed {
		t.Fatal("the three lengths ask for the same thing")
	}
	// Whatever the length, the reply's shape is what the app reads back.
	for name, p := range map[string]string{"brief": brief, "normal": normal, "detailed": detailed} {
		if !strings.Contains(p, "no bullet points") {
			t.Errorf("%s prompt dropped the format instructions", name)
		}
		if !strings.Contains(p, "we talked") {
			t.Errorf("%s prompt dropped the transcript", name)
		}
	}
}

// An empty or hand-mangled length must summarize normally rather than
// produce a prompt with no instruction about length in it at all.
func TestUnknownSummaryLengthFallsBackToNormal(t *testing.T) {
	want := buildSummaryPrompt("we talked", nil, SummaryOptions{Length: LengthNormal})
	for _, length := range []string{"", "medium", "BRIEF"} {
		if got := buildSummaryPrompt("we talked", nil, SummaryOptions{Length: length}); got != want {
			t.Errorf("length %q does not fall back to normal", length)
		}
	}
}

func TestExtraInstructionsAreAppendedNotSubstituted(t *testing.T) {
	p := buildSummaryPrompt("we talked", []string{"Northwind"}, SummaryOptions{
		Length: LengthNormal,
		Extra:  "  Answer in Ukrainian.  ",
	})
	if !strings.Contains(p, "Answer in Ukrainian.") {
		t.Error("the user's instructions are missing")
	}
	if strings.Contains(p, "  Answer in Ukrainian.  ") {
		t.Error("the user's instructions were not trimmed")
	}
	if !strings.Contains(p, "no bullet points") {
		t.Error("the built-in instructions were replaced rather than added to")
	}
	if !strings.Contains(p, "Northwind") {
		t.Error("the project hint was lost")
	}
	// The user's words come after the built-in prompt and before the
	// transcript: read as additions, not as a correction to the format.
	if strings.Index(p, "Answer in Ukrainian.") > strings.Index(p, "Transcript:") {
		t.Error("the user's instructions land after the transcript")
	}
}

func TestNoExtraInstructionsAddsNoBlock(t *testing.T) {
	p := buildSummaryPrompt("we talked", nil, SummaryOptions{Length: LengthNormal})
	if strings.Contains(p, "Additional instructions") {
		t.Error("an empty field still announces user instructions")
	}
}
