package ui

import (
	"testing"
	"time"

	"voxlog-go/internal/history"
)

func TestOverviewCountsTodayOnly(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	entries := []history.Entry{
		{Timestamp: now.Add(-time.Hour), Text: "one two three", RecordingSeconds: 6, DurationSeconds: 1},
		{Timestamp: now.Add(-2 * time.Hour), Text: "four five", RecordingSeconds: 4, DurationSeconds: 2},
		{Timestamp: now.AddDate(0, 0, -1), Text: "yesterday's words", RecordingSeconds: 9, DurationSeconds: 3},
	}
	meetings := []history.Meeting{
		{Start: now.Add(-3 * time.Hour), RecordingSeconds: 600},
		{Start: now.AddDate(0, 0, -2), RecordingSeconds: 900},
	}

	got := buildOverview(entries, meetings, now)
	if got.Recordings != 3 {
		t.Errorf("Recordings = %d, want 3 (two dictations and one meeting today)", got.Recordings)
	}
	if got.Words != 5 {
		t.Errorf("Words = %d, want 5 -- only today's dictations", got.Words)
	}
	if got.SpeakingSeconds != 610 {
		t.Errorf("SpeakingSeconds = %v, want 610", got.SpeakingSeconds)
	}
}

// The median, not the mean: one model load of eleven seconds should not make
// a day of instant decodes look slow.
func TestMedianDecodeIgnoresTheOutlier(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	var entries []history.Entry
	for _, d := range []float64{1.0, 1.2, 1.4, 11.0} {
		entries = append(entries, history.Entry{Timestamp: now, Text: "x", DurationSeconds: d})
	}

	got := buildOverview(entries, nil, now)
	if got.MedianDecode < 1.2 || got.MedianDecode > 1.4 {
		t.Errorf("MedianDecode = %v, want the middle of 1.0/1.2/1.4/11.0", got.MedianDecode)
	}
}

func TestWordsCountsWordsNotCharacters(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	entries := []history.Entry{
		{Timestamp: now, Text: "  треба   закрити це\nдо п'ятниці  "},
	}
	if got := buildOverview(entries, nil, now).Words; got != 5 {
		t.Errorf("Words = %d, want 5 -- runs of whitespace are one separator", got)
	}
}

// A meeting has no transcript until it is decoded, and an untranscribed one
// must still count as a recording that happened.
func TestUntranscribedMeetingStillCounts(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	got := buildOverview(nil, []history.Meeting{{Start: now, RecordingSeconds: 1200}}, now)
	if got.Recordings != 1 || got.SpeakingSeconds != 1200 {
		t.Errorf("got %d recordings / %v seconds, want 1 / 1200", got.Recordings, got.SpeakingSeconds)
	}
	if got.Words != 0 {
		t.Errorf("Words = %d, want 0 -- there is no transcript yet", got.Words)
	}
}

func TestChartAlwaysCoversFourteenDaysEndingToday(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	entries := []history.Entry{
		{Timestamp: now, Text: "today"},
		{Timestamp: now.AddDate(0, 0, -13), Text: "the oldest day still shown"},
		{Timestamp: now.AddDate(0, 0, -20), Text: "older than the window"},
	}

	got := buildOverview(entries, nil, now)
	if len(got.Days) != 14 {
		t.Fatalf("got %d days, want exactly 14 even where nothing was recorded", len(got.Days))
	}
	if got.Days[13].Day != now.Format("2006-01-02") {
		t.Errorf("last day is %s, want today last (oldest first)", got.Days[13].Day)
	}
	if got.Days[13].Dictations != 1 || got.Days[0].Dictations != 1 {
		t.Errorf("got %+v / %+v, want one dictation at each end of the window", got.Days[0], got.Days[13])
	}
	if got.TotalRecordings != 2 {
		t.Errorf("TotalRecordings = %d, want 2 -- the 20-day-old entry is outside the window", got.TotalRecordings)
	}
}

func TestEmptyHistoryIsAllZeroesNotACrash(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	got := buildOverview(nil, nil, now)
	if got.Recordings != 0 || got.Words != 0 || got.MedianDecode != 0 {
		t.Errorf("got %+v, want zeroes", got)
	}
	if len(got.Days) != 14 {
		t.Errorf("got %d days, want the chart's fourteen empty columns", len(got.Days))
	}
}

// A day boundary is local midnight, the same rule the History pane groups by.
func TestTodayEndsAtLocalMidnight(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 30, 0, 0, time.Local)
	entries := []history.Entry{
		{Timestamp: now.Add(-time.Hour), Text: "before midnight"}, // 23:30 yesterday
		{Timestamp: now, Text: "after midnight"},
	}
	if got := buildOverview(entries, nil, now).Recordings; got != 1 {
		t.Errorf("Recordings = %d, want 1 -- the 23:30 take belongs to yesterday", got)
	}
}
