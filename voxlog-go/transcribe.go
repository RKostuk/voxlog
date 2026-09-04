package main

import (
	"errors"
	"log"
	"sort"
	"strings"

	"voxlog-go/internal/asr"
	"voxlog-go/internal/audio"
	"voxlog-go/internal/diarize"
)

// The decoding side: turning a finished recording into text.
//
// Everything here runs on the decode queue's goroutine, never on the hotkey
// path, and hands control back through yield between blocks so a short take
// recorded later can jump the line.

// decode runs one piece of audio through the configured model. An empty
// result covers every failure -- callers decide whether that means "say
// nothing" or "report a failed take".
func (a *app) decode(spec asr.ModelSpec, language string, samples []float32) string {
	if len(samples) == 0 {
		return ""
	}
	transcriber, err := a.models.get(spec, asr.ModelDir(a.modelsDir, spec), language)
	if err != nil {
		log.Printf("model not ready: %v", err)
		return ""
	}
	out, err := transcriber.Transcribe(samples, language)
	if err != nil {
		log.Printf("transcribe: %v", err)
		return ""
	}
	return out
}

// transcribeBlock decodes one block of a recording, splitting the two sources
// apart if this take is a conversation.
func (a *app) transcribeBlock(spec asr.ModelSpec, language string, mic, system []float32, separate bool) string {
	if !separate {
		return a.decode(spec, language, mixAudio(mic, system))
	}
	if text := a.conversation(spec, language, mic, system); text != "" {
		return text
	}
	// Speaker models not ready yet (they download on first use): one block per
	// source is still better than refusing the take.
	return labelSpeakers(a.decode(spec, language, mic), a.decode(spec, language, system))
}

// conversation interleaves both channels into one transcript in the order
// things were actually said. The mic is run through the same segmentation
// model as the call -- not to tell speakers apart there, but to find out WHEN
// the user spoke, which is the only way their turns can be placed among the
// replies instead of stacked above them.
//
// Diarization says who spoke when; it deliberately does NOT decide what the
// recognizer gets to hear. Each channel is decoded whole, in one pass, and the
// words are sorted into speakers afterwards by their timestamps -- see
// wordTurns. Handing the recognizer a turn at a time was the earlier design
// and it produced junk: a transducer given a second of audio with no context
// hallucinates, and anything under minSpeakerSegment was dropped outright.
//
// Returns "" when the speaker models are not available yet, leaving the caller
// to fall back to the plain two-block form.
func (a *app) conversation(spec asr.ModelSpec, language string, mic, call []float32) string {
	return renderTurns(a.conversationTurns(spec, language, mic, call), nil)
}

// conversationTurns is conversation before it is flattened into text: the
// same work, stopping one step earlier, at the list of turns. A meeting keeps
// that list -- it is what per-reply playback and talk-time statistics are
// made of -- while a dictation only ever wanted the string.
//
// Every turn comes back with its text already decoded and its audio dropped,
// so a caller can hold a whole meeting's worth without holding the meeting's
// audio with it.
func (a *app) conversationTurns(spec asr.ModelSpec, language string, mic, call []float32) []turn {
	d := a.speakers.get(a.modelsDir)
	if d == nil {
		return nil
	}
	micSegments := diarize.Merge(d.Process(mic), maxSpeakerGap, minSpeakerSegment)
	callSegments := diarize.Merge(d.Process(call), maxSpeakerGap, minSpeakerSegment)
	if len(micSegments) == 0 && len(callSegments) == 0 {
		return nil
	}

	turns := a.wordTurnsBothChannels(spec, language, mic, call, micSegments, callSegments)
	if len(turns) == 0 {
		// No timings from this engine: back to cutting the audio up, which is
		// worse but is still a labelled conversation rather than one wall of
		// text.
		turns = append(
			channelTurns(micSegments, mic, youSpeaker, false),
			channelTurns(callSegments, call, 0, true)...)
		if len(turns) == 0 {
			return nil
		}
	}

	turns = a.resolveTurns(spec, language, turns)
	sortTurns(turns)
	// Fingerprints for the far end only: who is holding the microphone was
	// never in question, and a far-end voice bleeding through the speakers
	// must not teach the user's own profile somebody else's voice.
	a.embedTurns(turns, call, callSegments)
	return turns
}

// resolveTurns decodes the turns that are still audio and drops the ones that
// decoded to nothing. After it, a turn is text and timings only -- the audio
// it was cut from is released, which is what lets an hour-long meeting's turns
// be accumulated in memory while it decodes.
func (a *app) resolveTurns(spec asr.ModelSpec, language string, turns []turn) []turn {
	out := turns[:0]
	for _, t := range turns {
		if t.text == "" && t.audio != nil {
			t.text = a.decode(spec, language, t.audio)
		}
		t.audio = nil
		if t.text == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// wordTurnsBothChannels decodes each channel once and labels the words that
// come back. Empty means this model reported no word timings and the caller
// has to cut the audio instead.
func (a *app) wordTurnsBothChannels(spec asr.ModelSpec, language string, mic, call []float32, micSegments, callSegments []diarize.Segment) []turn {
	micWords := a.decodeWords(spec, language, mic)
	callWords := a.decodeWords(spec, language, call)
	if len(micWords) == 0 && len(callWords) == 0 {
		return nil
	}
	return append(
		wordTurns(micWords, micSegments, youSpeaker, false),
		wordTurns(callWords, callSegments, 0, true)...)
}

// decodeWords is decode's word-level twin: the same model, the same "an empty
// result covers every failure" contract, but with the timings sherpa already
// hands back alongside the text. Empty for an engine that cannot report them
// (the streaming recognizer) or a model that did not.
func (a *app) decodeWords(spec asr.ModelSpec, language string, samples []float32) []asr.Word {
	if len(samples) == 0 {
		return nil
	}
	transcriber, err := a.models.get(spec, asr.ModelDir(a.modelsDir, spec), language)
	if err != nil {
		log.Printf("model not ready: %v", err)
		return nil
	}
	wt, ok := transcriber.(asr.WordTranscriber)
	if !ok {
		return nil
	}
	words, err := wt.TranscribeWords(samples, language)
	if err != nil {
		log.Printf("transcribe words: %v", err)
		return nil
	}
	return words
}

// sortTurns puts both channels into one conversation: what the user said sits
// between the replies it came between, rather than in a block of its own.
// Stable, so two turns starting at the same instant keep the order they were
// found in rather than swapping between runs.
func sortTurns(turns []turn) {
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].start < turns[j].start })
}

// transcribeBlockTurns is transcribeBlock's turn-shaped twin: same decisions,
// same fallbacks, but it stops before the text is flattened.
func (a *app) transcribeBlockTurns(spec asr.ModelSpec, language string, mic, system []float32, separate bool) []turn {
	if separate {
		if turns := a.conversationTurns(spec, language, mic, system); len(turns) > 0 {
			return turns
		}
		// Speaker models not ready yet (they download on first use): one turn
		// per source is still better than refusing the take.
		var out []turn
		if text := a.decode(spec, language, mic); text != "" {
			out = append(out, turn{end: blockSeconds(mic), speaker: youSpeaker, channel: channelMic, text: text})
		}
		if text := a.decode(spec, language, system); text != "" {
			out = append(out, turn{end: blockSeconds(system), speaker: 0, channel: channelSystem, text: text})
		}
		return out
	}

	text := a.decode(spec, language, mixAudio(mic, system))
	if text == "" {
		return nil
	}
	// One turn covering the whole block, credited to nobody: the two sides
	// were mixed before the recognizer saw them, so there is no honest way to
	// say who spoke. It still carries timings, so it still plays back.
	return []turn{{end: blockSeconds(mic), speaker: mixedSpeaker, channel: channelMic, text: text}}
}

func blockSeconds(samples []float32) float32 {
	return float32(len(samples)) / float32(audio.SampleRate)
}

// transcribeFilesTurns is transcribeFiles with the structure kept: it decodes
// a recording block by block and returns every turn in it, timed from the
// start of the RECORDING rather than from the start of its block.
//
// That shift is the whole point. audio.ReadBlocks hands out an exact ordered
// partition of the file, so a running sample count in this callback is the
// absolute offset of each block, and adding it to a turn's own timings is
// what makes "play this reply" mean anything an hour into a call.
//
// stop is checked between blocks so the background backfill can be abandoned
// the moment the user starts recording something; it returns errStopped, and
// nothing is written.
func (a *app) transcribeFilesTurns(spec asr.ModelSpec, language, micPath, systemPath string, separate bool, yield func(), stop <-chan struct{}) ([]turn, error) {
	var systemR *audio.WAVReader
	if systemPath != "" {
		r, err := audio.OpenWAV(systemPath)
		if err != nil {
			log.Printf("meeting: system audio unreadable, transcribing the microphone alone: %v", err)
		} else {
			systemR = r
			defer systemR.Close()
		}
	}

	var (
		out   []turn
		at    int64 // samples of the microphone track consumed so far
		block int
	)
	err := audio.ReadBlocks(micPath, func(mic []float32) error {
		select {
		case <-stop:
			return errStopped
		default:
		}

		offset := float32(at) / float32(audio.SampleRate)
		at += int64(len(mic))

		var system []float32
		if systemR != nil {
			// Exactly as many samples as the microphone block, so the two
			// sides describe the same stretch of the call.
			block, err := systemR.Read(len(mic))
			if err != nil {
				log.Printf("meeting: reading system audio: %v", err)
				systemR = nil
			}
			system = block
		}

		for _, t := range a.transcribeBlockTurns(spec, language, mic, system, separate && len(system) > 0) {
			t.start += offset
			t.end += offset
			t.block = block
			out = append(out, t)
		}
		block++
		yield()
		return nil
	})
	if err != nil {
		if errors.Is(err, errStopped) {
			return nil, err
		}
		log.Printf("meeting: reading %s: %v", micPath, err)
	}
	return out, nil
}

// transcribeSamples decodes a take that is already in memory -- every
// dictation. Short takes are one block, which is the case that matters: no
// seams, no joining, exactly what this did before blocks existed.
func (a *app) transcribeSamples(spec asr.ModelSpec, language string, mic, system []float32, separate bool, yield func()) string {
	blocks := audio.SplitBlocks(mic)
	if len(blocks) == 1 {
		return a.transcribeBlock(spec, language, mic, system, separate)
	}

	var out []string
	at := 0
	for _, block := range blocks {
		// The system side is cut at the same sample offsets, so the two stay
		// aligned block for block.
		var sys []float32
		if hi := at + len(block); at < len(system) {
			sys = system[at:min(hi, len(system))]
		}
		at += len(block)

		if text := a.transcribeBlock(spec, language, block, sys, separate); text != "" {
			out = append(out, text)
		}
		yield()
	}
	return strings.Join(out, " ")
}
