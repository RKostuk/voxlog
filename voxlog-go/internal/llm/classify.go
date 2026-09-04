package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Result struct {
	IsTask bool
	Text   string
	Entity string
	// Status is one of "todo"/"in_progress"/"blocked"/"done" -- validated in
	// Classify, falls back to "todo" if the model returns anything else or
	// leaves it out.
	Status   string
	Reminder *time.Time
}

type classifyJSON struct {
	IsTask   bool    `json:"is_task"`
	Text     string  `json:"text"`
	Entity   string  `json:"entity"`
	Status   string  `json:"status"`
	Reminder *string `json:"reminder"`
}

var validStatuses = map[string]bool{"todo": true, "in_progress": true, "blocked": true, "done": true}

// UnfilteredEntity is the fixed catch-all for a task that doesn't match any
// of the user's configured projects (see settings.EntityDictionary). The
// model is instructed to use it, but Classify also enforces it in code --
// entities is now a closed list the user wrote, not something the model may
// extend, and a local 4B model does not always follow instructions to the
// letter.
const UnfilteredEntity = "Unfiltered"

// normalizeEntity maps whatever the model returned onto exactly one of
// allowed (matched case-insensitively, canonical casing from allowed wins)
// or UnfilteredEntity if it matches nothing -- including when allowed is
// empty, or the model left entity blank or invented something new.
func normalizeEntity(entity string, allowed []string) string {
	for _, a := range allowed {
		if strings.EqualFold(a, entity) {
			return a
		}
	}
	return UnfilteredEntity
}

// maxTranscriptChars keeps the prompt within a 4B model's comfortable
// context and this app's latency budget -- a 20-minute meeting transcript
// doesn't need to travel in full for a task-detection pass.
// ponytail: naive head-truncation; upgrade to a summarizing pre-pass only if
// long meetings start getting misclassified in practice.
const maxTranscriptChars = 3000

// Classify asks the model whether text describes an actionable task, and if
// so extracts the task text, an entity/project name (reused from entities
// when the transcript is plainly about one of them, or a short new one), and
// an optional reminder time resolved against now. modelDir is the directory
// asr.ModelDir(baseDir, Spec) resolves to -- mlx_lm's --model flag. rejected
// is a short list of past task texts the user explicitly marked "Not a
// task" (see task.Store.LoadRejected) -- a bounded negative-example nudge,
// not training, so it stays short enough to not meaningfully grow the
// prompt.
func (c *Cache) Classify(modelDir, text string, entities, rejected []string, now time.Time) (Result, error) {
	base, err := c.baseURL(modelDir)
	if err != nil {
		return Result{}, err
	}

	content, err := chatCompletion(base, buildPrompt(text, entities, rejected, now))
	if err != nil {
		return Result{}, err
	}

	jsonText, err := extractJSONObject(content)
	if err != nil {
		return Result{}, fmt.Errorf("llm: no JSON object in reply: %w (raw: %s)", err, content)
	}

	var parsed classifyJSON
	if err := json.Unmarshal([]byte(jsonText), &parsed); err != nil {
		return Result{}, fmt.Errorf("llm: parsing classification: %w (raw: %s)", err, jsonText)
	}
	if !parsed.IsTask {
		return Result{IsTask: false}, nil
	}

	res := Result{IsTask: true, Text: parsed.Text, Entity: normalizeEntity(parsed.Entity, entities), Status: "todo"}
	if validStatuses[parsed.Status] {
		res.Status = parsed.Status
	}
	if parsed.Reminder != nil && *parsed.Reminder != "" {
		// A reminder that fails to parse is dropped, not an error -- the task
		// itself is still worth keeping.
		if t, err := time.Parse(time.RFC3339, *parsed.Reminder); err == nil {
			res.Reminder = &t
		}
	}
	return res, nil
}

// extractJSONObject pulls the first balanced {...} substring out of s. The
// server has no grammar-constrained output (unlike llama.cpp's
// response_format), so a reply is asked to be pure JSON but may still carry
// stray whitespace, a code fence, or trailing prose around it -- this finds
// the object without trusting the rest of the string.
func extractJSONObject(s string) (string, error) {
	start := strings.IndexByte(s, '{')
	if start == -1 {
		return "", fmt.Errorf("no '{' found")
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced braces")
}

// maxRejectedChars is how much of each rejected example survives into the
// prompt -- these are a calibration nudge, not the transcript under review,
// so a short excerpt is plenty and keeps this section from growing the
// prompt by much even at the full maxRejected count.
const maxRejectedChars = 60

func rejectedBlock(rejected []string) string {
	if len(rejected) == 0 {
		return ""
	}
	quoted := make([]string, len(rejected))
	for i, r := range rejected {
		if len(r) > maxRejectedChars {
			r = r[:maxRejectedChars] + "…"
		}
		quoted[i] = `"` + r + `"`
	}
	return "\nThe user has explicitly marked text like the following as NOT " +
		"actual tasks before -- if this transcript reads similarly, prefer " +
		"is_task=false: " + strings.Join(quoted, "; ") + "\n"
}

func buildPrompt(text string, entities, rejected []string, now time.Time) string {
	if len(text) > maxTranscriptChars {
		text = text[:maxTranscriptChars]
	}
	entityList := "(none configured)"
	if len(entities) > 0 {
		entityList = strings.Join(entities, ", ")
	}
	return fmt.Sprintf(`You classify a dictated or meeting transcript for a task-tracking app.
Decide if it contains a concrete, actionable task the speaker (or someone
they mention) needs to do. Casual notes, questions, or general discussion
are NOT tasks.
%s
The only allowed projects are this fixed list: %s.
You MUST set entity to one of these EXACTLY as written, matching by meaning
even if the transcript spells or pronounces it differently. Never invent a
new project name. If the transcript is not clearly about any of these, or
the list is empty, set entity to exactly "Unfiltered".

Also judge the task's status from how the speaker talks about it:
- "blocked" if they say they're stuck, waiting on someone/something, or
  can't proceed.
- "done" if they say it's already finished (a task logged after the fact).
- "in_progress" if they say they've already started or are in the middle of
  it.
- "todo" otherwise -- the default when nothing suggests progress has begun.

The current date and time is %s -- resolve any relative time ("tomorrow",
"Friday", "in an hour") against it. If no time is mentioned, reminder must be
null.

Transcript:
"""
%s
"""

Respond with ONLY a single JSON object, nothing before or after it, no
markdown code fence, exactly these keys:
{"is_task": true or false, "text": "...", "entity": "...", "status": "todo" or "in_progress" or "blocked" or "done", "reminder": "RFC3339 timestamp or null"}`,
		rejectedBlock(rejected), entityList, now.Format(time.RFC3339), text)
}
