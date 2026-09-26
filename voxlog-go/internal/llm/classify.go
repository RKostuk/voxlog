package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Result is one task found in a transcript.
type Result struct {
	Text string
	// Notes is a short description the model adds when the transcript says
	// more about the task than fits its one line; "" otherwise.
	Notes  string
	Entity string
	// Status is one of "todo"/"in_progress"/"blocked"/"done" -- validated in
	// Classify, falls back to "todo" if the model returns anything else or
	// leaves it out.
	Status   string
	Reminder *time.Time
}

type taskJSON struct {
	Text     string  `json:"text"`
	Notes    string  `json:"notes"`
	Entity   string  `json:"entity"`
	Status   string  `json:"status"`
	Reminder *string `json:"reminder"`
}

type classifyJSON struct {
	Tasks []taskJSON `json:"tasks"`
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

// Classify asks the model which tasks, if any, text contains -- none for a
// passing remark, one or several when the speaker listed them -- and for
// each the task text, a description, a project (one of entities, or
// UnfilteredEntity), a status, and an optional reminder resolved against
// now. modelDir is the directory asr.ModelDir(baseDir, Spec) resolves to --
// mlx_lm's --model flag. rejected is a short list of past task texts the
// user explicitly marked "Not a task" (see task.Store.LoadRejected) -- a
// bounded negative-example nudge, not training. rules is the user's own
// "what counts as a task" text from Settings (see buildPrompt), "" for none.
//
// On OpenRouter each configured model is asked in turn until one answers
// with JSON that parses (see askInTurn).
func (c *Cache) Classify(modelDir, text string, entities, rejected []string, rules string, now time.Time, ep Endpoint) ([]Result, error) {
	target, err := c.resolve(modelDir, ep)
	if err != nil {
		return nil, err
	}
	var out []Result
	err = askInTurn(target, buildPrompt(text, entities, rejected, rules, now), func(reply string) error {
		var perr error
		out, perr = parseClassification(reply, entities)
		return perr
	})
	return out, err
}

// parseClassification reads the model's reply into tasks. A reply that is
// not the JSON asked for is an error, so the next model gets a turn; a task
// with no text is dropped, and a reminder that does not parse is dropped
// from its task -- the task itself is still worth keeping.
func parseClassification(reply string, entities []string) ([]Result, error) {
	jsonText, err := extractJSONObject(reply)
	if err != nil {
		return nil, fmt.Errorf("no JSON object in reply: %w (raw: %s)", err, reply)
	}
	var parsed classifyJSON
	if err := json.Unmarshal([]byte(jsonText), &parsed); err != nil {
		return nil, fmt.Errorf("parsing classification: %w (raw: %s)", err, jsonText)
	}
	var out []Result
	for _, t := range parsed.Tasks {
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		res := Result{Text: text, Notes: strings.TrimSpace(t.Notes), Entity: normalizeEntity(t.Entity, entities), Status: "todo"}
		if validStatuses[t.Status] {
			res.Status = t.Status
		}
		if t.Reminder != nil && *t.Reminder != "" {
			if when, err := time.Parse(time.RFC3339, *t.Reminder); err == nil {
				res.Reminder = &when
			}
		}
		out = append(out, res)
	}
	return out, nil
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
		"actual tasks before -- if part of this transcript reads similarly, " +
		"do not make a task of it: " + strings.Join(quoted, "; ") + "\n"
}

// maxTaskRulesChars bounds the user's rules the way maxTranscriptChars bounds
// the transcript: a pasted essay would crowd out the transcript it is meant
// to judge in a small model's context.
const maxTaskRulesChars = 1500

// rulesBlock is the user's own instructions for telling a task from a
// passing remark. Added to the built-in prompt, never in place of it: the
// reply is parsed as JSON, so the format has to outlive whatever the rules
// say -- the same bargain SummaryOptions.Extra makes.
func rulesBlock(rules string) string {
	rules = strings.TrimSpace(rules)
	if rules == "" {
		return ""
	}
	if len(rules) > maxTaskRulesChars {
		rules = strings.ToValidUTF8(rules[:maxTaskRulesChars], "")
	}
	// A dictation meant as text -- a message, a reply, a prompt -- is full
	// of things to do, and with output mode "paste unless task" every one
	// read as a task is text that never arrives where it was dictated. So a
	// trigger the user names is the only way in, not one way among several.
	return "\nAdditional rules from the user, to follow as long as they do not\n" +
		"contradict the JSON format asked for below. If these rules say what\n" +
		"makes something a task -- trigger words or phrases such as \"створи\n" +
		"задачу\" -- they are the ONLY way a task is made: a transcript that does\n" +
		"not match them has no tasks, however actionable it sounds, and\n" +
		"{\"tasks\": []} is the answer:\n" + rules + "\n"
}

// upcomingDays lists the next seven days by name and date. Small models
// resolve "by Friday" to a day already past, or to tomorrow, when left to do
// the calendar arithmetic themselves; looking a date up is something they
// get right.
func upcomingDays(now time.Time) string {
	days := make([]string, 7)
	for i := range days {
		d := now.AddDate(0, 0, i+1)
		days[i] = d.Format("Monday 2006-01-02")
	}
	days[0] = "tomorrow = " + days[0]
	return strings.Join(days, ", ")
}

func buildPrompt(text string, entities, rejected []string, rules string, now time.Time) string {
	if len(text) > maxTranscriptChars {
		text = text[:maxTranscriptChars]
	}
	entityList := "(none configured)"
	if len(entities) > 0 {
		entityList = strings.Join(entities, ", ")
	}
	return fmt.Sprintf(`You turn a dictated or meeting transcript into tasks for a task-tracking app.
Find every concrete, actionable task the speaker (or someone they mention)
needs to do. Casual notes, questions, thinking out loud or general
discussion are NOT tasks -- then the list is empty. If the speaker names
several tasks, return each one separately.
%s%s
The only allowed projects are this fixed list: %s.
Set a task's entity to one of these EXACTLY as written only when the
transcript names that project or is plainly about it, matching by meaning
even if it is spelled or pronounced differently. Never invent a new project
name and never guess: a task that does not mention one of them -- most
everyday tasks -- gets entity exactly "Unfiltered".

Judge each task's status from how the speaker talks about it:
- "blocked" if they say they're stuck, waiting on someone/something, or
  can't proceed.
- "done" if they say it's already finished (a task logged after the fact).
- "in_progress" if they say they've already started or are in the middle of
  it.
- "todo" otherwise -- the default when nothing suggests progress has begun.

The current date and time is %s -- resolve any relative time ("tomorrow",
"Friday", "in an hour") against it. A reminder is always in the future.
Never work a date out yourself; take it from this list, where the first day
is tomorrow: %s.
If the speaker asks to be reminded or names a time or deadline for a task,
set its reminder; otherwise reminder must be null.

Transcript:
"""
%s
"""

Write "text" as a short, self-contained task title with speech filler and
false starts cleaned up. Write "notes" only when the transcript gives
details the title leaves out (who, what exactly, how) -- never repeat the
title or the time; otherwise "".
LANGUAGE: "text" and "notes" MUST be in the transcript's own language -- a
Ukrainian transcript gives Ukrainian tasks. Never translate into English.

Respond with ONLY a single JSON object, nothing before or after it, no
markdown code fence, exactly this shape:
{"tasks": [{"text": "...", "notes": "...", "entity": "...", "status": "todo" or "in_progress" or "blocked" or "done", "reminder": "RFC3339 timestamp or null"}]}
Use {"tasks": []} when there is no task.`,
		rejectedBlock(rejected), rulesBlock(rules), entityList, now.Format("Monday, ")+now.Format(time.RFC3339), upcomingDays(now), text)
}
