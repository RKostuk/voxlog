# sherpa-onnx Go API notes (Task 6 spike)

Scratch notes for Task 7. Not shipped code — fold into Task 7's comments or
delete once Task 7 is done.

Package: `github.com/k2-fsa/sherpa-onnx-go-macos` v1.13.5 (latest available
via `go list -m -versions`, added to `voxlog-go/go.mod` by `go get`).

Sources:
- `go doc github.com/k2-fsa/sherpa-onnx-go-macos` and per-type `go doc`
  against the downloaded module
  (`~/go/pkg/mod/github.com/k2-fsa/sherpa-onnx-go-macos@v1.13.5/sherpa_onnx.go`,
  `c-api.h`) — this is the ground truth for all struct/function names below.
- https://github.com/k2-fsa/sherpa-onnx/issues/3664 (feature request for
  Nemotron-3.5 multilingual `prompt_index`, closed)
- https://github.com/k2-fsa/sherpa-onnx/pull/3671 (the implementation PR,
  merged 2026-06-12, "Add multilingual Nemotron-3.5 streaming ASR support") —
  read via GitHub API (`pulls/3671`) body and full `.diff`, since `go-macos`
  ships the same C API surface this PR introduced.

## OfflineRecognizer (non-streaming — Whisper/Parakeet-style transducer)

Construct:

```go
cfg := sherpa_onnx.OfflineRecognizerConfig{
    ModelConfig: sherpa_onnx.OfflineModelConfig{
        Transducer: sherpa_onnx.OfflineTransducerModelConfig{
            Encoder: "encoder.onnx",
            Decoder: "decoder.onnx",
            Joiner:  "joiner.onnx",
        },
        // OR for Whisper: ModelConfig.Whisper = OfflineWhisperModelConfig{
        //   Encoder, Decoder string; Language, Task string; TailPaddings int;
        //   EnableTokenTimestamps, EnableSegmentTimestamps int }
        Tokens:     "tokens.txt",
        NumThreads: 1,
        Debug:      0,
        Provider:   "cpu", // "cpu" | "cuda" | "coreml"
    },
    DecodingMethod: "greedy_search", // or "modified_beam_search"
}
recognizer := sherpa_onnx.NewOfflineRecognizer(&cfg)
defer sherpa_onnx.DeleteOfflineRecognizer(recognizer)
```

Decode a batch of waveform samples:

```go
stream := sherpa_onnx.NewOfflineStream(recognizer)
defer sherpa_onnx.DeleteOfflineStream(stream)

stream.AcceptWaveform(sampleRate, samples) // call ONCE with all samples
recognizer.Decode(stream)                  // or recognizer.DecodeStreams([]*OfflineStream) for batch
result := stream.GetResult()               // *OfflineRecognizerResult
// result.Text, result.Tokens, result.Timestamps, result.Durations,
// result.YsLogProbs, result.Lang, result.Emotion, result.Event
```

`OfflineStream` also exposes a generic per-stream option mechanism:
`SetOption(key, value string)`, `GetOption(key string) string`,
`HasOption(key string) bool` — backed by
`SherpaOnnxOfflineStreamSetOption`/`GetOption`/`HasOption` in `c-api.h`,
documented there with the example
`SherpaOnnxOfflineStreamSetOption(stream, "language", "en")` (used e.g. for
Whisper-family language/task hints without rebuilding the recognizer).

## OnlineRecognizer (streaming — Nemotron)

Construct:

```go
cfg := sherpa_onnx.OnlineRecognizerConfig{
    ModelConfig: sherpa_onnx.OnlineModelConfig{
        Transducer: sherpa_onnx.OnlineTransducerModelConfig{
            Encoder: "encoder.onnx",
            Decoder: "decoder.onnx",
            Joiner:  "joiner.onnx",
        },
        // Nemotron streaming ships as an online transducer model in this
        // package — there is no dedicated "Nemotron" OnlineModelConfig
        // field; it uses the Transducer sub-config above plus tokens.txt.
        Tokens:     "tokens.txt",
        NumThreads: 1,
        Provider:   "cpu",
    },
    DecodingMethod:          "greedy_search",
    EnableEndpoint:          1,
    Rule1MinTrailingSilence: 2.4,
    Rule2MinTrailingSilence: 1.2,
    Rule3MinUtteranceLength: 20,
}
recognizer := sherpa_onnx.NewOnlineRecognizer(&cfg)
defer sherpa_onnx.DeleteOnlineRecognizer(recognizer)
```

Stream lifecycle:

```go
stream := sherpa_onnx.NewOnlineStream(recognizer)
defer sherpa_onnx.DeleteOnlineStream(stream)

// feed chunks as they arrive
stream.AcceptWaveform(sampleRate, chunkSamples)

// decode loop: keep decoding while ready
for recognizer.IsReady(stream) {
    recognizer.Decode(stream) // or recognizer.DecodeStreams([]*OnlineStream)
}

// partial text any time:
partial := recognizer.GetResult(stream) // *OnlineRecognizerResult{Text, Tokens, Timestamps, Json}

// end of utterance:
if recognizer.IsEndpoint(stream) {
    final := recognizer.GetResult(stream)
    recognizer.Reset(stream) // reset internal decoder state for reuse; do NOT delete+recreate the stream for each utterance
}

// end of audio for this stream (e.g. mic stopped):
stream.InputFinished()
```

`recognizer.GetResult(stream)` returns the current hypothesis at any point —
same call is used for both "partial" (call it mid-utterance) and "final"
(call it once `IsEndpoint` is true, then `Reset`). There is no separate
partial-vs-final API; only `IsReady`/`IsEndpoint` tell you which read a given
call is.

`OnlineStream` has the same generic option mechanism as `OfflineStream`:
`SetOption(key, value string)`, `GetOption`, `HasOption`, backed by
`SherpaOnnxOnlineStreamSetOption`/`GetOption`/`HasOption`. `c-api.h`'s doc
comment example is `SherpaOnnxOnlineStreamSetOption(stream, "is_final", "1")`
(generic — used per model family for different keys).

## Nemotron multilingual prompt_index / language selection

**Not** a config struct field — no `PromptIndex` field exists anywhere in
`OnlineRecognizerConfig`/`OnlineModelConfig`/`OnlineTransducerModelConfig`
(confirmed absent via `go doc` on every online/offline config type and via
`grep -i "prompt_index\|nemotron"` over the module's `.go` and `c-api.h`
files — zero hits in the shipped v1.13.5 source).

Per PR #3671 (merged 2026-06-12, "Add multilingual Nemotron-3.5 streaming
ASR support", which is what introduced this feature and matches the C API
surface present in `c-api.h`), language/prompt selection is done via the
generic **stream option** mechanism, using the reserved key `"language"`:

```go
stream := sherpa_onnx.NewOnlineStream(recognizer)
stream.SetOption("language", "ja")   // force Japanese (per prompt_dictionary in encoder metadata)
stream.SetOption("language", "auto") // explicit auto-detect
// or simply don't call SetOption at all — empty/unset also means auto-detect
```

Internals (from the PR, not exposed in Go, informational only): the encoder
ONNX has a 6th input `prompt_index` (int64, shape `[batch]`) only on
multilingual Nemotron exports; the runtime detects this input's presence,
resolves the `"language"` stream option string to a numeric prompt id using
`prompt_dictionary`/`auto_prompt_id` baked into the encoder's ONNX metadata
at export time, and defaults to `auto_prompt_id` (the PR asserts this is
always `101`, matching the design spec's `prompt_index=101` for auto-detect)
when the option is unset or set to `"auto"`. English-only Nemotron exports
lack the `prompt_index` input entirely and ignore the `"language"` option.
The model also emits language-tag tokens (e.g. `<en-US>`) in its raw output;
per the PR the C++ runtime already filters these out of
`Text`/`Tokens`/`Timestamps` before they reach the Go-visible
`OnlineRecognizerResult`, so no extra filtering should be needed on the Go
side — but this specific claim was not directly observed against a running
model in this spike (no model files were downloaded/decoded), so re-verify
result text once Task 7 has a real model to test against.

## Cleanup (`Delete*`)

Every `New*` constructor has a matching `Delete*` free function; call it via
`defer` right after construction, symmetric with the C++ object it wraps.
Relevant to Task 7:

- `sherpa_onnx.DeleteOfflineRecognizer(recognizer)` — once, when the
  transcriber shuts down / model is unloaded.
- `sherpa_onnx.DeleteOfflineStream(stream)` — once per stream, after
  `GetResult()` is done with it (one-shot: create → accept waveform → decode
  → get result → delete).
- `sherpa_onnx.DeleteOnlineRecognizer(recognizer)` — once, when the
  transcriber shuts down / model is unloaded.
- `sherpa_onnx.DeleteOnlineStream(stream)` — once, when the *whole streaming
  session* ends (e.g. app quits or model changes) — NOT per utterance.
  Between utterances, call `recognizer.Reset(stream)` to reuse the same
  stream/decoder state instead of deleting and recreating it.

No `Close`/`Free` methods on the Go struct itself — cleanup is exclusively
via the package-level `Delete*` functions, and the struct's exported fields
are otherwise opaque (`// Has unexported fields.` in `go doc` for
`OfflineRecognizer`, `OfflineStream`, `OnlineRecognizer`, `OnlineStream`).

## Open items for Task 7

- No real model files were downloaded or run in this spike — everything
  above is verified against `go doc`/source of the Go bindings and the
  GitHub PR that implemented the feature, not against a live decode. Task 7
  should do a smoke test against an actual Nemotron/Whisper model export.
- Confirm at Task 7 time whether the `"language"` stream-option key needs to
  be set once per stream (before first `AcceptWaveform`) or can be changed
  mid-utterance; the PR text implies once-per-stream but this wasn't
  explicitly re-verified against the merged source in this spike.
