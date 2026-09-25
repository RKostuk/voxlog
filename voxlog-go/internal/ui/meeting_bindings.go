package ui

import (
	"errors"
	"fmt"
	"time"

	webview "github.com/webview/webview_go"

	"voxlog-go/internal/audio"
	"voxlog-go/internal/history"
	"voxlog-go/internal/task"
)

// What the meeting screen asks Go for once the user opens one meeting, plus
// everything the Voices pane does.
//
// None of it rides along in the page payload: an hour-long call is thousands
// of turns, and that payload is rebuilt on every refresh (see
// RefreshMainWindowIfOpen). Speaker totals are cheap and do travel with the
// list -- they draw the talk-time bar on every row -- but the replies
// themselves are fetched when a meeting is actually opened.

type turnJSON struct {
	Seq     int     `json:"seq"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Channel int     `json:"channel"`
	// LocalID is the speaker's number within this meeting; -1 is the user,
	// -2 is "the two sides were mixed before the recognizer saw them".
	LocalID int    `json:"speaker"`
	Name    string `json:"name,omitempty"`
	Text    string `json:"text"`
	// Corrected marks a reply a person has said belongs to someone in
	// particular, so the transcript can show what was decided by hand apart
	// from what the pipeline guessed.
	Corrected bool `json:"corrected,omitempty"`
}

// searchHitJSON is one search result: enough to draw the row and to open
// the meeting scrolled to the reply that matched.
type searchHitJSON struct {
	MeetingID string  `json:"meeting_id"`
	Day       string  `json:"day"`
	Time      string  `json:"time"`
	Seq       int     `json:"seq"`
	Start     float64 `json:"start"`
	LocalID   int     `json:"speaker"`
	Name      string  `json:"name,omitempty"`
	Text      string  `json:"text"`
	// Snippet marks the matched words with [[ ]] rather than HTML: the page
	// escapes everything it draws, and this is not the one exception.
	Snippet string `json:"snippet"`
}

// personJSON is one voice across every meeting it was heard in.
type personJSON struct {
	VoiceID   int64   `json:"voice_id"`
	Name      string  `json:"name"`
	Meetings  int     `json:"meetings"`
	TalkSecs  float64 `json:"talk_secs"`
	TurnCount int     `json:"turn_count"`
	LastSeen  string  `json:"last_seen"`
}

type speakerJSON struct {
	// Row is the speaker's database id, and what naming a voice acts on.
	Row       int64   `json:"row"`
	LocalID   int     `json:"speaker"`
	Name      string  `json:"name,omitempty"`
	VoiceID   int64   `json:"voice_id,omitempty"`
	TalkSecs  float64 `json:"talk_secs"`
	TurnCount int     `json:"turn_count"`
	// Identified says the speaker has a voice fingerprint, i.e. that naming
	// them is possible at all. Without one the Voices pane can still show the
	// speaker, but not recognise them anywhere else.
	Identified bool `json:"identified"`
}

type voiceJSON struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Secs    float64 `json:"secs"`
	HasClip bool    `json:"has_clip"`
}

// suggestionJSON is "this might be Ірина, ask before you say so": a match
// Go found in the 0.55-0.72 band, which it is not allowed to apply by itself.
type suggestionJSON struct {
	Row     int64   `json:"row"`
	VoiceID int64   `json:"voice_id"`
	Name    string  `json:"name"`
	Score   float64 `json:"score"`
}

type candidateJSON struct {
	Row       int64   `json:"row"`
	Meeting   string  `json:"meeting"`
	Day       string  `json:"day"`
	Score     float64 `json:"score,omitempty"`
	TalkSecs  float64 `json:"talk_secs"`
	Text      string  `json:"text"`
	Audio     string  `json:"audio"`
	Start     float64 `json:"start"`
	End       float64 `json:"end"`
	Suggested string  `json:"suggested,omitempty"`
	VoiceID   int64   `json:"suggested_voice_id,omitempty"`
}

func turnsJSON(turns []history.Turn, corrections []history.Correction) []turnJSON {
	out := make([]turnJSON, 0, len(turns))
	for _, t := range turns {
		out = append(out, turnJSON{
			Seq:       t.Seq,
			Start:     t.StartSecs,
			End:       t.EndSecs,
			Channel:   t.Channel,
			LocalID:   t.LocalID,
			Name:      t.Name,
			Text:      t.Text,
			Corrected: correctedBy(corrections, t),
		})
	}
	return out
}

// correctedBy reports whether a correction claims this reply -- the same
// midpoint rule the store applies them by, so the mark in the transcript and
// the assignment behind it can never disagree.
func correctedBy(corrections []history.Correction, t history.Turn) bool {
	mid := (t.StartSecs + t.EndSecs) / 2
	for _, c := range corrections {
		if c.StartSecs <= mid && mid <= c.EndSecs {
			return true
		}
	}
	return false
}

func speakersJSON(speakers []history.MeetingSpeaker) []speakerJSON {
	out := make([]speakerJSON, 0, len(speakers))
	for _, sp := range speakers {
		out = append(out, speakerJSON{
			Row:        sp.ID,
			LocalID:    sp.LocalID,
			Name:       sp.Name,
			VoiceID:    sp.VoiceID,
			TalkSecs:   sp.TalkSecs,
			TurnCount:  sp.TurnCount,
			Identified: len(sp.Embed) > 0,
		})
	}
	return out
}

func candidateJSONOf(c history.SpeakerCandidate) candidateJSON {
	return candidateJSON{
		Row:      c.SpeakerRow,
		Meeting:  c.MeetingStart.Format(time.RFC3339Nano),
		Day:      c.MeetingStart.Format("2006-01-02 15:04"),
		Score:    float64(c.Score),
		TalkSecs: c.TalkSecs,
		Text:     c.Text,
		Audio:    recordingName(c.AudioPath),
		Start:    c.StartSecs,
		End:      c.EndSecs,
	}
}

// bindMeetings registers everything the meeting screen and the Voices pane
// call. Split out of runMainWindow, which is long enough already.
func bindMeetings(w webview.WebView, store *history.Store, meetings *history.MeetingStore, tasks *task.Store) {
	// meetingDetail is what opening a meeting fetches: its replies, who spoke
	// them, and how long each person talked.
	w.Bind("meetingDetail", func(id string) (map[string]any, error) {
		at, err := time.Parse(time.RFC3339Nano, id)
		if err != nil {
			return nil, err
		}
		turns, err := meetings.Turns(at)
		if err != nil {
			return nil, err
		}
		speakers, err := meetings.Speakers(at)
		if err != nil {
			return nil, err
		}
		corrections, err := meetings.Corrections(at)
		if err != nil {
			return nil, err
		}
		// Who the unnamed speakers might be. Computed here rather than
		// fetched separately: the page draws them on the same rows, and
		// SpeakerSuggestions is the read-only half of the matching that
		// already ran when this meeting was decoded.
		unsure, err := meetings.SpeakerSuggestions(at)
		if err != nil {
			return nil, err
		}
		suggestions := make([]suggestionJSON, 0, len(unsure))
		for _, u := range unsure {
			suggestions = append(suggestions, suggestionJSON{
				Row: u.SpeakerRow, VoiceID: u.VoiceID, Name: u.Name, Score: float64(u.Score),
			})
		}
		return map[string]any{
			"turns":       turnsJSON(turns, corrections),
			"speakers":    speakersJSON(speakers),
			"suggestions": suggestions,
		}, nil
	})

	// searchMeetings searches every meeting's turns at once -- the pane's own
	// search box only ever looked inside the meeting that was open. Called
	// per keystroke, so it returns nothing for an empty query rather than
	// every turn ever recorded.
	w.Bind("searchMeetings", func(query string) ([]searchHitJSON, error) {
		hits, err := meetings.SearchTurns(query)
		if err != nil {
			return nil, err
		}
		out := make([]searchHitJSON, 0, len(hits))
		for _, h := range hits {
			out = append(out, searchHitJSON{
				MeetingID: h.Start.Format(time.RFC3339Nano),
				Day:       h.Start.Format("2006-01-02"),
				Time:      h.Start.Format("15:04"),
				Seq:       h.Seq,
				Start:     h.StartSecs,
				LocalID:   h.LocalID,
				Name:      h.Name,
				Text:      h.Text,
				Snippet:   h.Snippet,
			})
		}
		return out, nil
	})

	// peopleStats is the cross-meeting view of who the user actually talks
	// to. One query over meeting_speakers grouped by voice -- the reason
	// speakers are their own rows rather than a column on turns.
	w.Bind("peopleStats", func() ([]personJSON, error) {
		people, err := meetings.People()
		if err != nil {
			return nil, err
		}
		out := make([]personJSON, 0, len(people))
		for _, p := range people {
			out = append(out, personJSON{
				VoiceID:   p.VoiceID,
				Name:      p.Name,
				Meetings:  p.Meetings,
				TalkSecs:  p.TalkSecs,
				TurnCount: p.TurnCount,
				LastSeen:  p.LastSeen.Format("2006-01-02"),
			})
		}
		return out, nil
	})

	// setMeetingEntity files a meeting under a project by hand. Until now a
	// meeting only had a project if Task Hub happened to find a task in it,
	// which left the project filter blind to most of them.
	w.Bind("setMeetingEntity", func(id, entity string) error {
		at, err := time.Parse(time.RFC3339Nano, id)
		if err != nil {
			return err
		}
		if err := meetings.SetEntity(at, entity); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	w.Bind("voiceList", func() (map[string]any, error) {
		voices, err := meetings.Voices()
		if err != nil {
			return nil, err
		}
		out := make([]voiceJSON, 0, len(voices))
		for _, v := range voices {
			out = append(out, voiceJSON{ID: v.ID, Name: v.Name, Secs: v.Secs, HasClip: v.HasClip})
		}

		// The queue is the speakers nobody has identified, the ones who
		// talked most first: those are the people worth naming.
		unnamed, err := meetings.UnnamedSpeakers(12)
		if err != nil {
			return nil, err
		}
		queue := make([]candidateJSON, 0, len(unnamed))
		for _, c := range unnamed {
			queue = append(queue, candidateJSONOf(c))
		}
		return map[string]any{"voices": out, "unnamed": queue}, nil
	})

	// similarUnnamed answers "who else sounds like this?" -- the question
	// that turns naming a voice from a chore into one click.
	w.Bind("similarUnnamed", func(row int64) ([]candidateJSON, error) {
		sp, err := meetings.Speaker(row)
		if err != nil {
			return nil, err
		}
		similar, err := meetings.SimilarUnnamed(sp.Embed, 10)
		if err != nil {
			return nil, err
		}
		out := make([]candidateJSON, 0, len(similar))
		for _, c := range similar {
			if c.SpeakerRow == row {
				continue // the speaker being named is not a suggestion
			}
			out = append(out, candidateJSONOf(c))
		}
		return out, nil
	})

	// nameSpeaker names one speaker, and optionally everyone the user
	// confirmed sounds like them in the same breath.
	w.Bind("nameSpeaker", func(row int64, name string, alsoRows []int64) error {
		v, err := meetings.NameSpeaker(row, name)
		if err != nil {
			return err
		}
		for _, other := range alsoRows {
			if err := meetings.LinkSpeaker(other, v.ID); err != nil {
				return err
			}
		}
		// A sample of the voice, kept in the database so the Voices pane can
		// still play it after retention sweeps the recording it came from.
		// Best effort: a voice with no sample is a smaller loss than a naming
		// action that fails because a WAV has already been deleted.
		if !v.HasClip {
			if clip := clipFor(meetings, row); clip != nil {
				if err := meetings.SetVoiceClip(v.ID, clip); err != nil {
					return err
				}
			}
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// correctTurn is the transcript's own repair: this reply was not them.
	//
	// It reaches the fingerprints, not just the label. A correction that only
	// fixed the page would leave the next recording to make the same mistake,
	// which is the thing the user is trying to stop.
	w.Bind("correctTurn", func(id string, seq int, voiceID int64) error {
		at, err := time.Parse(time.RFC3339Nano, id)
		if err != nil {
			return err
		}
		if _, err := meetings.CorrectTurn(at, seq, voiceID); err != nil {
			return err
		}
		refingerprint(at)
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// correctTurnAs names a new person and gives them the reply in one action:
	// the picker's "Someone else...". A correction always points at a voice,
	// so the voice has to exist first.
	w.Bind("correctTurnAs", func(id string, seq int, name string) error {
		at, err := time.Parse(time.RFC3339Nano, id)
		if err != nil {
			return err
		}
		v, err := meetings.CreateVoice(name)
		if err != nil {
			return err
		}
		if _, err := meetings.CorrectTurn(at, seq, v.ID); err != nil {
			return err
		}
		refingerprint(at)
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// clearCorrection hands a reply back to the pipeline.
	w.Bind("clearCorrection", func(id string, seq int) error {
		at, err := time.Parse(time.RFC3339Nano, id)
		if err != nil {
			return err
		}
		if err := meetings.ClearCorrection(at, seq); err != nil {
			return err
		}
		refingerprint(at)
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// mergeSpeakers is the other half of the same repair, for the mistake that
	// goes the other way: one person the pipeline split in two.
	w.Bind("mergeSpeakers", func(id string, from, into int64) error {
		at, err := time.Parse(time.RFC3339Nano, id)
		if err != nil {
			return err
		}
		if err := meetings.MergeSpeakers(from, into); err != nil {
			return err
		}
		refingerprint(at)
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	w.Bind("linkSpeaker", func(row, voiceID int64) error {
		if err := meetings.LinkSpeaker(row, voiceID); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// unlinkSpeaker is "that was not them", and it has to reach the voice's
	// fingerprint, not just the label -- otherwise the same wrong match keeps
	// happening.
	w.Bind("unlinkSpeaker", func(row int64) error {
		if err := meetings.UnlinkSpeaker(row); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// voiceClips is what a voice is made of: every speaker row linked to it,
	// each with the audio to play. The Voices pane draws one row per clip so
	// the user can hear what they attached, promote one to the sample, or
	// take a wrong one back out.
	w.Bind("voiceClips", func(id int64) ([]candidateJSON, error) {
		clips, err := meetings.SpeakersByVoice(id)
		if err != nil {
			return nil, err
		}
		out := make([]candidateJSON, 0, len(clips))
		for _, c := range clips {
			out = append(out, candidateJSONOf(c))
		}
		return out, nil
	})

	// similarToVoice is similarUnnamed asked about a voice rather than a
	// speaker: "who else on file sounds like this person?" -- the question
	// behind adding more audio to somebody already named.
	w.Bind("similarToVoice", func(id int64) ([]candidateJSON, error) {
		voices, err := meetings.Voices()
		if err != nil {
			return nil, err
		}
		for _, v := range voices {
			if v.ID != id {
				continue
			}
			similar, err := meetings.SimilarUnnamed(v.Embed, 10)
			if err != nil {
				return nil, err
			}
			out := make([]candidateJSON, 0, len(similar))
			for _, c := range similar {
				out = append(out, candidateJSONOf(c))
			}
			return out, nil
		}
		return nil, fmt.Errorf("no voice with id %d", id)
	})

	// setVoiceSample replaces the couple of seconds the Voices pane plays for
	// a voice. Until now the sample was cut once, when the voice was first
	// named, and never again -- so the row played whatever the first clip
	// happened to be, however unrepresentative.
	w.Bind("setVoiceSample", func(id, row int64) error {
		clip := clipFor(meetings, row)
		if clip == nil {
			return errors.New("that recording is no longer on disk, so it cannot be the sample")
		}
		if err := meetings.SetVoiceClip(id, clip); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	w.Bind("renameVoice", func(id int64, name string) error {
		if err := meetings.RenameVoice(id, name); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	// forgetVoice drops the name and leaves the fingerprints, so the voice
	// can be recognised and named again; eraseVoice destroys them. Two
	// bindings rather than a flag, because they are two different promises to
	// the user and the UI must not be able to confuse them.
	w.Bind("forgetVoice", func(id int64) error {
		if err := meetings.ForgetVoice(id); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})

	w.Bind("eraseVoice", func(id int64) error {
		if err := meetings.EraseVoice(id); err != nil {
			return err
		}
		RefreshMainWindowIfOpen(store, meetings, tasks)
		return nil
	})
}

// clipFor cuts a couple of seconds out of a speaker's longest reply. Returns
// nil whenever the audio is no longer there, which is normal and not an
// error: a meeting recorded a month ago may well have been swept.
func clipFor(meetings *history.MeetingStore, row int64) []byte {
	c, err := meetings.SpeakerClipSource(row)
	if err != nil || c.AudioPath == "" {
		return nil
	}
	end := c.StartSecs + voiceClipSeconds
	if end > c.EndSecs {
		end = c.EndSecs
	}
	samples, err := audio.ReadRange(c.AudioPath, c.StartSecs, end)
	if err != nil || len(samples) == 0 {
		return nil
	}
	return audio.EncodeWAV(samples)
}

// voiceClipSeconds is how much of a voice is kept as a sample: enough to
// recognise somebody, small enough that a hundred of them are a few megabytes.
const voiceClipSeconds = 2.5

// errNoSuchVoice keeps the loopback server's 404 for a missing clip distinct
// from an actual read failure.
var errNoSuchVoice = errors.New("no such voice")

func voiceClipHandler(meetings *history.MeetingStore, id int64) ([]byte, error) {
	clip, err := meetings.VoiceClip(id)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoSuchVoice, err)
	}
	if len(clip) == 0 {
		return nil, errNoSuchVoice
	}
	return clip, nil
}
