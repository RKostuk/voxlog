package history

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Channel says which recording a turn is playable from. A meeting keeps the
// two sides of a call as separate WAV files (see meeting.go), so a reply
// cannot be played back without knowing which one it came in on.
const (
	ChannelMic    = 0
	ChannelSystem = 1
)

// YouSpeaker is the local id of the microphone's own speaker: whoever is
// holding the machine. Negative so it can never collide with a numbered
// far-end speaker.
const YouSpeaker = -1

// Turn is one stretch of one voice, in seconds from the start of the
// recording -- absolute, not relative to the 60-second block it was decoded
// in. That is the whole point of storing them: "play this reply" is
// meaningless without a timestamp the audio file agrees with.
type Turn struct {
	ID        int64
	Seq       int
	Channel   int
	StartSecs float64
	EndSecs   float64
	// SpeakerID points at meeting_speakers; LocalID and Name are carried
	// alongside so the window can render a turn without a second query.
	SpeakerID int64
	LocalID   int
	Name      string
	Text      string
}

// MeetingSpeaker is one voice within one meeting. It exists as its own row
// rather than a column on turns because everything the meeting screen shows
// -- talk time, share of the call, a name -- is per speaker, and because a
// name attached here renames every one of that speaker's replies at once.
type MeetingSpeaker struct {
	ID        int64
	LocalID   int
	VoiceID   int64 // 0 when this speaker has not been recognised as anyone
	Name      string
	TalkSecs  float64
	TurnCount int
	// Embed is the speaker's voice fingerprint for this meeting: the
	// centroid of their turns. It is what a later "who is this?" compares
	// against, and what Erase in the Voices pane destroys.
	Embed []float32
}

// ReplaceTurns writes the speakers and turns of one meeting, replacing
// whatever was there, and stamps the meeting with the extraction version
// behind them.
//
// One transaction, and idempotent by construction: re-running a decode for a
// meeting that already has turns produces the same rows rather than a second
// copy. That is what makes the background backfill safe to retry after a
// crash, which it will need, because it re-decodes hours of old audio.
func (s *MeetingStore) ReplaceTurns(start time.Time, speakers []MeetingSpeaker, turns []Turn, version int) error {
	db, err := s.open()
	if err != nil {
		return err
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("history: writing turns: %w", err)
	}
	defer tx.Rollback()

	ns := start.UnixNano()
	var exists int
	if err := tx.QueryRow("SELECT COUNT(*) FROM meetings WHERE start_ns = ?", ns).Scan(&exists); err != nil {
		return fmt.Errorf("history: writing turns: %w", err)
	}
	if exists == 0 {
		return fmt.Errorf("history: no meeting at %s", start.Format(time.RFC3339Nano))
	}

	// Turns first: they reference the speaker rows about to be deleted, and
	// that reference is ON DELETE SET NULL, which would quietly orphan them.
	if _, err := tx.Exec("DELETE FROM turns WHERE meeting_ns = ?", ns); err != nil {
		return fmt.Errorf("history: clearing turns: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM meeting_speakers WHERE meeting_ns = ?", ns); err != nil {
		return fmt.Errorf("history: clearing speakers: %w", err)
	}

	rowByLocal := make(map[int]int64, len(speakers))
	for _, sp := range speakers {
		var voiceID any
		if sp.VoiceID != 0 {
			voiceID = sp.VoiceID
		}
		res, err := tx.Exec(`
			INSERT INTO meeting_speakers
				(meeting_ns, local_id, voice_id, talk_secs, turn_count, embed)
			VALUES (?, ?, ?, ?, ?, ?)`,
			ns, sp.LocalID, voiceID, sp.TalkSecs, sp.TurnCount, EncodeVec(sp.Embed))
		if err != nil {
			return fmt.Errorf("history: writing speaker %d: %w", sp.LocalID, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("history: writing speaker %d: %w", sp.LocalID, err)
		}
		rowByLocal[sp.LocalID] = id
	}

	for i, t := range turns {
		var speakerID any
		if id, ok := rowByLocal[t.LocalID]; ok {
			speakerID = id
		}
		_, err := tx.Exec(`
			INSERT INTO turns
				(meeting_ns, seq, channel, start_secs, end_secs, speaker_id, text)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			ns, i, t.Channel, t.StartSecs, t.EndSecs, speakerID, t.Text)
		if err != nil {
			return fmt.Errorf("history: writing turn %d: %w", i, err)
		}
	}

	if _, err := tx.Exec("UPDATE meetings SET turns_version = ? WHERE start_ns = ?", version, ns); err != nil {
		return fmt.Errorf("history: stamping turn version: %w", err)
	}
	return tx.Commit()
}

// Turns returns one meeting's replies in the order they were said. Not part
// of the meetings list payload on purpose: an hour-long call is thousands of
// rows, and the list is rebuilt on every refresh.
func (s *MeetingStore) Turns(start time.Time) ([]Turn, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}

	rows, err := db.sql.Query(`
		SELECT t.id, t.seq, t.channel, t.start_secs, t.end_secs,
		       COALESCE(t.speaker_id, 0), COALESCE(ms.local_id, ?), COALESCE(v.name, ''), t.text
		FROM turns t
		LEFT JOIN meeting_speakers ms ON ms.id = t.speaker_id
		LEFT JOIN voices v ON v.id = ms.voice_id
		WHERE t.meeting_ns = ?
		ORDER BY t.seq`, YouSpeaker, start.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("history: reading turns: %w", err)
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var t Turn
		if err := rows.Scan(&t.ID, &t.Seq, &t.Channel, &t.StartSecs, &t.EndSecs,
			&t.SpeakerID, &t.LocalID, &t.Name, &t.Text); err != nil {
			return nil, fmt.Errorf("history: reading turns: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Speakers returns who spoke in one meeting and how much, newest-talker-last
// (local id order, which is the order they first spoke).
func (s *MeetingStore) Speakers(start time.Time) ([]MeetingSpeaker, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(speakerSelectSQL+" WHERE ms.meeting_ns = ? ORDER BY ms.local_id", start.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("history: reading speakers: %w", err)
	}
	defer rows.Close()
	out, _, err := scanSpeakers(rows)
	return out, err
}

// SpeakersByMeeting answers the same question for every meeting at once, in
// one query. The meetings list draws a talk-time bar on every row, and doing
// that per row would be a query per meeting on every refresh.
func (s *MeetingStore) SpeakersByMeeting() (map[int64][]MeetingSpeaker, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(speakerSelectSQL + " ORDER BY ms.meeting_ns, ms.local_id")
	if err != nil {
		return nil, fmt.Errorf("history: reading speakers: %w", err)
	}
	defer rows.Close()
	_, byMeeting, err := scanSpeakers(rows)
	return byMeeting, err
}

const speakerSelectSQL = `
	SELECT ms.meeting_ns, ms.id, ms.local_id, COALESCE(ms.voice_id, 0),
	       COALESCE(v.name, ''), ms.talk_secs, ms.turn_count, ms.embed
	FROM meeting_speakers ms
	LEFT JOIN voices v ON v.id = ms.voice_id`

func scanSpeakers(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]MeetingSpeaker, map[int64][]MeetingSpeaker, error) {
	var flat []MeetingSpeaker
	byMeeting := map[int64][]MeetingSpeaker{}
	for rows.Next() {
		var (
			sp        MeetingSpeaker
			meetingNS int64
			embed     []byte
		)
		if err := rows.Scan(&meetingNS, &sp.ID, &sp.LocalID, &sp.VoiceID,
			&sp.Name, &sp.TalkSecs, &sp.TurnCount, &embed); err != nil {
			return nil, nil, fmt.Errorf("history: reading speakers: %w", err)
		}
		sp.Embed = DecodeVec(embed)
		flat = append(flat, sp)
		byMeeting[meetingNS] = append(byMeeting[meetingNS], sp)
	}
	return flat, byMeeting, rows.Err()
}

// Speaker reads one speaker row by id -- what the window has to hand when the
// user points at somebody in a transcript and says who they are.
func (s *MeetingStore) Speaker(row int64) (MeetingSpeaker, error) {
	db, err := s.open()
	if err != nil {
		return MeetingSpeaker{}, err
	}
	var (
		sp        MeetingSpeaker
		meetingNS int64
		embed     []byte
	)
	err = db.sql.QueryRow(speakerSelectSQL+" WHERE ms.id = ?", row).Scan(
		&meetingNS, &sp.ID, &sp.LocalID, &sp.VoiceID, &sp.Name, &sp.TalkSecs, &sp.TurnCount, &embed)
	if err != nil {
		return MeetingSpeaker{}, fmt.Errorf("history: reading speaker %d: %w", row, err)
	}
	sp.Embed = DecodeVec(embed)
	return sp, nil
}

// SpeakerClipSource points at the best few seconds of one speaker: their
// longest reply, and which recording it is in. Naming a voice cuts its stored
// sample from here.
func (s *MeetingStore) SpeakerClipSource(row int64) (SpeakerCandidate, error) {
	db, err := s.open()
	if err != nil {
		return SpeakerCandidate{}, err
	}
	c, _, err := scanCandidate(db.sql.QueryRow(`
		SELECT ms.id, ms.meeting_ns, ms.talk_secs, ms.embed,
		       COALESCE(t.text, ''), COALESCE(t.channel, 0),
		       COALESCE(t.start_secs, 0), COALESCE(t.end_secs, 0),
		       m.audio_path, m.system_audio_path
		FROM meeting_speakers ms
		JOIN meetings m ON m.start_ns = ms.meeting_ns
		LEFT JOIN turns t ON t.id = (
			SELECT id FROM turns
			WHERE speaker_id = ms.id
			ORDER BY (end_secs - start_secs) DESC
			LIMIT 1
		)
		WHERE ms.id = ?`, row))
	if err != nil {
		return SpeakerCandidate{}, err
	}
	return c, nil
}

// NeedsTurns picks the next meeting worth re-reading for turns: one whose
// stored turns predate the current extraction version (or that has none at
// all) and whose audio is still on disk, newest first.
//
// Derived from state rather than from a job table, which is what makes the
// backfill resumable for free: a decode that crashes writes nothing, so the
// same meeting is picked again next time, and one that finishes is never
// picked again. Raising the version constant re-queues everything.
//
// attemptCap stops a recording that reliably kills the decoder from being
// retried forever.
func (s *MeetingStore) NeedsTurns(version, attemptCap int) (Meeting, bool, error) {
	db, err := s.open()
	if err != nil {
		return Meeting{}, false, err
	}
	m, err := scanMeeting(db.sql.QueryRow(selectMeetingSQL+`
		WHERE turns_version < ? AND audio_path <> '' AND backfill_attempts < ?
		ORDER BY start_ns DESC LIMIT 1`, version, attemptCap))
	if errors.Is(err, sql.ErrNoRows) {
		return Meeting{}, false, nil
	}
	if err != nil {
		return Meeting{}, false, fmt.Errorf("history: looking for meetings to re-read: %w", err)
	}
	return m, true, nil
}

// NoteBackfillAttempt records that a re-read was started, before it is. A
// meeting that takes the decoder down with it therefore still gets its
// attempt counted, which is the only thing that can stop the loop retrying it
// on every launch.
func (s *MeetingStore) NoteBackfillAttempt(start time.Time) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(
		"UPDATE meetings SET backfill_attempts = backfill_attempts + 1 WHERE start_ns = ?", start.UnixNano())
	if err != nil {
		return fmt.Errorf("history: noting a re-read attempt: %w", err)
	}
	return nil
}

// GiveUpOnTurns marks a meeting as not worth re-reading, with the reason.
// Used when the audio is simply gone: nothing about that will change on the
// next launch, so retrying it would be a permanent no-op that hides the
// meetings that could still be recovered.
func (s *MeetingStore) GiveUpOnTurns(start time.Time, version int, reason string) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	_, err = db.sql.Exec(
		"UPDATE meetings SET turns_version = ?, backfill_error = ? WHERE start_ns = ?",
		version, reason, start.UnixNano())
	if err != nil {
		return fmt.Errorf("history: giving up on a re-read: %w", err)
	}
	return nil
}
