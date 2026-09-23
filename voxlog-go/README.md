# Voxlog

A menu bar dictation app for macOS. Hold a key, speak, and the transcript
lands in whatever app you were typing in. Everything runs locally: the audio
never leaves the machine and nothing is sent to a server.

Native Go rewrite of the original Python/rumps app, which lived in this repo
until commit `723c673`.

## Requirements

- macOS 13 or later, Apple Silicon (arm64 only)
- Accessibility permission, or the hotkeys silently do nothing: the event tap
  is created successfully either way and simply never receives events
- Microphone permission
- Screen Recording permission, only if you capture system audio — macOS
  treats even audio-only capture as screen capture

## What it does

**Dictation.** Tap the dictate key to start, tap it again to stop — or switch
to hold-to-talk, where the recording lasts exactly as long as the key is
down. The transcript is pasted into the focused app, copied, both, or neither,
depending on the output mode. Every take is appended to a per-day JSON file.
A hold shorter than 250ms is thrown away rather than decoded: it was a key
brushed on the way to something else, not a sentence.

**Meetings.** A second binding records a whole call, on its own, for as long
as it takes: microphone and system audio together, written to disk as they
arrive rather than held in memory, so an hour costs ~115MB of disk and nothing
in RAM, and a crash mid-call loses nothing. A meeting is never pasted
anywhere — it goes to history, marked as a meeting. It can be transcribed when
it ends or left until you ask for it from the History section, and its audio
is kept unless you turn that off in Settings. Recordings otherwise sit on
disk until you clean them up yourself, with a Delete audio button on the row
or a Free up space button in Settings that sweeps by age or by a size
ceiling — both off by default. The menu bar carries the elapsed time while
one runs.

Ordinary dictation keeps working during a meeting: same key, same overlay,
same paste. It listens in on the meeting's own capture rather than opening the
microphone twice, so the two can never drift apart.

**Nothing waits for anything.** Transcription runs on its own queue, so the
next take can start while earlier ones are still decoding. The shortest
recording in the queue goes first, and a long one hands over between blocks —
a ten-second remark dictated during a meeting is decoded in the middle of that
meeting's hour of audio, not after it.

**Hotkeys.** A binding is either a single key tapped on its own (press and
release with nothing in between — this is what makes a bare right-Command
binding usable) or a combination, matched against the modifiers held when the
key goes down. Capture them in Settings by performing the shortcut; a key
already used by another binding is refused rather than silently taking over.
Escape cancels a recording in progress only if you turn that on: it gets
pressed constantly for unrelated reasons, and losing a take to a stray press
is worse than having no cancel key. Cancelling a dictation made during a
meeting leaves the meeting recording.

**Recording indicator.** A small floating capsule with a live level meter,
placed beside the cursor, following the cursor, in a screen corner, or down
in the strip the Dock sits in. The menu bar icon carries the state too: red
while the microphone is open, blue while a model is decoding.

**History.** The app is one window with four sections — Overview, History,
Meetings, Settings — rather than a separate popover and Settings window. It
opens on the Overview: today's recordings, words dictated and speaking time,
a fourteen-day chart, and the three most recent items, with a live banner
whenever a meeting is currently recording. The History section lists past
transcripts grouped by day, newest first.
Clicking an entry copies it to the clipboard rather than pasting it: a window
you opened and left open has no way to know what you want it pasted into.
Unlike the old popover, the window stays where you put it instead of hiding
itself the moment you click into another app. Old day files can be pruned
automatically after a week, two weeks, or a month. A meeting shows its length
instead of its transcription time, and an untranscribed one carries a
Transcribe button for as long as its audio is still on disk. Recordings are
kept on disk and can be played back right from the row they belong to, with
no separate player to open. They stay there until you ask for them to be
cleaned up — a Delete audio action on the row, or Free up space in Settings,
which deletes past an age or over a size ceiling you set there; neither is
on by default.

**System audio.** A meeting always records what is playing on the Mac; the
setting adds it to plain dictation too. Only one capture of it exists in the
machine, so a dictation made during a meeting is microphone-only — the meeting
owns it, and that is what you would have asked for anyway.

**Separating speakers.** With system audio on, the two sources can be
transcribed apart instead of mixed, and labelled. It only engages when the
system side actually carried sound — the toggle alone is not enough, since a
take with nothing playing has exactly one speaker. Speakers on the far end
are told apart with pyannote segmentation plus speaker embeddings, and your
own turns are interleaved with theirs in the order things were said:

```
You: треба закрити це до п'ятниці
Speaker 1: я підготую документи
Speaker 2: тоді я перевірю в четвер
```

The speaker models (~35 MB) download on first use. The take that triggers the
download does not wait for it.

**Comparing models.** Settings can record once and transcribe that recording
with every downloaded model, showing what each made of it and how long it
took, load time reported separately from decode time. Nothing is written to
history. Needs at least two downloaded models to be worth anything.

**Letting an LLM read it.** Settings has an MCP section that serves notes,
meetings and tasks to a client on this machine over the Model Context
Protocol. Off until you turn it on; when it is on it listens on 127.0.0.1
only, behind a token, and the pane hands you the one-line command to paste
into Claude Code. The port it was given is remembered, so a client
configured once keeps working after a restart. It reads by default — the
tools that add or move a task appear only if you turn on the second switch,
and nothing else about the app can be written from outside it. Transcripts
go out; the paths of the recordings behind them do not.

## Models

| Model | Size | Notes |
|---|---|---|
| `parakeet tdt-0.6b-v3` | ~2.5 GB | The one to use for dictation. 25 European languages including Ukrainian, auto-detected. Transcribed every test sentence here without an error. |
| `whisper large-v3` | ~1.8 GB | As accurate as parakeet on the same Ukrainian audio, noticeably slower to decode. Takes an explicit language. |
| `nemotron streaming-320ms` | ~0.7 GB | The only one that can show text while you speak. Measurably less accurate: it substitutes words and truncates the last word of anything handed to it in pieces, which is inherent to a streaming model. |

A model that has not been downloaded cannot be selected — choosing one used
to succeed and then fail on every dictation with no hint that the choice was
the problem.

**Ruled out:** NVIDIA Canary. sherpa-onnx only accepts `en`, `de`, `es`, `fr`
as its source language, in current master as well, since that is what the
180m-flash export covers. Pointed at the canary-1b-v2 export with the
language left empty to get past that check, it *translates* rather than
transcribes — Ukrainian speech came back as English prose.

## Sensitivity

One slider over two mechanisms. Up to 100% it moves the input device's own
volume through CoreAudio, the same control System Settings shows under
Sound → Input — the only lever that changes how much signal the microphone
actually produces. Above 100% the device is already at its maximum, so the
samples are scaled on top: a real doubling of the waveform at 200%, at the
cost of doubling the hiss with it.

Gain does not improve recognition. The same take at 100%, 200%, 400% and 800%
produced a character-identical transcript: the recognizers normalize their own
features, so amplitude cannot help them, only clip them.

The meters read **peak**, not average, on a dBFS scale from -40 to -6, and
hold the loudest moment between reads so a transient cannot slip between two
of them. They rise instantly and fall on a fixed slope, which is what a peak
meter has always done.

## Building

```sh
make build     # binary
make bundle    # ../Voxlog.app, signed
make dist      # ../Voxlog.zip, via ditto
make icons     # regenerate AppIcon.icns and the menu bar glyphs
```

`bundle` copies the two sherpa-onnx dylibs into `Contents/Frameworks` and
repoints the rpath at them. Without that the app runs only on the machine
that built it: the linker records an rpath into the local Go module cache.

Signing uses whatever codesigning identity is on the machine, falling back to
ad-hoc. macOS keys Accessibility and Screen Recording permissions to the
signature, so a stable identity means grants survive a rebuild.

Handing the app to someone else: it is signed but not notarized, so they need
`xattr -dr com.apple.quarantine /Applications/Voxlog.app` once, and then to
grant the permissions themselves.

Icons are generated by `tools/mkicons` rather than from SVG, because
qlmanage — the only SVG rasterizer macOS ships — flattens transparency onto
white, which renders a menu bar template image as a solid black rectangle.

## Layout

| Package | What lives there |
|---|---|
| `internal/audio` | Microphone capture via malgo, level metering, device volume, WAV files, block splitting |
| `internal/systemaudio` | System audio capture via ScreenCaptureKit |
| `internal/asr` | sherpa-onnx recognizers, model download and layout |
| `internal/diarize` | Speaker segmentation and clustering |
| `internal/history` | Per-day transcript files, retention |
| `internal/hotkey` | CGEventTap listener, bindings, key labels |
| `internal/output` | Clipboard and simulated paste, via NSPasteboard |
| `internal/permissions` | TCC status checks |
| `internal/settings` | JSON settings under Application Support |
| `internal/ui` | The three webview windows, and the AppKit calls behind them |

At the top level, alongside `main.go`: `app.go` holds the state a hotkey press
acts on, `dictate.go` and `meeting.go` are the two kinds of recording session,
`transcribe.go` turns a finished recording into text, `decodequeue.go` decides
what gets decoded next, and `tray.go` derives the menu bar from everything
happening at once.

## Notes

- Crashes: a panic during dictation is caught, logged with its stack, and
  surfaced as a notification rather than taking the app down. `~/Library/Logs/
  Voxlog.log` gets the process's stderr too, so a traceback has somewhere to
  land — a bundled app launched from Finder has fd 2 on `/dev/null`.
- No CoreML or Neural Engine: the linked sherpa-onnx exposes no such
  provider, so everything runs on CPU.
- Text goes to the clipboard through NSPasteboard, not `pbcopy`. pbcopy takes
  no encoding argument and decodes stdin using the environment's locale; an
  app launched from Finder has no `LANG`, so it fell back to MacRoman and
  turned every Cyrillic transcript into mojibake.
