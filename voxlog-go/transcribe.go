package main

import (
	"log"
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
	d := a.speakers.get(a.modelsDir)
	if d == nil {
		return ""
	}
	micSegments := diarize.Merge(d.Process(mic), maxSpeakerGap, minSpeakerSegment)
	callSegments := diarize.Merge(d.Process(call), maxSpeakerGap, minSpeakerSegment)
	if len(micSegments) == 0 && len(callSegments) == 0 {
		return ""
	}

	if turns := a.wordTurnsBothChannels(spec, language, mic, call, micSegments, callSegments); len(turns) > 0 {
		return renderTurns(turns, nil)
	}

	// No timings from this engine: back to cutting the audio up, which is
	// worse but is still a labelled conversation rather than one wall of text.
	micTurns := channelTurns(micSegments, mic, youSpeaker, false)
	callTurns := channelTurns(callSegments, call, 0, true)
	if len(micTurns) == 0 && len(callTurns) == 0 {
		return ""
	}
	return renderTurns(append(micTurns, callTurns...), func(samples []float32) string {
		return a.decode(spec, language, samples)
	})
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

// transcribeFiles decodes a recording from disk a block at a time -- how a
// meeting is transcribed. An hour of audio never becomes an hour of resident
// memory, and yield between blocks is what lets a dictation recorded during
// the call be decoded without waiting for all of it.
func (a *app) transcribeFiles(spec asr.ModelSpec, language, micPath, systemPath string, separate bool, yield func()) string {
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

	var out []string
	err := audio.ReadBlocks(micPath, func(mic []float32) error {
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

		if text := a.transcribeBlock(spec, language, mic, system, separate && len(system) > 0); text != "" {
			out = append(out, text)
		}
		yield()
		return nil
	})
	if err != nil {
		log.Printf("meeting: reading %s: %v", micPath, err)
	}
	return strings.Join(out, " ")
}
