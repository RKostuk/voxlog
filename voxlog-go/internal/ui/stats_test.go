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
	if got.Today.Recordings != 3 {
		t.Errorf("Today.Recordings = %d, want 3 (two dictations and one meeting today)", got.Today.Recordings)
	}
	if got.Today.Words != 5 {
		t.Errorf("Today.Words = %d, want 5 -- only today's dictations", got.Today.Words)
	}
	if got.Today.SpeakingSeconds != 610 {
		t.Errorf("Today.SpeakingSeconds = %v, want 610", got.Today.SpeakingSeconds)
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
	if got.Today.MedianDecode < 1.2 || got.Today.MedianDecode > 1.4 {
		t.Errorf("Today.MedianDecode = %v, want the middle of 1.0/1.2/1.4/11.0", got.Today.MedianDecode)
	}
}

func TestWordsCountsWordsNotCharacters(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	entries := []history.Entry{
		{Timestamp: now, Text: "  треба   закрити це\nдо п'ятниці  "},
	}
	if got := buildOverview(entries, nil, now).Today.Words; got != 5 {
		t.Errorf("Today.Words = %d, want 5 -- runs of whitespace are one separator", got)
	}
}

// A meeting has no transcript until it is decoded, and an untranscribed one
// must still count as a recording that happened.
func TestUntranscribedMeetingStillCounts(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	got := buildOverview(nil, []history.Meeting{{Start: now, RecordingSeconds: 1200}}, now)
	if got.Today.Recordings != 1 || got.Today.SpeakingSeconds != 1200 {
		t.Errorf("got %d recordings / %v seconds, want 1 / 1200", got.Today.Recordings, got.Today.SpeakingSeconds)
	}
	if got.Today.Words != 0 {
		t.Errorf("Today.Words = %d, want 0 -- there is no transcript yet", got.Today.Words)
	}
}

func TestChartAlwaysCoversEightWeeksEndingThisWeek(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local) // a Tuesday
	entries := []history.Entry{
		{Timestamp: now, Text: "this week"},
		{Timestamp: now.AddDate(0, 0, -49), Text: "the oldest week still shown"},
		{Timestamp: now.AddDate(0, 0, -70), Text: "older than the window"},
	}

	got := buildOverview(entries, nil, now)
	if len(got.Weeks) != overviewWeeks {
		t.Fatalf("got %d weeks, want exactly %d even where nothing was recorded", len(got.Weeks), overviewWeeks)
	}
	if want := startOfWeek(now).Format("2006-01-02"); got.Weeks[7].Week != want {
		t.Errorf("last column is %s, want this week (%s) last, oldest first", got.Weeks[7].Week, want)
	}
	if got.Weeks[7].Dictations != 1 || got.Weeks[0].Dictations != 1 {
		t.Errorf("got %+v / %+v, want one dictation at each end of the window", got.Weeks[0], got.Weeks[7])
	}
	if got.WindowRecordings != 2 {
		t.Errorf("WindowRecordings = %d, want 2 -- the 70-day-old entry is outside the window", got.WindowRecordings)
	}
}

// A week is Monday to Sunday: a Sunday take belongs to the week that started
// six days earlier, not to the one beginning the next morning.
func TestWeeksStartOnMonday(t *testing.T) {
	sunday := time.Date(2026, 8, 16, 22, 0, 0, 0, time.Local)
	monday := time.Date(2026, 8, 17, 9, 0, 0, 0, time.Local)
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)

	got := buildOverview([]history.Entry{{Timestamp: sunday, Text: "a"}, {Timestamp: monday, Text: "b"}}, nil, now)
	if got.Weeks[6].Dictations != 1 {
		t.Errorf("previous week has %d dictations, want the Sunday take", got.Weeks[6].Dictations)
	}
	if got.Weeks[7].Dictations != 1 {
		t.Errorf("this week has %d dictations, want the Monday take", got.Weeks[7].Dictations)
	}
}

// The axis says the month only where it turns over; repeating it on every
// column is eight labels carrying one fact.
func TestWeekLabelsNameTheMonthOnlyWhenItChanges(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	weeks := buildOverview(nil, nil, now).Weeks
	if weeks[0].Label != "29 Jun" {
		t.Errorf("first label = %q, want %q -- the first column has no predecessor to differ from", weeks[0].Label, "29 Jun")
	}
	var named int
	for _, w := range weeks {
		if len(w.Label) > 2 {
			named++
		}
	}
	if named != 3 {
		t.Errorf("%d labels carry a month, want 3 (the first, plus July and August turning over)", named)
	}
}

// What the fourteen-day window could never answer: how much there is in
// total, however old.
func TestAllTimeCountsWhatTheWindowDoesNot(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	entries := []history.Entry{
		{Timestamp: now, Text: "two words", RecordingSeconds: 10, DurationSeconds: 1},
		{Timestamp: now.AddDate(0, 0, -200), Text: "three words here", RecordingSeconds: 30, DurationSeconds: 3},
	}
	meetings := []history.Meeting{{Start: now.AddDate(0, 0, -100), RecordingSeconds: 600}}

	got := buildOverview(entries, meetings, now)
	if got.AllTime.Recordings != 3 {
		t.Errorf("AllTime.Recordings = %d, want 3 -- age is not a reason to stop counting", got.AllTime.Recordings)
	}
	if got.AllTime.Words != 5 {
		t.Errorf("AllTime.Words = %d, want 5", got.AllTime.Words)
	}
	if got.AllTime.SpeakingSeconds != 640 {
		t.Errorf("AllTime.SpeakingSeconds = %v, want 640", got.AllTime.SpeakingSeconds)
	}
	if got.AllTime.MedianDecode != 2 {
		t.Errorf("AllTime.MedianDecode = %v, want 2 -- the middle of 1 and 3", got.AllTime.MedianDecode)
	}
	if got.FirstDay != now.AddDate(0, 0, -200).Format("2006-01-02") {
		t.Errorf("FirstDay = %q, want the oldest recording's day", got.FirstDay)
	}
	if got.ActiveDays != 3 {
		t.Errorf("ActiveDays = %d, want 3 -- three distinct days have something in them", got.ActiveDays)
	}
	if got.SpanDays != 201 {
		t.Errorf("SpanDays = %d, want 201 -- both ends counted", got.SpanDays)
	}
}

// Two recordings on one day are one day of recording, not two.
func TestActiveDaysCountsDaysNotRecordings(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	entries := []history.Entry{
		{Timestamp: now.Add(-time.Hour), Text: "a"},
		{Timestamp: now.Add(-2 * time.Hour), Text: "b"},
	}
	got := buildOverview(entries, []history.Meeting{{Start: now.Add(-3 * time.Hour)}}, now)
	if got.ActiveDays != 1 || got.SpanDays != 1 {
		t.Errorf("got %d active / %d span, want 1 / 1", got.ActiveDays, got.SpanDays)
	}
}

func TestEmptyHistoryIsAllZeroesNotACrash(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, time.Local)
	got := buildOverview(nil, nil, now)
	if got.Today.Recordings != 0 || got.Today.Words != 0 || got.Today.MedianDecode != 0 {
		t.Errorf("got %+v, want zeroes", got.Today)
	}
	if got.AllTime.Recordings != 0 || got.FirstDay != "" || got.SpanDays != 0 {
		t.Errorf("got %+v / first %q / span %d, want nothing recorded yet", got.AllTime, got.FirstDay, got.SpanDays)
	}
	if len(got.Weeks) != overviewWeeks {
		t.Errorf("got %d weeks, want the chart's %d empty columns", len(got.Weeks), overviewWeeks)
	}
}

// A day boundary is local midnight, the same rule the History pane groups by.
func TestTodayEndsAtLocalMidnight(t *testing.T) {
	now := time.Date(2026, 8, 18, 0, 30, 0, 0, time.Local)
	entries := []history.Entry{
		{Timestamp: now.Add(-time.Hour), Text: "before midnight"}, // 23:30 yesterday
		{Timestamp: now, Text: "after midnight"},
	}
	if got := buildOverview(entries, nil, now).Today.Recordings; got != 1 {
		t.Errorf("Today.Recordings = %d, want 1 -- the 23:30 take belongs to yesterday", got)
	}
}

func TestOverviewBucketsAllTimeByMonth(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)
	entries := []history.Entry{
		{Timestamp: now, Text: "сьогодні"},
		{Timestamp: now.AddDate(0, -2, 0), Text: "два місяці тому"},
		// Older than the twelve months drawn: counted in All time, not on
		// the chart.
		{Timestamp: now.AddDate(-2, 0, 0), Text: "позаминулого року"},
	}
	meetings := []history.Meeting{{Start: now.AddDate(0, -2, -1), RecordingSeconds: 60}}

	ov := buildOverview(entries, meetings, now)
	if len(ov.Months) == 0 {
		t.Fatal("no monthly buckets at all")
	}
	if len(ov.Months) > overviewMonths {
		t.Fatalf("got %d months, want at most %d", len(ov.Months), overviewMonths)
	}
	last := ov.Months[len(ov.Months)-1]
	if last.Month != now.Format("2006-01") {
		t.Errorf("last column is %q, want this month %q", last.Month, now.Format("2006-01"))
	}
	if last.Dictations != 1 || last.Meetings != 0 {
		t.Errorf("this month = %+v", last)
	}
	var found bool
	for _, m := range ov.Months {
		if m.Month == now.AddDate(0, -2, 0).Format("2006-01") {
			found = true
			if m.Dictations != 1 || m.Meetings != 1 {
				t.Errorf("two months ago = %+v, want one of each", m)
			}
		}
	}
	if !found {
		t.Error("the month two months back is missing from the chart")
	}
	if ov.AllTime.Recordings != 4 {
		t.Errorf("all time counts %d recordings, want 4", ov.AllTime.Recordings)
	}
}

func TestOverviewDropsMonthsBeforeTheFirstRecording(t *testing.T) {
	// A user two months in should not be shown ten empty columns explaining
	// how long they did not have the app.
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)
	ov := buildOverview([]history.Entry{{Timestamp: now.AddDate(0, -1, 0), Text: "перший"}}, nil, now)
	if len(ov.Months) != 2 {
		t.Fatalf("got %d months, want 2 (last month and this one): %+v", len(ov.Months), ov.Months)
	}
	if ov.Months[0].Label != "Aug 26" {
		t.Errorf("first label is %q, want the year named once at the left", ov.Months[0].Label)
	}
	if ov.Months[1].Label != "Sep" {
		t.Errorf("second label is %q, want the year left off inside one year", ov.Months[1].Label)
	}
}

func TestOverviewWithNoHistoryStillDrawsThisMonth(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.Local)
	ov := buildOverview(nil, nil, now)
	if len(ov.Months) != 1 || ov.Months[0].Month != "2026-09" {
		t.Fatalf("empty history gave %+v, want this month alone", ov.Months)
	}
}
