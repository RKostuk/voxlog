package main

import (
	"errors"
	"log"
	"sync"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/diarize"
	"voxlog-go/internal/history"
	"voxlog-go/internal/voiceid"
	"voxlog-go/internal/voiceprint"
)

// Telling the speakers of a meeting apart.
//
// A recording is diarized once, whole, before any of it is decoded -- see
// diarizeRecording. That ordering is the substance of this file: the diarizer
// is reliable over what it is shown and blind beyond it, so asking it a minute
// at a time meant asking it to tell three people apart while it could hear
// one, and then renumbering them for the next minute. Measured on a 5-minute
// call, one pass over the whole recording found three speakers where sixty
// seconds at a time found nine, which were then merged back down to one.
//
// What remains here is the older per-block path, still used when the
// diarization models have not been downloaded yet, and the pieces that turn
// linked turns into what the meeting screen and the database store.

// turnsSchemaVersion is the version of everything above: how turns are cut,
// how their times are computed, how speakers are linked. A meeting's stored
// turns carry the version they were produced by, so raising this number is
// what asks the background backfill to run every old meeting through the
// improved pipeline -- and leaving it alone is what stops it re-decoding
// hours of audio for nothing.
const turnsSchemaVersion = 3

// mixedSpeaker marks a turn nobody can be credited with: the two sides were
// mixed together before the recognizer saw them, which is what happens when
// the user has speaker separation off. It still has timings, so it still
// plays back -- it just does not claim to know who spoke.
const mixedSpeaker = -2

// errStopped unwinds a decode that has been asked to give the machine back,
// which is how the background backfill gets out of the way the moment the
// user starts recording. Nothing is written when it comes back.
var errStopped = errors.New("transcription stopped")

// embedderCache holds the loaded embedding model. It mirrors diarizerCache
// deliberately, including the contract that matters most: nil when the models
// are not on disk yet. No fingerprints means speakers stay numbered per
// meeting -- the behaviour before this existed -- rather than an error.
//
// It does NOT download anything of its own: the model file is the second half
// of diarize.Spec, which diarizerCache already fetches.
type embedderCache struct {
	mu    sync.Mutex
	inner *voiceid.Embedder
}

func (c *embedderCache) get(modelsDir string) *voiceid.Embedder {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.inner != nil {
		return c.inner
	}
	if !asr.IsDownloaded(modelsDir, diarize.Spec) {
		return nil
	}
	e, err := voiceid.New(asr.ModelDir(modelsDir, diarize.Spec), 4)
	if err != nil {
		log.Printf("voice id: %v", err)
		return nil
	}
	c.inner = e
	return e
}

// embedTurns fingerprints the far end of one block, one fingerprint per
// person rather than per reply.
//
// Per person because that is what the question needs: several seconds of one
// voice is a far steadier fingerprint than a two-word answer, and the
// diarizer has already grouped the block's replies by voice -- inside one
// block, that grouping is the thing it is good at.
//
// The microphone side is skipped on purpose. Who is holding the machine was
// never in question, and far-end voices bleed into the mic through the
// speakers, so fingerprinting it would teach the user's own profile everyone
// else's voice.
func (a *app) embedTurns(turns []turn, call []float32, segments []diarize.Segment) {
	if len(turns) == 0 || len(call) == 0 || len(segments) == 0 {
		return
	}
	e := a.embedders.get(a.modelsDir)
	if e == nil {
		return
	}

	// One fingerprint per person, from their longest continuous stretches.
	//
	// Continuous is the point. This used to concatenate a speaker's scattered
	// replies into one buffer and hand that to the model as if it were one
	// stretch of speech, which put a splice every few hundred milliseconds --
	// a transient no speaker model was trained on, heard as part of the voice.
	bySpeaker := map[int][]diarize.Segment{}
	for _, t := range turns {
		if t.channel != channelSystem || t.speaker == youSpeaker || t.speaker == mixedSpeaker {
			continue
		}
		bySpeaker[t.speaker] = append(bySpeaker[t.speaker], diarize.Segment{Start: t.start, End: t.end, Speaker: t.speaker})
	}

	embedBySpeaker := make(map[int][]float32, len(bySpeaker))
	for speaker, segs := range bySpeaker {
		if v := diarize.Fingerprint(e, call, segs); len(v) > 0 {
			embedBySpeaker[speaker] = v
		}
	}
	for i := range turns {
		if v, ok := embedBySpeaker[turns[i].speaker]; ok {
			turns[i].embed = v
		}
	}
}

// linkSpeakers renumbers a whole meeting's turns so one person keeps one
// number from the first block to the last. Without it, "Speaker 2" means
// nothing beyond the minute it appears in -- and a talk-time statistic built
// on it would be counting strangers.
//
// The microphone's turns are left alone: they are the user by definition, and
// they never entered the clustering.
func linkSpeakers(turns []turn) {
	type key struct{ block, speaker int }
	var (
		blocks []voiceprint.BlockSpeaker
		index  = map[key]int{}
	)
	for _, t := range turns {
		if t.speaker == youSpeaker || t.speaker == mixedSpeaker {
			continue
		}
		k := key{t.block, t.speaker}
		at, ok := index[k]
		if !ok {
			blocks = append(blocks, voiceprint.BlockSpeaker{Block: t.block, Local: t.speaker})
			at = len(blocks) - 1
			index[k] = at
		}
		blocks[at].Secs += float64(t.end - t.start)
		if len(blocks[at].Embed) == 0 {
			blocks[at].Embed = t.embed
		}
	}
	if len(blocks) == 0 {
		return
	}

	ids := voiceprint.LinkBlocks(blocks, voiceprint.MergeThreshold)
	for i := range turns {
		if turns[i].speaker == youSpeaker || turns[i].speaker == mixedSpeaker {
			continue
		}
		if at, ok := index[key{turns[i].block, turns[i].speaker}]; ok {
			turns[i].speaker = ids[at]
		}
	}
}

// meetingSpeakers turns the linked turns into the rows the meeting screen
// reads: one per person, with how long they talked and how often. The totals
// are computed here, once, rather than by summing turns on every refresh --
// the meetings list draws a talk-time bar on every row.
func meetingSpeakers(turns []turn) []history.MeetingSpeaker {
	var order []int
	byID := map[int]*history.MeetingSpeaker{}
	embeds := map[int][][]float32{}
	weights := map[int][]float64{}

	for _, t := range turns {
		sp, ok := byID[t.speaker]
		if !ok {
			sp = &history.MeetingSpeaker{LocalID: t.speaker}
			byID[t.speaker] = sp
			order = append(order, t.speaker)
		}
		sp.TalkSecs += float64(t.end - t.start)
		sp.TurnCount++
		if len(t.embed) > 0 {
			embeds[t.speaker] = append(embeds[t.speaker], t.embed)
			weights[t.speaker] = append(weights[t.speaker], float64(t.end-t.start))
		}
	}

	out := make([]history.MeetingSpeaker, 0, len(order))
	for _, id := range order {
		sp := byID[id]
		// One fingerprint per person for the whole meeting: this is what a
		// later meeting compares against to say "that is the same person",
		// and what a name is eventually attached to.
		sp.Embed = voiceprint.WeightedCentroid(embeds[id], weights[id])
		out = append(out, *sp)
	}
	return out
}

// historyTurns converts the decoded turns into the storable ones. The two
// types stay separate on purpose: one carries audio and model-local speaker
// numbers through the decode, the other is what the meeting screen and the
// database agree on.
func historyTurns(turns []turn) []history.Turn {
	out := make([]history.Turn, 0, len(turns))
	for i, t := range turns {
		out = append(out, history.Turn{
			Seq:       i,
			Channel:   t.channel,
			StartSecs: float64(t.start),
			EndSecs:   float64(t.end),
			LocalID:   t.speaker,
			Text:      t.text,
		})
	}
	return out
}

// diarizeRecording answers "who spoke when" for one whole recording, in one
// pass, before a word of it is transcribed.
//
// This is the order the question has to be asked in. Diarizing inside each
// sixty-second decode block, which is what this did until it was measured,
// asks the model to tell three people apart while showing it one minute at a
// time and then renumbers them from scratch for the next minute -- so the same
// colleague is Speaker 1, then Speaker 3, then Speaker 2, and what should have
// been three people in an hour-long call came back as sixty numberings to be
// reconciled afterwards from spliced two-second fingerprints.
//
// Nil when the models are not downloaded yet, which leaves the caller on the
// older per-block path rather than refusing the recording.
func (a *app) diarizeRecording(path string) []diarize.Segment {
	if path == "" {
		return nil
	}
	d := a.speakers.get(a.modelsDir)
	if d == nil {
		return nil
	}
	segments, err := diarize.WindowedFile(d, a.embedders.get(a.modelsDir), path, maxSpeakerGap, minSpeakerSegment)
	if err != nil {
		log.Printf("meeting: diarizing %s: %v", path, err)
		return nil
	}
	return segments
}

// blockSegments cuts one recording's segments down to the block being decoded,
// with their times moved into that block's own clock -- which is what the word
// labelling downstream expects, and what the caller shifts back afterwards.
func blockSegments(segments []diarize.Segment, from, to float32) []diarize.Segment {
	out := make([]diarize.Segment, 0, len(segments))
	for _, s := range segments {
		if s.End <= from || s.Start >= to {
			continue
		}
		s.Start = max(s.Start, from) - from
		s.End = min(s.End, to) - from
		if s.End > s.Start {
			out = append(out, s)
		}
	}
	return out
}

// speakerEmbeds fingerprints each speaker of a finished recording, reading
// only the few seconds of each one it needs back off disk.
//
// One fingerprint per person for the whole meeting, built from continuous
// speech: this is what a later meeting compares against to say "that is the
// same person", and what a name is eventually attached to.
func (a *app) speakerEmbeds(path string, segments []diarize.Segment) map[int][]float32 {
	e := a.embedders.get(a.modelsDir)
	if e == nil || path == "" || len(segments) == 0 {
		return nil
	}

	bySpeaker := map[int][]diarize.Segment{}
	for _, s := range segments {
		bySpeaker[s.Speaker] = append(bySpeaker[s.Speaker], s)
	}

	out := make(map[int][]float32, len(bySpeaker))
	for speaker, segs := range bySpeaker {
		if v := diarize.FingerprintFile(e, path, segs); len(v) > 0 {
			out[speaker] = v
		}
	}
	return out
}
