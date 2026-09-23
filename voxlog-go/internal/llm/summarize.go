package llm

import "strings"

// SummaryOptions is what the user has said about summaries in
// Settings > LLM model. A struct rather than more positional arguments: the
// settings pane will keep growing knobs, and every one of them would
// otherwise change this function's signature and every call to it.
type SummaryOptions struct {
	// Length is settings.SummaryBrief/Normal/Detailed. An unknown or empty
	// value is treated as normal -- a hand-edited settings file must not turn
	// summarizing off by typo.
	Length string
	// Extra is the user's own instructions, appended to the built-in prompt.
	// Never a replacement: see buildSummaryPrompt.
	Extra string
}

// Length values, mirroring settings.Summary* without importing the settings
// package -- llm is below it, and the two strings are a wire format either
// way (they are in settings.json).
const (
	LengthBrief    = "brief"
	LengthNormal   = "normal"
	LengthDetailed = "detailed"
)

// Summarize asks the model for a short plain-text summary of a meeting
// transcript. Unlike Classify, the reply isn't JSON -- there's nothing to
// parse, just a couple of sentences to trim and hand back.
func (c *Cache) Summarize(modelDir, text string, entities []string, opts SummaryOptions) (string, error) {
	base, err := c.baseURL(modelDir)
	if err != nil {
		return "", err
	}
	content, err := chatCompletion(base, buildSummaryPrompt(text, entities, opts))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(content), nil
}

// summaryShape is the one sentence that says how much to write, per length.
func summaryShape(length string) string {
	switch length {
	case LengthBrief:
		return `Summarize this meeting transcript in one or two plain sentences: what it
was about, and the single most important thing that came out of it.`
	case LengthDetailed:
		return `Summarize this meeting transcript in five or six plain sentences: what was
discussed, every decision made, and anything left open. Name who is doing
what where the transcript says so.`
	default:
		return `Summarize this meeting transcript in 2-3 plain sentences: what was
discussed and any decisions made.`
	}
}

func buildSummaryPrompt(text string, entities []string, opts SummaryOptions) string {
	if len(text) > maxTranscriptChars {
		text = text[:maxTranscriptChars]
	}
	entityHint := ""
	if len(entities) > 0 {
		// Not a constraint, just a nudge: if the meeting is plainly about one
		// of these, use its exact name rather than describing it vaguely.
		entityHint = "Known project/client names, for reference if relevant: " +
			strings.Join(entities, ", ") + ".\n\n"
	}
	// The user's instructions come after the built-in ones and are labelled
	// as theirs, so the model reads them as additions rather than as a
	// correction to the format demanded above.
	extra := ""
	if s := strings.TrimSpace(opts.Extra); s != "" {
		extra = "Additional instructions from the user, to follow as long as they do not\n" +
			"contradict the format above:\n" + s + "\n\n"
	}
	return summaryShape(opts.Length) + ` No preamble ("This meeting..."), no
markdown, no bullet points -- just the sentences.

` + extra + entityHint + `Transcript:
"""
` + text + `
"""`
}
