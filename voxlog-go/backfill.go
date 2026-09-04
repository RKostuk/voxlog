package main

import (
	"log"
	"os"
	"time"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/history"
	"voxlog-go/internal/settings"
	"voxlog-go/internal/ui"
)

// Re-reading meetings recorded before per-speaker replies existed.
//
// Their transcripts are a single string with no timings in it, so nothing in
// them can be played back, counted, or attributed to anybody. The audio is
// still on disk, so the information is recoverable -- it just costs a decode
// per meeting, and that is CPU the user did not ask to spend right now.
//
// Everything here is therefore about staying out of the way: one meeting at a
// time, only when nothing else is decoding, abandoned the moment a recording
// starts, and idle by default.

const (
	// backfillStartDelay leaves the first minute after launch alone: the ASR
	// model is warming up and the user's first hotkey lands somewhere in it.
	backfillStartDelay = 60 * time.Second
	// backfillPoll is how often "is the machine free?" is re-asked.
	backfillPoll = 20 * time.Second
	// backfillRest is a duty cycle, expressed as a multiple of how long the
	// last decode took: work for one unit, rest for two. It is what keeps a
	// hundred old meetings from pinning every core for an afternoon.
	backfillRest    = 2
	backfillMaxRest = 5 * time.Minute
	// backfillAttempts is how many times one meeting may be tried before it
	// is left alone. A recording that reliably kills the decoder must not
	// become an infinite loop on every launch.
	backfillAttempts = 3
)

// startBackfill runs the re-read loop for as long as the app is up. Started
// once, from onReady, after the migrations have run.
func (a *app) startBackfill() {
	go func() {
		time.Sleep(backfillStartDelay)
		for {
			rest := a.backfillOnce()
			time.Sleep(rest)
		}
	}()
}

// backfillOnce re-reads at most one meeting and reports how long to wait
// before considering another.
func (a *app) backfillOnce() time.Duration {
	mode := a.store.Get().BackfillTurns
	if mode == settings.BackfillManual {
		return backfillPoll
	}
	// Idle means idle: not while a meeting or a dictation is being recorded,
	// and not while anything is waiting in the decode queue. Startup skips
	// only the second check, so it catches up sooner but still never races a
	// take the user is waiting for.
	if a.queue.outstanding() > 0 {
		return backfillPoll
	}
	if mode != settings.BackfillStartup && a.recordingInProgress() {
		return backfillPoll
	}

	m, ok, err := a.meetings.NeedsTurns(turnsSchemaVersion, backfillAttempts)
	if err != nil {
		log.Printf("backfill: %v", err)
		return backfillPoll
	}
	if !ok {
		// Nothing left to do. Keep checking, cheaply: a meeting decoded
		// today by an older version will show up here after an upgrade.
		return 10 * time.Minute
	}

	// Audio swept by retention is not a failure to retry -- nothing about it
	// will be different next time, and leaving it in the queue would hide the
	// meetings that can still be recovered.
	if _, err := os.Stat(m.AudioPath); err != nil {
		if err := a.meetings.GiveUpOnTurns(m.Start, turnsSchemaVersion, "the recording is no longer on disk"); err != nil {
			log.Printf("backfill: %v", err)
		}
		return time.Second
	}

	took := a.backfillMeeting(m)
	rest := time.Duration(backfillRest) * took
	if rest > backfillMaxRest {
		rest = backfillMaxRest
	}
	if rest < backfillPoll {
		rest = backfillPoll
	}
	return rest
}

// backfillMeeting re-decodes one meeting for its turns and returns how long
// that took.
//
// Deliberately NOT transcribeMeetingEntry: this must not notify, must not
// re-run the LLM over a transcript that already has a summary and tasks
// hanging off it, and must not delete audio the user chose to keep. It adds
// structure to a meeting that already has text; it does not redo the meeting.
func (a *app) backfillMeeting(m history.Meeting) time.Duration {
	cfg := a.store.Get()
	// modelForSettings rather than a.model: this must never notify or open
	// the settings window. A backlog task noticing that no model is selected
	// is not news the user needs interrupting for.
	spec, ok := modelForSettings(cfg)
	if !ok || !asr.IsDownloaded(a.modelsDir, spec) {
		return 0
	}
	language := cfg.Language
	separate := cfg.SeparateSpeakers && m.SystemAudioPath != ""

	// Counted before the work, not after: a recording that takes the decoder
	// down with it still has to use up an attempt, or it is retried forever.
	if err := a.meetings.NoteBackfillAttempt(m.Start); err != nil {
		log.Printf("backfill: %v", err)
		return 0
	}

	stop := a.backfillStopChan()
	started := time.Now()
	done := make(chan struct{})

	// Submitted with the recording's own length, so the queue's existing
	// shortest-first ordering lets any fresh dictation past it, and the yield
	// between blocks lets one interrupt it mid-decode. No new priority
	// mechanism is needed.
	a.queue.submit(m.RecordingSeconds, func(yield func()) {
		defer close(done)

		turns, err := a.transcribeFilesTurns(spec, language, m.AudioPath, m.SystemAudioPath, separate, yield, stop)
		if err != nil {
			log.Printf("backfill: %s: %v", m.Start.Format(time.RFC3339), err)
			return
		}
		if len(turns) == 0 {
			if err := a.meetings.GiveUpOnTurns(m.Start, turnsSchemaVersion, "nothing could be transcribed"); err != nil {
				log.Printf("backfill: %v", err)
			}
			return
		}
		linkSpeakers(turns)

		if err := a.meetings.ReplaceTurns(m.Start, meetingSpeakers(turns), historyTurns(turns), turnsSchemaVersion); err != nil {
			log.Printf("backfill: saving turns: %v", err)
			return
		}
		if _, err := a.meetings.IdentifySpeakers(m.Start); err != nil {
			log.Printf("backfill: identifying speakers: %v", err)
		}

		// The existing transcript stands unless there was none. It already
		// has a summary and possibly tasks pointing at it, and replacing it
		// with a fresh decode would orphan both for no gain.
		if m.Text == "" {
			text := renderTurns(turns, nil)
			if err := a.meetings.Update(m.Start, func(e *history.Meeting) { e.Text = text }); err != nil {
				log.Printf("backfill: attaching the transcript: %v", err)
			}
		}
		log.Printf("backfill: re-read %s (%d replies)", m.Start.Format(time.RFC3339), len(turns))
		ui.RefreshMainWindowIfOpen(a.hist, a.meetings, a.tasks)
	})

	<-done
	return time.Since(started)
}

// recordingInProgress is true while the user is dictating or in a call. The
// backlog gives the machine back for both.
func (a *app) recordingInProgress() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.meeting != nil || a.dictation != nil
}

// backfillStopChan hands out the channel a running re-read watches. Closing
// it (see stopBackfillLocked) unwinds the decode between blocks, within
// seconds, and nothing is written -- the meeting is simply picked up later.
func (a *app) backfillStopChan() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.backfillStop == nil {
		a.backfillStop = make(chan struct{})
	}
	return a.backfillStop
}

// stopBackfillLocked is called the moment a recording starts. The machine is
// the user's; a backlog task must never be the reason a dictation decodes
// slowly.
//
// Locked, i.e. a.mu is ALREADY held by the caller: startDictation and
// startMeeting both run under it (their comments say so), and a.mu is a plain
// sync.Mutex, so taking it again here deadlocks the hotkey path outright --
// which is exactly what it did.
func (a *app) stopBackfillLocked() {
	if a.backfillStop != nil {
		close(a.backfillStop)
		a.backfillStop = nil
	}
}
