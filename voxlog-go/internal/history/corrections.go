package history

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Corrections: who spoke, according to the person who was there.
//
// Diarization is wrong often enough to matter, and a listener always knows
// better than a clustering threshold. A correction is that knowledge, stored
// so it outlives the pipeline that got it wrong.
//
// It is stored against the recording's clock rather than against a turn,
// because turns do not survive. ReplaceTurns deletes every turn and speaker
// row of a meeting and writes them again, which is what happens each time the
// extraction version is raised and the backfill re-runs old recordings through
// an improved pipeline -- the exact event a correction has to outlast. A
// stretch of seconds is still the same stretch afterwards, however the replies
// have been re-cut.

// ErrNoSuchTurn is returned when a correction names a reply that is not there.
var ErrNoSuchTurn = errors.New("history: no such reply")

// CorrectTurn records that the reply at seq was spoken by voiceID, applies it,
// and returns the meeting's speakers as they now stand.
//
// The correction is stored as that reply's stretch of the recording, not its
// seq: the seq is how the user pointed at it, the range is what survives.
func (s *MeetingStore) CorrectTurn(start time.Time, seq int, voiceID int64) ([]MeetingSpeaker, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}

	ns := start.UnixNano()
	tx, err := db.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("history: correcting a reply: %w", err)
	}
	defer tx.Rollback()

	var from, to float64
	err = tx.QueryRow("SELECT start_secs, end_secs FROM turns WHERE meeting_ns = ? AND seq = ?", ns, seq).Scan(&from, &to)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuchTurn
	}
	if err != nil {
		return nil, fmt.Errorf("history: correcting a reply: %w", err)
	}

	if err := correctRange(tx, ns, from, to, voiceID); err != nil {
		return nil, err
	}
	if err := applyCorrections(tx, ns); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("history: correcting a reply: %w", err)
	}
	return s.Speakers(start)
}

// MergeSpeakers records that every reply of one speaker belongs to another.
//
// It is written as one correction per reply rather than as a correction about
// speakers, so there is a single mechanism to apply, export and undo. The
// action is one click; the rows behind it are per-reply because that is the
// shape that survives a re-decode.
func (s *MeetingStore) MergeSpeakers(from, into int64) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	if from == into {
		return nil
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("history: merging speakers: %w", err)
	}
	defer tx.Rollback()

	var ns int64
	var voiceID sql.NullInt64
	err = tx.QueryRow("SELECT meeting_ns, voice_id FROM meeting_speakers WHERE id = ?", into).Scan(&ns, &voiceID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("history: merging speakers: no speaker %d", into)
	}
	if err != nil {
		return fmt.Errorf("history: merging speakers: %w", err)
	}
	if !voiceID.Valid {
		// Merging into a speaker nobody has recognised has nothing to record
		// against: a correction points at a voice. Naming the speaker first is
		// what the picker does, and what the caller must do here.
		return fmt.Errorf("history: merging speakers: speaker %d has no voice to merge into", into)
	}

	// Both sides of the merge, not only the replies that move. A correction is
	// the only thing that survives a re-decode, and a merge that recorded just
	// the moving half would come apart again the next time the meeting was
	// decoded: the speaker they were merged INTO comes back unnamed, because
	// ReplaceTurns writes speaker rows with no voice on them.
	rows, err := tx.Query(
		"SELECT start_secs, end_secs FROM turns WHERE speaker_id IN (?, ?) ORDER BY seq", from, into)
	if err != nil {
		return fmt.Errorf("history: merging speakers: %w", err)
	}
	type span struct{ from, to float64 }
	var spans []span
	for rows.Next() {
		var sp span
		if err := rows.Scan(&sp.from, &sp.to); err != nil {
			rows.Close()
			return fmt.Errorf("history: merging speakers: %w", err)
		}
		spans = append(spans, sp)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("history: merging speakers: %w", err)
	}

	for _, sp := range spans {
		if err := correctRange(tx, ns, sp.from, sp.to, voiceID.Int64); err != nil {
			return err
		}
	}
	if err := applyCorrections(tx, ns); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearCorrection removes the correction covering a reply and re-applies what
// is left, returning the reply to whatever the pipeline decided.
func (s *MeetingStore) ClearCorrection(start time.Time, seq int) error {
	db, err := s.open()
	if err != nil {
		return err
	}

	ns := start.UnixNano()
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("history: clearing a correction: %w", err)
	}
	defer tx.Rollback()

	var from, to float64
	err = tx.QueryRow("SELECT start_secs, end_secs FROM turns WHERE meeting_ns = ? AND seq = ?", ns, seq).Scan(&from, &to)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoSuchTurn
	}
	if err != nil {
		return fmt.Errorf("history: clearing a correction: %w", err)
	}

	mid := (from + to) / 2
	if _, err := tx.Exec(
		"DELETE FROM turn_corrections WHERE meeting_ns = ? AND start_secs <= ? AND end_secs >= ?",
		ns, mid, mid); err != nil {
		return fmt.Errorf("history: clearing a correction: %w", err)
	}

	// Every speaker row is rebuilt from the pipeline's own answer before what
	// is left of the corrections goes back on, so clearing one really does
	// undo it rather than leave its speaker row behind.
	if err := resetSpeakers(tx, ns); err != nil {
		return err
	}
	if err := applyCorrections(tx, ns); err != nil {
		return err
	}
	return tx.Commit()
}

// Corrections returns a meeting's corrections in start order. Read by the
// window, to mark which replies a person has already been through, and by the
// reference export in tools/diareval.
func (s *MeetingStore) Corrections(start time.Time) ([]Correction, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(`
		SELECT c.id, c.start_secs, c.end_secs, c.voice_id, COALESCE(v.name, '')
		FROM turn_corrections c
		LEFT JOIN voices v ON v.id = c.voice_id
		WHERE c.meeting_ns = ?
		ORDER BY c.start_secs`, start.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("history: reading corrections: %w", err)
	}
	defer rows.Close()

	var out []Correction
	for rows.Next() {
		var c Correction
		if err := rows.Scan(&c.ID, &c.StartSecs, &c.EndSecs, &c.VoiceID, &c.Name); err != nil {
			return nil, fmt.Errorf("history: reading corrections: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Correction is one stretch of a recording a person has said belongs to
// someone in particular.
type Correction struct {
	ID        int64
	StartSecs float64
	EndSecs   float64
	VoiceID   int64
	Name      string
}

// correctRange stores one correction, replacing any that already covers the
// same stretch: correcting the same reply twice is a person changing their
// mind, not two opinions to reconcile.
func correctRange(tx *sql.Tx, ns int64, from, to float64, voiceID int64) error {
	mid := (from + to) / 2
	if _, err := tx.Exec(
		"DELETE FROM turn_corrections WHERE meeting_ns = ? AND start_secs <= ? AND end_secs >= ?",
		ns, mid, mid); err != nil {
		return fmt.Errorf("history: recording a correction: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO turn_corrections (meeting_ns, start_secs, end_secs, voice_id, created_ns)
		VALUES (?, ?, ?, ?, ?)`, ns, from, to, voiceID, time.Now().UnixNano()); err != nil {
		return fmt.Errorf("history: recording a correction: %w", err)
	}
	return nil
}

// applyCorrections lays a meeting's corrections over whatever the pipeline
// decided. Called at the end of every decode and after every correction, so
// the two can never disagree about what the transcript says.
//
// A reply belongs to the correction its MIDDLE falls inside. Overlap alone
// would let a correction claim a neighbour it merely touches -- and replies do
// touch, because the segmentation model pads them.
func applyCorrections(tx *sql.Tx, ns int64) error {
	rows, err := tx.Query(
		"SELECT start_secs, end_secs, voice_id FROM turn_corrections WHERE meeting_ns = ? ORDER BY start_secs", ns)
	if err != nil {
		return fmt.Errorf("history: applying corrections: %w", err)
	}
	type correction struct {
		from, to float64
		voiceID  int64
	}
	var corrections []correction
	for rows.Next() {
		var c correction
		if err := rows.Scan(&c.from, &c.to, &c.voiceID); err != nil {
			rows.Close()
			return fmt.Errorf("history: applying corrections: %w", err)
		}
		corrections = append(corrections, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("history: applying corrections: %w", err)
	}
	if len(corrections) == 0 {
		return nil
	}

	for _, c := range corrections {
		row, err := speakerForVoice(tx, ns, c.voiceID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
			UPDATE turns SET speaker_id = ?
			WHERE meeting_ns = ?
			  AND (start_secs + end_secs) / 2 >= ?
			  AND (start_secs + end_secs) / 2 <= ?`, row, ns, c.from, c.to); err != nil {
			return fmt.Errorf("history: applying corrections: %w", err)
		}
	}

	return recountSpeakers(tx, ns)
}

// speakerForVoice finds the row a voice already occupies in this meeting, or
// makes one. A correction naming someone who was not in the transcript at all
// is the whole point of the feature: the pipeline missed them.
func speakerForVoice(tx *sql.Tx, ns, voiceID int64) (int64, error) {
	var row int64
	err := tx.QueryRow(
		"SELECT id FROM meeting_speakers WHERE meeting_ns = ? AND voice_id = ?", ns, voiceID).Scan(&row)
	if err == nil {
		if _, err := tx.Exec("UPDATE meeting_speakers SET voice_manual = 1 WHERE id = ?", row); err != nil {
			return 0, fmt.Errorf("history: applying corrections: %w", err)
		}
		return row, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("history: applying corrections: %w", err)
	}

	var next sql.NullInt64
	if err := tx.QueryRow(
		"SELECT MAX(local_id) FROM meeting_speakers WHERE meeting_ns = ?", ns).Scan(&next); err != nil {
		return 0, fmt.Errorf("history: applying corrections: %w", err)
	}
	local := 0
	if next.Valid {
		local = int(next.Int64) + 1
	}
	res, err := tx.Exec(`
		INSERT INTO meeting_speakers (meeting_ns, local_id, voice_id, talk_secs, turn_count, voice_manual)
		VALUES (?, ?, ?, 0, 0, 1)`, ns, local, voiceID)
	if err != nil {
		return 0, fmt.Errorf("history: applying corrections: %w", err)
	}
	row, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("history: applying corrections: %w", err)
	}
	return row, nil
}

// recountSpeakers brings the denormalised talk time and reply count back in
// line with the turns each speaker now holds. They are denormalised because
// the meetings list draws a talk-time bar on every row; that means every move
// of a reply has to pay for it here.
func recountSpeakers(tx *sql.Tx, ns int64) error {
	_, err := tx.Exec(`
		UPDATE meeting_speakers SET
			talk_secs = COALESCE((SELECT SUM(end_secs - start_secs) FROM turns WHERE speaker_id = meeting_speakers.id), 0),
			turn_count = COALESCE((SELECT COUNT(*) FROM turns WHERE speaker_id = meeting_speakers.id), 0)
		WHERE meeting_ns = ?`, ns)
	if err != nil {
		return fmt.Errorf("history: recounting speakers: %w", err)
	}
	// A speaker a correction emptied is no longer in the meeting. One the
	// pipeline produced is left alone even when empty: that is a decode
	// result, not a leftover.
	if _, err := tx.Exec(`
		DELETE FROM meeting_speakers
		WHERE meeting_ns = ? AND turn_count = 0 AND voice_manual = 1
		  AND NOT EXISTS (SELECT 1 FROM turn_corrections c WHERE c.meeting_ns = meeting_speakers.meeting_ns
		                    AND c.voice_id = meeting_speakers.voice_id)`, ns); err != nil {
		return fmt.Errorf("history: recounting speakers: %w", err)
	}
	return nil
}

// resetSpeakers undoes every correction's effect on the transcript, putting
// each reply back with the speaker the last decode gave it -- which is why
// that answer is kept in its own column rather than overwritten. Only
// ClearCorrection needs it: after a decode the rows are already the
// pipeline's own.
func resetSpeakers(tx *sql.Tx, ns int64) error {
	if _, err := tx.Exec(
		"UPDATE turns SET speaker_id = decoded_speaker_id WHERE meeting_ns = ?", ns); err != nil {
		return fmt.Errorf("history: clearing a correction: %w", err)
	}
	if _, err := tx.Exec(
		"UPDATE meeting_speakers SET voice_manual = 0 WHERE meeting_ns = ?", ns); err != nil {
		return fmt.Errorf("history: clearing a correction: %w", err)
	}
	return nil
}

// SetSpeakerEmbed replaces one speaker's fingerprint and recomputes the voice
// it belongs to.
//
// It exists because the fingerprint cannot be computed here. This package is
// deliberately free of cgo -- see the note on Cosine in vec.go -- and
// describing a voice means running a 27 MB model over its audio. So the app
// recomputes, and hands the answer back through this.
func (s *MeetingStore) SetSpeakerEmbed(speakerRow int64, embed []float32) error {
	db, err := s.open()
	if err != nil {
		return err
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("history: storing a fingerprint: %w", err)
	}
	defer tx.Rollback()

	var voiceID sql.NullInt64
	if err := tx.QueryRow("SELECT voice_id FROM meeting_speakers WHERE id = ?", speakerRow).Scan(&voiceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // The speaker was corrected out of existence meanwhile.
		}
		return fmt.Errorf("history: storing a fingerprint: %w", err)
	}
	if _, err := tx.Exec("UPDATE meeting_speakers SET embed = ? WHERE id = ?", EncodeVec(embed), speakerRow); err != nil {
		return fmt.Errorf("history: storing a fingerprint: %w", err)
	}
	if voiceID.Valid {
		if err := recomputeVoice(tx, voiceID.Int64); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SpeakerTurns returns the replies each speaker of a meeting now holds, which
// is what their fingerprint has to be rebuilt from after a correction moves
// replies between them.
func (s *MeetingStore) SpeakerTurns(start time.Time) (map[int64][]Turn, error) {
	turns, err := s.Turns(start)
	if err != nil {
		return nil, err
	}
	out := map[int64][]Turn{}
	for _, t := range turns {
		if t.SpeakerID == 0 {
			continue
		}
		out[t.SpeakerID] = append(out[t.SpeakerID], t)
	}
	return out, nil
}
