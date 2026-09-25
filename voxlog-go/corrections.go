package main

import (
	"log"
	"time"

	"voxlog-go/internal/diarize"
	"voxlog-go/internal/history"
)

// Rebuilding a speaker's fingerprint after a person has corrected who they
// are.
//
// This is the half of a correction that makes it teach rather than only
// repair. Moving a reply changes what a speaker of this meeting sounds like,
// which changes the voice that speaker belongs to, which is what every later
// meeting is recognised against. Without it a correction would fix one page
// and the same mistake would be made again on the next recording.
//
// It lives here rather than in internal/history because describing a voice
// means running the embedding model, and the storage layer is deliberately
// free of that dependency.

// refreshSpeakerEmbeds recomputes the fingerprint of every speaker of one
// meeting from the replies they now hold, and with it the centroid of every
// voice those speakers belong to.
//
// Best effort by design: the transcript is what the user asked to fix, and it
// is already fixed by the time this runs. A recording deleted under a kept
// transcript, or models not yet downloaded, costs the improvement to future
// recognition and nothing else.
func (a *app) refreshSpeakerEmbeds(start time.Time) {
	m, err := a.meetings.Get(start)
	if err != nil || m.SystemAudioPath == "" {
		return
	}
	e := a.embedders.get(a.modelsDir)
	if e == nil {
		return
	}

	bySpeaker, err := a.meetings.SpeakerTurns(start)
	if err != nil {
		log.Printf("corrections: reading replies: %v", err)
		return
	}

	for row, turns := range bySpeaker {
		segments := make([]diarize.Segment, 0, len(turns))
		for _, t := range turns {
			// The microphone side is the user by definition and was never
			// fingerprinted; a correction does not change that.
			if t.Channel != history.ChannelSystem {
				continue
			}
			segments = append(segments, diarize.Segment{Start: float32(t.StartSecs), End: float32(t.EndSecs)})
		}
		if len(segments) == 0 {
			continue
		}
		embed := diarize.FingerprintFile(e, m.SystemAudioPath, segments)
		if len(embed) == 0 {
			continue
		}
		if err := a.meetings.SetSpeakerEmbed(row, embed); err != nil {
			log.Printf("corrections: storing a fingerprint: %v", err)
		}
	}
}
