package main

import (
	"log"
	"path/filepath"
	"sync"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/voiceid"
)

// A session is one stretch of listening that turned into a file.
//
// Always-on does not decide what it is recording when it starts recording:
// two seconds of audio say nothing. It decides while the recording runs, on
// every reply the voice-activity gate hands it, and the answer can change
// exactly once -- from a note to a conversation. That is the shape the day
// actually has: a couple of remarks to yourself, and then somebody starts
// talking to you.
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
	// clear before a note becomes a conversation. One stray reply is a cough,
	// a passer-by, or a phrase from a video; two replies and three seconds of
	// them is somebody talking.
	escalateSegments = 2
	escalateSeconds  = 3.0
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
	if len(embed) == 0 {
		return false
	}
	embed = voiceid.Normalize(embed)
	if embed == nil {
		return false
	}

	best, bestScore := -1, float32(0)
	for i, v := range s.voices {
		if score := voiceid.Cosine(v.centroid, embed); score > bestScore {
			best, bestScore = i, score
		}
	}
	if best >= 0 && bestScore >= voiceid.MergeThreshold {
		v := &s.voices[best]
		v.centroid = voiceid.WeightedCentroid(
			[][]float32{v.centroid, embed}, []float64{v.secs, seconds})
		v.segments++
		v.secs += seconds
	} else {
		s.voices = append(s.voices, sessionVoice{centroid: embed, segments: 1, secs: seconds})
	}

	return s.escalateLocked()
}

// escalateLocked flips the session to a conversation if any voice other than
// the first has said enough to be a real second person. Callers hold mu.
func (s *session) escalateLocked() bool {
	if s.kind == sessionMeeting || len(s.voices) < 2 {
		return false
	}
	// The first voice is whoever started talking -- usually the user. Every
	// other one is a candidate second party; one of them clearing the bar is
	// enough.
	for _, v := range s.voices[1:] {
		if v.segments >= escalateSegments && v.secs >= escalateSeconds {
			s.kind = sessionMeeting
			return true
		}
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

	if s.micWAV != nil {
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
	return s.micPath, s.sysPath, s.sysVoiced
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
