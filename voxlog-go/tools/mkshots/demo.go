package main

import (
	"encoding/json"
	"log"
	"time"

	"voxlog-go/internal/settings"
)

// Everything below is invented. Nothing here comes from anybody's machine:
// the point of rendering the interface separately is that a screenshot of
// Voxlog should never contain a real transcript.

const meetingID = "2026-09-22T10:04:00.000000000Z"

func day(offset int) time.Time {
	return time.Now().AddDate(0, 0, -offset)
}

func stamp(t time.Time) string { return t.Format(time.RFC3339Nano) }

// voxlogJSON is the object internal/ui assigns in one Eval at window
// creation -- same keys, same shapes (see pageData and settingsJSON).
func voxlogJSON(pane string) string {
	cfg := settings.DefaultSettings()
	cfg.DictateKeyID = "sym:command_r"
	cfg.HistoryKeyID = "sym:option_r"
	cfg.MeetingKeyID = "vk:100"
	cfg.TranscriptsDir = "/Users/you/Documents/Voxlog/Transcripts"
	cfg.AlwaysOn = true
	cfg.MCPEnabled = true
	cfg.CaptureSystemAudio = true
	cfg.MeetingSystemAudio = true
	cfg.SeparateSpeakers = true
	// Otherwise the pane seeds the project dictionary on load and saves,
	// which means a stubbed saveSettings and a re-render mid-screenshot.
	cfg.EntityDictionarySeeded = true
	cfg.EntityDictionary = []string{"Voxlog", "Website"}

	raw, err := json.Marshal(cfg)
	if err != nil {
		log.Fatal(err)
	}
	var settingsMap map[string]any
	if err := json.Unmarshal(raw, &settingsMap); err != nil {
		log.Fatal(err)
	}

	out := map[string]any{
		"pane":        pane,
		"days":        demoDays(),
		"meetings":    demoMeetings(),
		"overview":    demoOverview(),
		"tasks":       demoTasks(),
		"rejected":    []string{"thanks, talk tomorrow"},
		"decodeQueue": demoQueue(),
		"settings":    settingsMap,
		"models":      demoModels(),
		"llmModel":    map[string]any{"downloaded": true},
	}
	body, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		log.Fatal(err)
	}
	return string(body)
}

func demoDays() []map[string]any {
	today, yesterday := day(0), day(1)
	return []map[string]any{
		{
			"day": today.Format("2006-01-02"),
			"entries": []map[string]any{
				{
					"time": "16:41", "duration_seconds": 1.4, "recording_seconds": 6.2,
					"text": "Ask the hosting provider whether the staging box can be resized before the demo.",
					"id":   stamp(today), "audio": "2026-09-23T16-41-02.wav", "has_audio": true,
					"task_id":     "20260923-164104.002913000",
					"task_entity": "Website", "task_status": "todo",
				},
				{
					"time": "14:08", "duration_seconds": 2.1, "recording_seconds": 11.8,
					"text": "The importer should skip rows it has already seen instead of failing the whole file.",
					"id":   stamp(today.Add(-2 * time.Hour)), "audio": "2026-09-23T14-08-30.wav", "has_audio": true,
				},
				{
					"time": "09:55", "duration_seconds": 0.9, "recording_seconds": 4.1,
					"text": "Note to self: the release notes still say beta.",
					"id":   stamp(today.Add(-7 * time.Hour)), "has_audio": false, "auto_started": true,
				},
			},
		},
		{
			"day": yesterday.Format("2006-01-02"),
			"entries": []map[string]any{
				{
					"time": "18:20", "duration_seconds": 1.7, "recording_seconds": 9.4,
					"text": "Book the room for Thursday and send the agenda the evening before.",
					"id":   stamp(yesterday), "audio": "2026-09-22T18-20-11.wav", "has_audio": true,
				},
				{
					"time": "11:02", "duration_seconds": 1.2, "recording_seconds": 5.5,
					"text": "Reply to the design feedback: the empty state needs a sentence, not an icon.",
					"id":   stamp(yesterday.Add(-7 * time.Hour)), "audio": "2026-09-22T11-02-48.wav", "has_audio": true,
				},
			},
		},
	}
}

func demoMeetings() []map[string]any {
	second := day(3)
	return []map[string]any{
		{
			"id": meetingID, "time": "10:04", "day": day(1).Format("2006-01-02"),
			"recording_seconds": 2748, "duration_seconds": 74.2,
			"summary": "Agreed to cut the importer's scope for the first release and ship the CSV path only. Alex takes the migration notes, Priya checks the pricing page copy before Thursday.",
			"text":    "Full transcript of the design review.",
			"entity":  "Website", "audio": "meeting-2026-09-22T10-04-00.wav",
			"system_audio": "meeting-2026-09-22T10-04-00-system.wav", "has_audio": true, "has_turns": true,
			"speakers": []map[string]any{
				{"row": 1, "speaker": -1, "talk_secs": 1180, "turn_count": 31, "identified": true},
				{"row": 2, "speaker": 1, "name": "Alex", "voice_id": 4, "talk_secs": 902, "turn_count": 24, "identified": true},
				{"row": 3, "speaker": 2, "name": "Priya", "voice_id": 7, "talk_secs": 611, "turn_count": 18, "identified": true},
			},
		},
		{
			"id": stamp(second), "time": "09:30", "day": second.Format("2006-01-02"),
			"recording_seconds": 963, "duration_seconds": 28.6,
			"summary": "Weekly standup. Nothing blocked; the flaky test is a timing issue in the decode queue.",
			"text":    "Full transcript of the standup.", "entity": "Voxlog",
			"audio": "meeting-2026-09-20T09-30-00.wav", "has_audio": true, "has_turns": true,
			"speakers": []map[string]any{
				{"row": 4, "speaker": -1, "talk_secs": 402, "turn_count": 14, "identified": true},
				{"row": 5, "speaker": 1, "name": "Alex", "voice_id": 4, "talk_secs": 388, "turn_count": 12, "identified": true},
			},
		},
	}
}

// The Overview payload, in the shape buildOverview writes today: two spans
// for the tiles, and two sets of columns for the chart -- weeks behind
// Today, months behind All time.
func demoOverview() map[string]any {
	weekly := []struct{ dict, meet int }{{9, 1}, {14, 2}, {11, 1}, {18, 3}, {7, 0}, {16, 2}, {21, 3}, {12, 2}}
	weeks := make([]map[string]any, 0, len(weekly))
	monday := startOfDemoWeek(time.Now())
	for i, w := range weekly {
		at := monday.AddDate(0, 0, -7*(len(weekly)-1-i))
		label := at.Format("2")
		if i == 0 || at.Day() <= 7 {
			label = at.Format("2 Jan")
		}
		weeks = append(weeks, map[string]any{
			"week": at.Format("2006-01-02"), "label": label,
			"dictations": w.dict, "meetings": w.meet,
		})
	}

	monthly := []struct{ dict, meet int }{{34, 4}, {51, 7}, {46, 5}, {62, 9}, {58, 8}, {49, 6}}
	months := make([]map[string]any, 0, len(monthly))
	first := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.Local)
	for i, m := range monthly {
		at := first.AddDate(0, -(len(monthly) - 1 - i), 0)
		label := at.Format("Jan")
		if i == 0 {
			label = at.Format("Jan 06")
		}
		months = append(months, map[string]any{
			"month": at.Format("2006-01"), "label": label,
			"dictations": m.dict, "meetings": m.meet,
		})
	}

	return map[string]any{
		"today": map[string]any{
			"recordings": 7, "words": 612, "speaking_seconds": 418.2,
			"median_decode": 1.4, "typing_minutes": 15,
		},
		"all_time": map[string]any{
			"recordings": 364, "words": 48210, "speaking_seconds": 92140.0,
			"median_decode": 1.6, "typing_minutes": 1205,
		},
		"weeks": weeks, "months": months,
		"window_recordings": 108, "window_words": 15840, "window_seconds": 28460.0,
		"first_day":   first.AddDate(0, -5, 0).Format("2006-01-02"),
		"active_days": 128, "span_days": 164,
	}
}

// The Monday of t's week, local, same rule as stats.go.
func startOfDemoWeek(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

func demoTasks() []map[string]any {
	return []map[string]any{
		{
			"id": "20260923-164104.002913000", "source_kind": "dictation",
			"source_key": stamp(day(0)), "text": "Resize the staging box before the demo",
			"entity": "Website", "status": "todo", "created": stamp(day(0)),
		},
		{
			"id": "20260922-100900.114000000", "source_kind": "meeting",
			"source_key": meetingID, "text": "Check the pricing page copy before Thursday",
			"entity": "Website", "status": "in_progress", "created": stamp(day(1)),
			"notes": "Priya has the draft.",
		},
		{
			"id": "20260922-101500.552000000", "source_kind": "meeting",
			"source_key": meetingID, "text": "Cut the importer down to the CSV path for the first release",
			"entity": "Voxlog", "status": "todo", "created": stamp(day(1)),
		},
		{
			"id": "20260920-093400.019000000", "source_kind": "meeting",
			"source_key": stamp(day(3)), "text": "Fix the timing issue in the decode queue test",
			"entity": "Voxlog", "status": "done", "created": stamp(day(3)),
		},
	}
}

func demoQueue() []map[string]any {
	return []map[string]any{
		{
			"label": "Meeting, 10:04", "kind": "transcribe", "key": meetingID,
			"seconds": 2748, "running": true,
			"queued_at_ms": time.Now().Add(-40 * time.Second).UnixMilli(),
		},
		{
			"label": "Dictation, 16:41", "kind": "llm", "stage": "Looking for tasks",
			"key": stamp(day(0)), "seconds": 6.2, "running": false,
			"queued_at_ms": time.Now().Add(-8 * time.Second).UnixMilli(),
		},
	}
}

func demoModels() []map[string]any {
	return []map[string]any{
		{"family": "parakeet", "variant": "tdt-0.6b-v3", "downloaded": true,
			"supports_language": false, "supports_streaming": false,
			"description": "The one to use. 25 European languages, detected automatically."},
		{"family": "whisper", "variant": "large-v3", "downloaded": true,
			"supports_language": true, "supports_streaming": false,
			"description": "As accurate, noticeably slower. Takes an explicit language."},
		{"family": "nemotron", "variant": "streaming-320ms", "downloaded": false,
			"supports_language": false, "supports_streaming": true,
			"description": "The only one that can show text while you speak."},
	}
}

// bindingAnswers is what the page gets back from the bindings it calls while
// it is loading. Anything missing here is a broken screenshot: several call
// sites .then() the answer and stop rendering if it is not a Promise.
var bindingAnswers = mustJSON(map[string]any{
	"keyLabel":              "Right Command",
	"inputDevices":          []string{"MacBook Pro Microphone", "Studio Display Microphone"},
	"recordingsSize":        map[string]any{"bytes": 4_930_000_000, "human": "4.9 GB"},
	"micVolume":             map[string]any{"value": 0.82, "readable": true, "settable": true},
	"defaultTranscriptsDir": "/Users/you/Documents/Voxlog/Transcripts",
	"checkPermissions": map[string]any{
		"accessibility": "granted", "microphone": "authorized",
		"screenrecording": "granted", "systemaudio": "",
	},
	"meetingStatus": map[string]any{"running": true, "seconds": 2748},
	"mcpStatus": map[string]any{
		"enabled": true, "running": true, "port": 51888,
		"url": "http://127.0.0.1:51888/mcp",
		// Obviously not a real token: the connect command under it carries
		// the token in full, so the demo one has to be unusable on sight.
		"token": "demo-token-not-a-real-one", "error": "",
	},
	"meetingDetail": map[string]any{
		"speakers": demoMeetings()[0]["speakers"],
		"turns":    demoTurns(),
	},
	"peopleStats": []map[string]any{
		{"voice_id": 4, "name": "Alex", "meetings": 14, "talk_secs": 9820, "turn_count": 214, "last_seen": stamp(day(1))},
		{"voice_id": 7, "name": "Priya", "meetings": 9, "talk_secs": 5110, "turn_count": 132, "last_seen": stamp(day(1))},
	},
	"voiceList": map[string]any{
		"voices":  []map[string]any{{"id": 4, "name": "Alex", "secs": 9820, "has_clip": true}},
		"unnamed": []any{},
	},
	"searchMeetings": []any{},
	"similarUnnamed": []any{},
	"runModelTest":   []any{},
})

func demoTurns() []map[string]any {
	lines := []struct {
		speaker int
		name    string
		text    string
		start   float64
	}{
		{-1, "", "So the importer. Do we need the whole thing for the first release?", 12.4},
		{1, "Alex", "Not really. Everyone we have talked to is coming from a CSV export anyway.", 18.9},
		{2, "Priya", "Then ship the CSV path and say so on the pricing page.", 26.1},
		{-1, "", "Agreed. Alex, can you write the migration notes this week?", 33.7},
		{1, "Alex", "Yes. I will keep it to one page.", 39.2},
	}
	out := make([]map[string]any, 0, len(lines))
	for i, l := range lines {
		channel := 1
		if l.speaker == -1 {
			channel = 0
		}
		out = append(out, map[string]any{
			"seq": i, "start": l.start, "end": l.start + 5.2, "channel": channel,
			"speaker": l.speaker, "name": l.name, "text": l.text,
		})
	}
	return out
}

// silentBindings do something in the app and answer nothing. A screenshot
// never triggers them, but a stray event might, and an undefined function
// throws where a no-op does not.
var silentBindings = mustJSON([]string{
	"applyEntry", "pasteEntry", "transcribeEntry", "stopMeeting", "deleteRecording",
	"saveSettings", "revealModels", "chooseDirectory", "restartApp", "settingsWindowClosed",
	"setMicVolume", "startMicTest", "stopMicTest", "startModelTest", "cancelModelTest",
	"requestPermission", "captureKey", "downloadModel", "downloadLLMModel", "freeUpSpace",
	"setTaskStatus", "setTaskText", "setTaskNotes", "deleteTask", "rejectTask",
	"removeRejected", "restoreRejected", "setMeetingEntity", "nameSpeaker", "linkSpeaker",
	"unlinkSpeaker", "renameVoice", "forgetVoice", "eraseVoice", "copyText",
	"mcpRegenerateToken", "finishWelcome", "welcomeStep", "setLLMAPIKey", "llmAPIKeyStored",
})

func mustJSON(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		log.Fatal(err)
	}
	return string(body)
}
