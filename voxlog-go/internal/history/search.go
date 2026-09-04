package history

import (
	"fmt"
	"strings"
	"time"
)

// SearchHit is one turn that matched a search, with enough context around it
// to draw a result row without a second query: which meeting it is in, when
// in that recording it happens, and who said it.
type SearchHit struct {
	Start     time.Time
	TurnID    int64
	Seq       int
	Channel   int
	StartSecs float64
	EndSecs   float64
	LocalID   int
	Name      string
	Text      string
	// Snippet is the matched text with the matching words wrapped in [[ ]].
	// Markers rather than HTML: the window escapes everything it renders,
	// and handing it markup to trust would be the one exception.
	Snippet string
}

// searchLimit bounds a result set. Search is a way into a meeting, not a
// report: past a screenful the useful move is a narrower query, not more
// scrolling.
const searchLimit = 100

// SearchTurns finds turns matching query across every meeting, most recent
// first. The query is prefix-matched word by word, so typing "invoi" finds
// "invoice" -- searching a transcript is a way of remembering a
// conversation, and half-remembered words are the normal case.
//
// An empty or punctuation-only query returns nothing rather than everything:
// the caller is a search box, and the box being empty is not a request for
// every turn ever recorded.
func (s *MeetingStore) SearchTurns(query string) ([]SearchHit, error) {
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}

	db, err := s.open()
	if err != nil {
		return nil, err
	}

	rows, err := db.sql.Query(`
		SELECT t.meeting_ns, t.id, t.seq, t.channel, t.start_secs, t.end_secs,
		       COALESCE(ms.local_id, ?), COALESCE(v.name, ''), t.text,
		       snippet(turns_fts, 0, '[[', ']]', '…', 12)
		FROM turns_fts
		JOIN turns t ON t.id = turns_fts.rowid
		LEFT JOIN meeting_speakers ms ON ms.id = t.speaker_id
		LEFT JOIN voices v ON v.id = ms.voice_id
		WHERE turns_fts MATCH ?
		ORDER BY t.meeting_ns DESC, t.seq
		LIMIT ?`, YouSpeaker, match, searchLimit)
	if err != nil {
		return nil, fmt.Errorf("history: searching turns: %w", err)
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var ns int64
		if err := rows.Scan(&ns, &h.TurnID, &h.Seq, &h.Channel, &h.StartSecs, &h.EndSecs,
			&h.LocalID, &h.Name, &h.Text, &h.Snippet); err != nil {
			return nil, fmt.Errorf("history: searching turns: %w", err)
		}
		h.Start = time.Unix(0, ns)
		out = append(out, h)
	}
	return out, rows.Err()
}

// ftsQuery turns what the user typed into an FTS5 MATCH expression.
//
// Everything is rebuilt from scratch rather than escaped, because FTS5's
// query language is not a subset of plain text: a stray quote, NEAR or "-"
// is a syntax error, and a syntax error in a search box as you type is
// indistinguishable from the app being broken. Words are extracted, quoted
// as literals, given a prefix "*", and ANDed.
func ftsQuery(query string) string {
	var words []string
	for _, w := range strings.FieldsFunc(query, func(r rune) bool {
		return !isSearchRune(r)
	}) {
		// The quotes make the word a literal, so nothing inside it can be
		// read as an operator; the doubling is FTS5's own escape for a quote
		// that survived isSearchRune (it cannot today, and this stays correct
		// if that ever changes).
		words = append(words, `"`+strings.ReplaceAll(w, `"`, `""`)+`"*`)
	}
	return strings.Join(words, " AND ")
}

// isSearchRune keeps letters, digits and the apostrophe -- "don't" is one
// word, and the tokenizer treats it as one too.
func isSearchRune(r rune) bool {
	switch {
	case r >= '0' && r <= '9':
		return true
	case r == '\'':
		return true
	}
	return isLetter(r)
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127
}

// Person is one voice seen across every meeting: who the user actually
// talks to, and how much. Answers "who am I really in meetings with", which
// is a question no single meeting can.
type Person struct {
	VoiceID   int64
	Name      string
	Meetings  int
	TalkSecs  float64
	TurnCount int
	LastSeen  time.Time
}

// People aggregates meeting_speakers by voice, most talked-with first.
// Speakers who have not been recognised as anyone are left out: they are one
// row per meeting by definition, so counting them here would say nothing
// except how many meetings had strangers in them.
func (s *MeetingStore) People() ([]Person, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(`
		SELECT v.id, v.name, COUNT(DISTINCT ms.meeting_ns),
		       COALESCE(SUM(ms.talk_secs), 0), COALESCE(SUM(ms.turn_count), 0),
		       MAX(ms.meeting_ns)
		FROM meeting_speakers ms
		JOIN voices v ON v.id = ms.voice_id
		GROUP BY v.id, v.name
		ORDER BY SUM(ms.talk_secs) DESC`)
	if err != nil {
		return nil, fmt.Errorf("history: reading people: %w", err)
	}
	defer rows.Close()

	var out []Person
	for rows.Next() {
		var p Person
		var lastNS int64
		if err := rows.Scan(&p.VoiceID, &p.Name, &p.Meetings, &p.TalkSecs, &p.TurnCount, &lastNS); err != nil {
			return nil, fmt.Errorf("history: reading people: %w", err)
		}
		p.LastSeen = time.Unix(0, lastNS)
		out = append(out, p)
	}
	return out, rows.Err()
}
