package llm

import "strings"

// Summarize asks the model for a short plain-text summary of a meeting
// transcript. Unlike Classify, the reply isn't JSON -- there's nothing to
// parse, just a couple of sentences to trim and hand back.
func (c *Cache) Summarize(modelDir, text string, entities []string) (string, error) {
	base, err := c.baseURL(modelDir)
	if err != nil {
		return "", err
	}
	content, err := chatCompletion(base, buildSummaryPrompt(text, entities))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(content), nil
}

func buildSummaryPrompt(text string, entities []string) string {
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
	return `Summarize this meeting transcript in 2-3 plain sentences: what was
discussed and any decisions made. No preamble ("This meeting..."), no
markdown, no bullet points -- just the sentences.

` + entityHint + `Transcript:
"""
` + text + `
"""`
}
