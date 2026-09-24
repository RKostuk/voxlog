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

func TestParseSummaryTakesTheProjectOffTheLastLine(t *testing.T) {
	allowed := []string{"Northwind", "Contoso"}
	_, summary, entity := parseSummary("They agreed on the price.\n\nPROJECT: northwind\n", allowed)
	if summary != "They agreed on the price." {
		t.Errorf("summary = %q, want the sentence with no project line", summary)
	}
	// Canonical casing comes from the user's list, not from the model.
	if entity != "Northwind" {
		t.Errorf("entity = %q, want Northwind", entity)
	}
}

func TestParseSummaryDropsAProjectNobodyConfigured(t *testing.T) {
	for _, reply := range []string{
		"They agreed on the price.\nPROJECT: none",
		"They agreed on the price.\nPROJECT: Something The Model Made Up",
		`They agreed on the price.
Project: "Northwind Traders".`,
	} {
		_, summary, entity := parseSummary(reply, []string{"Northwind", "Contoso"})
		if entity != "" {
			t.Errorf("reply %q yielded entity %q, want empty", reply, entity)
		}
		// The label line is an echo of an instruction, not a sentence about
		// the meeting, so it goes whether or not its value was usable.
		if summary != "They agreed on the price." {
			t.Errorf("reply %q left the project line in the summary: %q", reply, summary)
		}
	}
}

// A small model forgets the last line often enough that losing the summary
// over it would be the wrong trade.
func TestParseSummarySurvivesAMissingProjectLine(t *testing.T) {
	_, summary, entity := parseSummary("  They agreed on the price.  ", []string{"Northwind"})
	if summary != "They agreed on the price." {
		t.Errorf("summary = %q", summary)
	}
	if entity != "" {
		t.Errorf("entity = %q, want empty", entity)
	}
}

func TestTheProjectLineIsOnlyAskedForWhenThereAreProjects(t *testing.T) {
	with := buildSummaryPrompt("we talked", []string{"Northwind"}, SummaryOptions{})
	if !strings.Contains(with, "PROJECT:") {
		t.Error("the prompt does not ask for the project line")
	}
	without := buildSummaryPrompt("we talked", nil, SummaryOptions{})
	if strings.Contains(without, "PROJECT:") {
		t.Error("the prompt asks which project, with no projects to choose from")
	}
}

// The title leads the reply and comes off it the same way the project comes
// off the end -- and its absence is not an error either.
func TestParseSummaryTakesTheTitleOffTheFirstLine(t *testing.T) {
	title, summary, entity := parseSummary(
		"TITLE: CSV importer scope\nThey agreed on the price.\nPROJECT: Northwind",
		[]string{"Northwind"})
	if title != "CSV importer scope" {
		t.Errorf("title = %q", title)
	}
	if summary != "They agreed on the price." {
		t.Errorf("summary = %q, want the title line gone", summary)
	}
	if entity != "Northwind" {
		t.Errorf("entity = %q", entity)
	}
}

func TestParseSummarySurvivesAMissingTitleLine(t *testing.T) {
	title, summary, _ := parseSummary("They agreed on the price.", nil)
	if title != "" {
		t.Errorf("title = %q, want empty", title)
	}
	// An unlabelled first line is the summary's own opening sentence; taking
	// it for a heading would leave the summary starting mid-story.
	if summary != "They agreed on the price." {
		t.Errorf("summary = %q", summary)
	}
}

func TestParseSummaryCleansAndRefusesOversizedTitles(t *testing.T) {
	title, _, _ := parseSummary(`TITLE: "Pricing page copy."`+"\nThey agreed.", nil)
	if title != "Pricing page copy" {
		t.Errorf("title = %q, want the quotes and full stop gone", title)
	}
	long := "TITLE: " + strings.Repeat("word ", 30) + "\nThey agreed."
	if title, _, _ := parseSummary(long, nil); title != "" {
		t.Errorf("title = %q, want a sentence-length title refused", title)
	}
	// Runes, not bytes: a Ukrainian title is two bytes a letter.
	cyrillic := "TITLE: Обсяг імпортера CSV для першого релізу\nДомовились."
	if title, _, _ := parseSummary(cyrillic, nil); title != "Обсяг імпортера CSV для першого релізу" {
		t.Errorf("title = %q, want the Ukrainian title kept whole", title)
	}
}

func TestThePromptAsksForATitle(t *testing.T) {
	p := buildSummaryPrompt("we talked", nil, SummaryOptions{Length: LengthBrief})
	if !strings.Contains(p, "TITLE:") {
		t.Error("the prompt does not ask for a title")
	}
	if strings.Index(p, "TITLE:") > strings.Index(p, "Transcript:") {
		t.Error("the title instruction lands after the transcript")
	}
}
