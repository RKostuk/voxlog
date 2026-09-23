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
// transcript, and for the project that meeting belongs to. The reply isn't
// JSON: it is the summary, then one trailing PROJECT: line, which is all
// parseSummary has to find.
//
// The project comes out of this pass rather than a classifier of its own
// because the model has just read the whole transcript to write the summary.
// A second pass would be a second minute of a 4B model's time for an answer
// already sitting in front of it.
//
// The returned entity is one of entities exactly, or empty. Empty is a real
// answer -- a meeting that is plainly not about any configured project has no
// project, and inventing "Unfiltered" for it (as task classification does)
// would file every stray call under one heading.
func (c *Cache) Summarize(modelDir, text string, entities []string, opts SummaryOptions) (summary, entity string, err error) {
	base, err := c.baseURL(modelDir)
	if err != nil {
		return "", "", err
	}
	content, err := chatCompletion(base, buildSummaryPrompt(text, entities, opts))
	if err != nil {
		return "", "", err
	}
	summary, entity = parseSummary(content, entities)
	return summary, entity, nil
}

// parseSummary splits the reply into the summary and the project. A reply
// with no PROJECT: line, or one naming something that is not on the list, is
// a summary with no project -- never an error: the summary is the part worth
// keeping, and a local model forgets the last line often enough that losing
// the whole reply over it would be the wrong trade.
func parseSummary(content string, entities []string) (summary, entity string) {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		rest, ok := cutPrefixFold(line, projectLabel)
		if !ok {
			break // the last thing said was summary, not a project
		}
		// Drop the label line whether or not its value matches anything: it
		// is an instruction's echo, not a sentence about the meeting.
		summary = strings.TrimSpace(strings.Join(lines[:i], "\n"))
		return summary, matchEntity(strings.Trim(strings.TrimSpace(rest), `"'.`), entities)
	}
	return strings.TrimSpace(content), ""
}

// projectLabel is the marker the prompt asks for. A label rather than JSON:
// mlx_lm has no schema-constrained decoding (see chatCompletion), and one
// trailing line is the shape a small model gets right most often.
const projectLabel = "PROJECT:"

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// matchEntity is normalizeEntity without its Unfiltered fallback: for a
// meeting, "none of them" is an answer to keep rather than a bucket to file
// under. Canonical casing comes from the user's list, not from the model.
func matchEntity(entity string, allowed []string) string {
	if entity == "" {
		return ""
	}
	for _, a := range allowed {
		if strings.EqualFold(a, entity) {
			return a
		}
	}
	return ""
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
		// In the sentences themselves this is only a nudge: if the meeting is
		// plainly about one of these, use its exact name rather than
		// describing it vaguely. For the PROJECT line below it is a closed
		// list, and matchEntity enforces that in code either way.
		entityHint = "Known project/client names: " +
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

` + projectAsk(entities) + extra + entityHint + `Transcript:
"""
` + text + `
"""`
}

// projectAsk is the instruction behind the reply's trailing PROJECT: line.
// With no configured projects there is nothing to choose from, so the line is
// not asked for at all rather than asked for and always answered "none".
func projectAsk(entities []string) string {
	if len(entities) == 0 {
		return ""
	}
	return `Then, on its own final line, write which project this meeting belongs to, in
exactly this form: "` + projectLabel + ` <name>". The name must be one of the known
projects below, copied exactly as written there, matched by meaning even if
the transcript spells or pronounces it differently. Never invent a project.
If the meeting is not clearly about any of them, write "` + projectLabel + ` none".

`
}
