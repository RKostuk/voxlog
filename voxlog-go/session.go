package main

import (
	"log"
	"path/filepath"
	"sync"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/voiceprint"
)

// A session is one stretch of listening that turned into a file.
//
// Always-on does not decide what it is recording when it starts recording:
// two seconds of audio say nothing. It decides while the recording runs, on
// every reply the voice-activity gate hands it, and the answer can change
// exactly once -- from a note to a conversation. That is the shape the day
// actually has: a couple of remarks to yourself, and then somebody starts
// talking to you.
// trailingSilenceSeconds is how much of the pause after the last word is
// kept. Enough that nothing is clipped and the recording does not end the
// instant somebody stops talking, which sounds like a fault.
const trailingSilenceSeconds = 1.5

const (
	// sessionNote is one person thinking out loud. It ends up in History,
	// beside dictations, because that is what it is -- a dictation nobody
	// pressed a key for.
	sessionNote = "note"
	// sessionMeeting is a conversation: a second voice in the room, or the
	// far end of a call. Ends up in Meetings, with diarization and turns.
	sessionMeeting = "meeting"
)

const (
	// escalateSegments and escalateSeconds are what a second voice has to
	// clear before a note becomes a conversation.
	//
	// Deliberately steep, because the two mistakes are not symmetrical. A
	// conversation mistaken for a note is filed a minute later than it might
	// have been and shows up in History instead of Meetings. A note mistaken
	// for a conversation sits in an open file waiting for the five-minute
	// conversation gap, which from the outside looks exactly like the
	// feature not working -- which is what happened the first time this ran:
	// one person talking to themselves escalated after 27 seconds, with the
	// far end at digital silence.
	escalateSegments = 3
	escalateSeconds  = 8.0
	// distinctVoice is how unlike the first speaker a cluster has to be
	// before it counts as a second person. Stricter than
	// voiceprint.MergeThreshold (0.55), which answers a different question --
	// "are these two stretches the same person" inside a transcript, where
	// splitting one person in two is the worse failure. Here the cost is
	// reversed, so a voice only opens a conversation when it is plainly not
	// the one already talking.
	distinctVoice = 0.45
	// farEndEscalateSeconds is the same bar for the other side of a call.
	// Higher than a single reply because music with vocals clears the
	// voice-activity gate too, and a false conversation records the room for
	// as long as the album lasts.
	farEndEscalateSeconds = 10.0
)

// session is the state of one recording in progress. Always-on owns the
// microphone; this owns the files and the verdict.
type session struct {
	start    time.Time
	spec     asr.ModelSpec
	language string
	// auto is false for a session the user started by hand. Kept so one
	// close path serves both.
	auto bool

	// mu guards everything below: the microphone callback, the system-audio
	// callback and the supervisor all touch it.
	mu         sync.Mutex
	kind       string
	micWAV     *audio.WAVWriter
	sysWAV     *audio.WAVWriter
	micPath    string
	sysPath    string
	sysRunning bool
	sysVoiced  float64
	farEndSecs float64
	lastVoiced time.Time
	// confirmed is set the first time a reply passes the second stage. A
	// session is opened the moment the gate hears a voice, before anything
	// has been checked, so that the menu bar can say "recording" while the
	// first sentence is still being said -- and a session that never gets a
	// confirmation is thrown away whole, file and all.
	confirmed bool
	// stopTicker ends the goroutine that keeps the menu bar clock moving.
	stopTicker chan struct{}
	// lastVoicedSample is how far into the recording the last speech was,
	// in samples. The clock cannot answer that: a session runs on for the
	// whole silence gap after the last word, and the difference between
	// "when it stopped" and "when the talking stopped" is precisely the
	// silence that gets trimmed off the end.
	lastVoicedSample int64
	// voiced is how much of this recording the gate called speech, in
	// seconds. Not the same question as confirmed: this is "was anything
	// said here at all", which is what decides whether the file is worth
	// keeping, while confirmed is "do we know who said it".
	voiced float64
	// warned is set once the "tell them they are being recorded" banner has
	// been posted for this session. A session becomes a conversation exactly
	// once, but promote() is called on every far-end segment, so without this
	// the reminder would arrive over and over for as long as the other side
	// keeps talking.
	warned bool

	// voices are the distinct speakers heard so far, as running centroids.
	// This is the live version of what the diarizer does after the fact --
	// cheaper, because it only ever sees one reply at a time, and available
	// now, which is the whole point.
	voices []sessionVoice
}

type sessionVoice struct {
	centroid []float32
	segments int
	secs     float64
}

// sessionPaths builds the two file names for a session starting at at.
func sessionPaths(dir string, at time.Time) (mic, system string) {
	return meetingPaths(dir, at)
}

// newSession opens the microphone file and starts as a note. Nothing here
// decides what the recording is; noteVoice and noteFarEnd do that later.
func newSession(dir string, at time.Time, spec asr.ModelSpec, language string, auto bool) (*session, error) {
	micPath, sysPath := sessionPaths(dir, at)
	micWAV, err := audio.NewWAVWriter(micPath)
	if err != nil {
		return nil, err
	}
	return &session{
		start:      at,
		spec:       spec,
		language:   language,
		auto:       auto,
		kind:       sessionNote,
		micWAV:     micWAV,
		micPath:    micPath,
		sysPath:    sysPath,
		lastVoiced: at,
	}, nil
}

// writeMic appends microphone audio. It does NOT touch the silence timer:
// only noteVoice does, and only for a stretch the voice-activity gate has
// confirmed. Loudness is not speech -- a fan, a keyboard and a passing truck
// all clear any level threshold worth having, and a timer driven by them
// never expires, which is how a recording ends up three minutes long with
// nothing said in it.
func (s *session) writeMic(chunk []float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.micWAV == nil {
		return
	}
	if err := s.micWAV.Write(chunk); err != nil {
		log.Printf("session: writing microphone audio: %v", err)
	}
}

// writeSystem appends the other side of a call, if the tap is running for
// this session.
//
// sysVoiced still accumulates on level, because all it feeds is the "was
// there a second party at all" test at close time -- a much cruder question
// than "has the room gone quiet", which noteFarEnd answers from the gate.
func (s *session) writeSystem(chunk []float32, level float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if level >= systemVoicedThreshold {
		s.sysVoiced += float64(len(chunk)) / audio.SampleRate
	}
	if s.sysWAV == nil {
		return
	}
	if err := s.sysWAV.Write(chunk); err != nil {
		log.Printf("session: writing system audio: %v", err)
	}
}

// noteVoice folds one reply's fingerprint into the session's speakers and
// reports whether the session has just become a conversation.
//
// A nil embedding (no voice model on disk) still counts as speech; it simply
// cannot contribute to the "how many people" question, which is why the
// far-end signal below is the fallback when there is no extractor.
func (s *session) noteVoice(embed []float32, seconds float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastVoiced = time.Now()
	s.markVoicedHereLocked()
	s.confirmed = true
	if len(embed) == 0 {
		return false
	}
	embed = voiceprint.Normalize(embed)
	if embed == nil {
		return false
	}

	best, bestScore := -1, float32(0)
	for i, v := range s.voices {
		if score := voiceprint.Cosine(v.centroid, embed); score > bestScore {
			best, bestScore = i, score
		}
	}
	if best >= 0 && bestScore >= voiceprint.MergeThreshold {
		v := &s.voices[best]
		v.centroid = voiceprint.WeightedCentroid(
			[][]float32{v.centroid, embed}, []float64{v.secs, seconds})
		v.segments++
		v.secs += seconds
	} else {
		s.voices = append(s.voices, sessionVoice{centroid: embed, segments: 1, secs: seconds})
	}

	return s.escalateLocked()
}

// escalateLocked flips the session to a conversation if any voice other than
// the first has said enough, and sounds different enough, to be a real second
// person. Callers hold mu.
func (s *session) escalateLocked() bool {
	if s.kind == sessionMeeting || len(s.voices) < 2 {
		return false
	}
	// The first voice is whoever started talking -- usually the user. Every
	// other one is a candidate second party, and has to clear three bars:
	// enough replies, enough seconds, and enough distance from the first
	// voice. The third is what keeps one person recorded at two distances
	// from the microphone from becoming two people.
	first := s.voices[0].centroid
	for _, v := range s.voices[1:] {
		if v.segments < escalateSegments || v.secs < escalateSeconds {
			continue
		}
		sim := voiceprint.Cosine(first, v.centroid)
		if sim >= distinctVoice {
			continue
		}
		s.kind = sessionMeeting
		log.Printf("always-on: second voice after %.0fs over %d replies, %.2f similar to the first -- this is a conversation",
			v.secs, v.segments, sim)
		return true
	}
	return false
}

// noteFarEnd adds confirmed speech from the system-audio side and reports
// whether that just made this a conversation. Speech, not sound: the caller
// runs its own voice-activity gate over the tap, so a playing album does not
// turn the room into a meeting.
func (s *session) noteFarEnd(seconds float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.farEndSecs += seconds
	s.lastVoiced = time.Now()
	s.confirmed = true
	if s.kind == sessionMeeting || s.farEndSecs < farEndEscalateSeconds {
		return false
	}
	s.kind = sessionMeeting
	return true
}

// promote forces the session to a conversation, whatever it has heard. Used
// when the far end is known before a word is spoken.
func (s *session) promote() {
	s.mu.Lock()
	s.kind = sessionMeeting
	s.mu.Unlock()
}

// warnOnce reports whether this is the first time anybody has asked to warn
// about this session, and remembers that they did.
func (s *session) warnOnce() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warned {
		return false
	}
	s.warned = true
	return true
}

// heardVoice is the voice-activity gate saying somebody is talking, right
// now, in this recording. It moves the silence timer and nothing else.
//
// Separate from noteVoice because the two answer different questions. This
// one is "is the room still busy", and the gate is the whole authority on
// that. noteVoice is "who is talking", which costs a model call and has a
// loudness floor under it -- and letting that floor hold the silence timer
// is what cut recordings in half mid-sentence when the speaker was quiet or
// far from the microphone.
func (s *session) heardVoice() {
	s.mu.Lock()
	s.lastVoiced = time.Now()
	s.markVoicedHereLocked()
	s.mu.Unlock()
}

// markVoicedHereLocked notes that the audio written so far ends in speech.
// Callers hold mu.
func (s *session) markVoicedHereLocked() {
	if s.micWAV != nil {
		s.lastVoicedSample = s.micWAV.Samples()
	}
}

// addVoiced records another stretch the gate called speech.
func (s *session) addVoiced(seconds float64) {
	s.mu.Lock()
	s.voiced += seconds
	s.lastVoiced = time.Now()
	s.markVoicedHereLocked()
	s.mu.Unlock()
}

// voicedSeconds is how much speech this recording is known to contain.
func (s *session) voicedSeconds() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.voiced
}

// isConfirmed reports whether anything in this recording has passed the
// second stage yet.
func (s *session) isConfirmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confirmed
}

func (s *session) currentKind() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kind
}

// quietFor is how long nothing has been said, on either side.
func (s *session) quietFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastVoiced)
}

// attachSystem hands the session the system-audio file to write into.
func (s *session) attachSystem(w *audio.WAVWriter, path string, running bool) {
	s.mu.Lock()
	s.sysWAV, s.sysPath, s.sysRunning = w, path, running
	s.mu.Unlock()
}

// closeFiles flushes and closes both writers and reports what was recorded.
func (s *session) closeFiles() (micPath, sysPath string, sysVoiced float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	written := int64(0)
	if s.micWAV != nil {
		written = s.micWAV.Samples()
		if err := s.micWAV.Close(); err != nil {
			log.Printf("session: closing the microphone recording: %v", err)
		}
		s.micWAV = nil
	}
	if s.sysWAV != nil {
		if err := s.sysWAV.Close(); err != nil {
			log.Printf("session: closing the system recording: %v", err)
		}
		s.sysWAV = nil
	} else {
		// Nothing was ever written to the far-end file, so there is no file.
		s.sysPath = ""
	}

	s.trimTrailingSilenceLocked(written)
	return s.micPath, s.sysPath, s.sysVoiced
}

// trimTrailingSilenceLocked cuts both tracks back to shortly after the last
// thing said in them.
//
// A recording runs on until the room has been quiet for the whole silence
// gap -- twenty seconds for a note, minutes for a call -- and all of that is
// an empty room. Keeping it costs disk, makes playback sit through nothing,
// and is audio the recognizer has to be protected from.
//
// Both tracks are cut at the same sample. transcribeFilesTurns pairs them
// positionally, so cutting one further than the other would put the far end
// out of step with the microphone for the whole recording.
func (s *session) trimTrailingSilenceLocked(written int64) {
	if s.lastVoicedSample <= 0 || written <= 0 {
		return
	}
	keep := s.lastVoicedSample + int64(trailingSilenceSeconds*audio.SampleRate)
	if keep >= written {
		return
	}
	for _, path := range []string{s.micPath, s.sysPath} {
		if path == "" {
			continue
		}
		if err := audio.TruncateWAV(path, keep); err != nil {
			log.Printf("session: trimming %s: %v", path, err)
		}
	}
}

// paths is what a retention sweep must not delete while this is open.
func (s *session) paths() (mic, system string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.micPath, s.sysPath
}

// sessionDir is where session audio lands: the same folder meetings already
// use, so one retention sweep and one backup cover everything recorded.
func (a *app) sessionDir() string {
	return filepath.Join(a.hist.Dir(), meetingsDirName)
}
