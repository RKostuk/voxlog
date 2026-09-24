package history

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Voices are people, as far as this program can tell them apart: a name and
// the fingerprint of the voice that carries it.
//
// A name lives here rather than on a turn on purpose. Name a voice once and
// every reply it ever spoke -- in meetings recorded months ago -- reads with
// that name, and forgetting it takes them all back to "Speaker 2" without
// touching a single word of transcript.

// IdentifyThreshold is how alike a meeting's speaker must be to a known voice
// before the name is applied with nobody asked. It is deliberately stricter
// than the threshold used to join two stretches of one meeting: a wrong name
// spread across a user's whole history is a much worse failure than an
// unnamed speaker, and the gap between the two thresholds is where the Voices
// pane asks instead of assuming.
const IdentifyThreshold = 0.72

// SuggestThreshold is the bottom of that band. Below it, a voice is not worth
// mentioning as a possibility.
const SuggestThreshold = 0.55

// nameKey folds a name the way a person would: "Олег" and "олег" are one
// person, in Cyrillic as much as in ASCII.
func nameKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

type Voice struct {
	ID      int64
	Name    string
	Secs    float64
	Created time.Time
	Updated time.Time
	Embed   []float32
	HasClip bool
}

func (s *MeetingStore) Voices() ([]Voice, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(`
		SELECT id, name, secs, created_ns, updated_ns, embed, clip IS NOT NULL
		FROM voices ORDER BY name_key`)
	if err != nil {
		return nil, fmt.Errorf("history: reading voices: %w", err)
	}
	defer rows.Close()

	var out []Voice
	for rows.Next() {
		v, err := scanVoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanVoice(row rowScanner) (Voice, error) {
	var (
		v                    Voice
		createdNS, updatedNS int64
		embed                []byte
	)
	if err := row.Scan(&v.ID, &v.Name, &v.Secs, &createdNS, &updatedNS, &embed, &v.HasClip); err != nil {
		return Voice{}, fmt.Errorf("history: reading voice: %w", err)
	}
	v.Created = time.Unix(0, createdNS)
	v.Updated = time.Unix(0, updatedNS)
	v.Embed = DecodeVec(embed)
	return v, nil
}

// NameSpeaker gives one meeting's speaker a name, creating the voice if this
// is the first time that name is used and joining the existing one if it is
// not. Two people who genuinely share a first name are the user's problem to
// disambiguate ("Alex (design)"); the alternative -- silently keeping two
// voices with one name -- makes the Voices pane unreadable.
func (s *MeetingStore) NameSpeaker(speakerRow int64, name string) (Voice, error) {
	db, err := s.open()
	if err != nil {
		return Voice{}, err
	}
	if name == "" {
		return Voice{}, errors.New("history: a voice needs a name")
	}

	tx, err := db.sql.Begin()
	if err != nil {
		return Voice{}, fmt.Errorf("history: naming a voice: %w", err)
	}
	defer tx.Rollback()

	var voiceID int64
	err = tx.QueryRow("SELECT id FROM voices WHERE name_key = ?", nameKey(name)).Scan(&voiceID)
	if errors.Is(err, sql.ErrNoRows) {
		now := time.Now().UnixNano()
		res, err := tx.Exec(`
			INSERT INTO voices (name, name_key, created_ns, updated_ns, embed, secs)
			VALUES (?, ?, ?, ?, ?, 0)`, name, nameKey(name), now, now, []byte(nil))
		if err != nil {
			return Voice{}, fmt.Errorf("history: creating voice %q: %w", name, err)
		}
		if voiceID, err = res.LastInsertId(); err != nil {
			return Voice{}, fmt.Errorf("history: creating voice %q: %w", name, err)
		}
	} else if err != nil {
		return Voice{}, fmt.Errorf("history: naming a voice: %w", err)
	}

	if _, err := tx.Exec("UPDATE meeting_speakers SET voice_id = ? WHERE id = ?", voiceID, speakerRow); err != nil {
		return Voice{}, fmt.Errorf("history: linking speaker to voice: %w", err)
	}
	if err := recomputeVoice(tx, voiceID); err != nil {
		return Voice{}, err
	}

	v, err := scanVoice(tx.QueryRow(`
		SELECT id, name, secs, created_ns, updated_ns, embed, clip IS NOT NULL
		FROM voices WHERE id = ?`, voiceID))
	if err != nil {
		return Voice{}, err
	}
	return v, tx.Commit()
}

// LinkSpeaker attaches an already-known voice to another speaker -- what
// confirming one of the Voices pane's suggestions does.
func (s *MeetingStore) LinkSpeaker(speakerRow, voiceID int64) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("history: linking a voice: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("UPDATE meeting_speakers SET voice_id = ? WHERE id = ?", voiceID, speakerRow); err != nil {
		return fmt.Errorf("history: linking a voice: %w", err)
	}
	if err := recomputeVoice(tx, voiceID); err != nil {
		return err
	}
	return tx.Commit()
}

// UnlinkSpeaker says "that was not them" about one speaker, and rebuilds the
// voice's fingerprint without it. The correction has to reach the centroid,
// or the same wrong match keeps happening.
func (s *MeetingStore) UnlinkSpeaker(speakerRow int64) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("history: unlinking a voice: %w", err)
	}
	defer tx.Rollback()

	var voiceID sql.NullInt64
	if err := tx.QueryRow("SELECT voice_id FROM meeting_speakers WHERE id = ?", speakerRow).Scan(&voiceID); err != nil {
		return fmt.Errorf("history: unlinking a voice: %w", err)
	}
	if _, err := tx.Exec("UPDATE meeting_speakers SET voice_id = NULL WHERE id = ?", speakerRow); err != nil {
		return fmt.Errorf("history: unlinking a voice: %w", err)
	}
	if voiceID.Valid {
		if err := recomputeVoice(tx, voiceID.Int64); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// recomputeVoice rebuilds a voice's fingerprint from every speaker currently
// linked to it, weighted by how long each of them talked.
func recomputeVoice(tx *sql.Tx, voiceID int64) error {
	rows, err := tx.Query("SELECT embed, talk_secs FROM meeting_speakers WHERE voice_id = ?", voiceID)
	if err != nil {
		return fmt.Errorf("history: recomputing a voice: %w", err)
	}
	defer rows.Close()

	var (
		vecs    [][]float32
		weights []float64
		secs    float64
	)
	for rows.Next() {
		var (
			embed []byte
			talk  float64
		)
		if err := rows.Scan(&embed, &talk); err != nil {
			return fmt.Errorf("history: recomputing a voice: %w", err)
		}
		if v := DecodeVec(embed); len(v) > 0 {
			vecs = append(vecs, v)
			weights = append(weights, talk)
		}
		secs += talk
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	_, err = tx.Exec("UPDATE voices SET embed = ?, secs = ?, updated_ns = ? WHERE id = ?",
		EncodeVec(centroid(vecs, weights)), secs, time.Now().UnixNano(), voiceID)
	if err != nil {
		return fmt.Errorf("history: recomputing a voice: %w", err)
	}
	return nil
}

func (s *MeetingStore) RenameVoice(id int64, name string) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("history: a voice needs a name")
	}
	// Only the label changes. The fingerprint is the identity, and renaming
	// somebody does not make them sound different.
	res, err := db.sql.Exec("UPDATE voices SET name = ?, name_key = ?, updated_ns = ? WHERE id = ?",
		name, nameKey(name), time.Now().UnixNano(), id)
	if err != nil {
		return fmt.Errorf("history: renaming voice: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("history: no voice with id %d", id)
	}
	return nil
}

// ForgetVoice drops the name. Every speaker that carried it goes back to
// being an unnamed number (the foreign key is ON DELETE SET NULL), and their
// per-meeting fingerprints survive, so the same voice can be recognised and
// named again later. This is the reversible one.
func (s *MeetingStore) ForgetVoice(id int64) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	if _, err := db.sql.Exec("DELETE FROM voices WHERE id = ?", id); err != nil {
		return fmt.Errorf("history: forgetting voice: %w", err)
	}
	return nil
}

// EraseVoice is the other one, and the difference matters: it also destroys
// the fingerprints of every speaker that was this voice. That is the "delete
// what you learned about how I sound" action, and it must actually delete it
// rather than merely stop showing a name.
func (s *MeetingStore) EraseVoice(id int64) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	tx, err := db.sql.Begin()
	if err != nil {
		return fmt.Errorf("history: erasing voice: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec("UPDATE meeting_speakers SET embed = NULL WHERE voice_id = ?", id); err != nil {
		return fmt.Errorf("history: erasing voice: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM voices WHERE id = ?", id); err != nil {
		return fmt.Errorf("history: erasing voice: %w", err)
	}
	return tx.Commit()
}

// SetVoiceClip stores a couple of seconds of this voice as a small WAV, so
// the Voices pane can play a sample even after retention has swept the
// recording it was cut from. It is the only audio this database holds, and it
// is measured in tens of kilobytes.
func (s *MeetingStore) SetVoiceClip(id int64, wav []byte) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	if _, err := db.sql.Exec("UPDATE voices SET clip = ? WHERE id = ?", wav, id); err != nil {
		return fmt.Errorf("history: storing voice clip: %w", err)
	}
	return nil
}

func (s *MeetingStore) VoiceClip(id int64) ([]byte, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	var clip []byte
	err = db.sql.QueryRow("SELECT clip FROM voices WHERE id = ?", id).Scan(&clip)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("history: no voice with id %d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("history: reading voice clip: %w", err)
	}
	return clip, nil
}

// SpeakerCandidate is one unnamed speaker, offered up for naming: who they
// sound like, and where to hear them.
type SpeakerCandidate struct {
	SpeakerRow   int64
	MeetingStart time.Time
	Score        float32
	TalkSecs     float64
	// The longest thing they said, and where it is in which recording -- so
	// the pane can play a representative few seconds rather than the first
	// "mhm" of the call.
	Text      string
	Channel   int
	StartSecs float64
	EndSecs   float64
	AudioPath string
}

// SimilarUnnamed returns the unnamed speakers that sound most like embed,
// closest first. This is what turns naming a voice from a chore into one
// click: name it once, confirm the ten clips that are obviously the same
// person, and the profile is built.
//
// Brute force on purpose. Even a heavy user has a few thousand speaker rows
// of a few hundred bytes each; scoring all of them takes single-digit
// milliseconds, and a vector index would be a dependency, a schema and a
// rebuild step bought for nothing.
func (s *MeetingStore) SimilarUnnamed(embed []float32, limit int) ([]SpeakerCandidate, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	if len(embed) == 0 {
		return nil, nil
	}

	rows, err := db.sql.Query(unnamedSpeakerSQL)
	if err != nil {
		return nil, fmt.Errorf("history: comparing voices: %w", err)
	}
	defer rows.Close()

	var out []SpeakerCandidate
	for rows.Next() {
		c, vec, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		c.Score = Cosine(embed, vec)
		if c.Score < SuggestThreshold {
			continue
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// UnnamedSpeakers is the naming queue: the speakers nobody has identified,
// the ones who talked most first, because those are the people worth naming.
func (s *MeetingStore) UnnamedSpeakers(limit int) ([]SpeakerCandidate, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(unnamedSpeakerSQL+" ORDER BY ms.talk_secs DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("history: reading unnamed speakers: %w", err)
	}
	defer rows.Close()

	var out []SpeakerCandidate
	for rows.Next() {
		c, _, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// The longest turn stands in for the speaker: it is the most audio of them in
// one piece, which is both the best thing to listen to and the best thing to
// read when deciding who they are. Every query that hands a speaker to the
// window wants exactly this shape, so it is written once here and each caller
// adds its own WHERE.
const candidateSelectSQL = `
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
	)`

const unnamedSpeakerSQL = candidateSelectSQL + `
	WHERE ms.voice_id IS NULL AND ms.embed IS NOT NULL AND ms.local_id >= 0`

// SpeakersByVoice is the other direction: everything currently attached to
// one voice, longest-talking first. It is what the voice is actually made of
// -- the rows recomputeVoice averages -- so the window can play each of them,
// promote one to the stored sample, or say "that was not them" about it.
func (s *MeetingStore) SpeakersByVoice(voiceID int64) ([]SpeakerCandidate, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.Query(candidateSelectSQL+`
		WHERE ms.voice_id = ? ORDER BY ms.talk_secs DESC`, voiceID)
	if err != nil {
		return nil, fmt.Errorf("history: reading the speakers of voice %d: %w", voiceID, err)
	}
	defer rows.Close()

	var out []SpeakerCandidate
	for rows.Next() {
		c, _, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanCandidate(rows rowScanner) (SpeakerCandidate, []float32, error) {
	var (
		c                   SpeakerCandidate
		meetingNS           int64
		embed               []byte
		micPath, systemPath string
	)
	err := rows.Scan(&c.SpeakerRow, &meetingNS, &c.TalkSecs, &embed,
		&c.Text, &c.Channel, &c.StartSecs, &c.EndSecs, &micPath, &systemPath)
	if err != nil {
		return SpeakerCandidate{}, nil, fmt.Errorf("history: reading speakers: %w", err)
	}
	c.MeetingStart = time.Unix(0, meetingNS)
	c.AudioPath = micPath
	if c.Channel == ChannelSystem {
		c.AudioPath = systemPath
	}
	return c, DecodeVec(embed), nil
}

// IdentifySpeakers puts names to one meeting's speakers by comparing them
// with every voice already known, and returns the ones it was not sure enough
// about to decide -- those are what the Voices pane asks about.
//
// Run right after a meeting's turns are written, so a transcript opened a
// minute later already reads with names on it.
func (s *MeetingStore) IdentifySpeakers(start time.Time) ([]SpeakerSuggestion, error) {
	sure, unsure, err := s.matchSpeakers(start)
	if err != nil {
		return nil, err
	}
	for _, m := range sure {
		if err := s.LinkSpeaker(m.SpeakerRow, m.VoiceID); err != nil {
			return unsure, err
		}
	}
	return unsure, nil
}

// SpeakerSuggestions is IdentifySpeakers without the linking: the same
// "this might be Ірина" answers, for a meeting the window is showing right
// now. Read-only on purpose -- opening a meeting must not quietly rename
// anybody, and 0.55-0.72 is exactly the band where the app is not allowed to
// decide by itself.
func (s *MeetingStore) SpeakerSuggestions(start time.Time) ([]SpeakerSuggestion, error) {
	_, unsure, err := s.matchSpeakers(start)
	return unsure, err
}

// matchSpeakers scores every unnamed speaker of one meeting against every
// known voice and splits the answers at the two thresholds: sure enough to
// apply, and only worth asking about.
func (s *MeetingStore) matchSpeakers(start time.Time) (sure, unsure []SpeakerSuggestion, err error) {
	speakers, err := s.Speakers(start)
	if err != nil {
		return nil, nil, err
	}
	voices, err := s.Voices()
	if err != nil {
		return nil, nil, err
	}
	if len(voices) == 0 {
		return nil, nil, nil
	}

	for _, sp := range speakers {
		if sp.VoiceID != 0 || len(sp.Embed) == 0 {
			continue
		}
		best, bestScore := Voice{}, float32(0)
		for _, v := range voices {
			if score := Cosine(sp.Embed, v.Embed); score > bestScore {
				best, bestScore = v, score
			}
		}
		match := SpeakerSuggestion{SpeakerRow: sp.ID, VoiceID: best.ID, Name: best.Name, Score: bestScore}
		switch {
		case bestScore >= IdentifyThreshold:
			sure = append(sure, match)
		case bestScore >= SuggestThreshold:
			unsure = append(unsure, match)
		}
	}
	return sure, unsure, nil
}

// SpeakerSuggestion is "this might be Ірина, ask before you say so".
type SpeakerSuggestion struct {
	SpeakerRow int64
	VoiceID    int64
	Name       string
	Score      float32
}
