package ui

import (
	"math"
	"sort"
	"strings"
	"time"

	"voxlog-go/internal/history"
)

// Eight weeks, not fourteen days: the chart is there to show a trend, and a
// fortnight of daily columns was a lot of numbers for very little of one.
const overviewWeeks = 8

type overviewWeek struct {
	Week       string `json:"week"`  // the Monday, "2006-01-02"
	Label      string `json:"label"` // "4 Aug" where the month turns over, else "11"
	Dictations int    `json:"dictations"`
	Meetings   int    `json:"meetings"`
}

// One set of figures for one span of time. The tiles show today's by default
// and swap to all_time on the pane's own toggle -- same four numbers, so they
// are computed the same way twice rather than by two different rules.
type overviewSpan struct {
	Recordings      int     `json:"recordings"`       // both kinds
	Words           int     `json:"words"`            // dictations only
	SpeakingSeconds float64 `json:"speaking_seconds"` // both kinds
	MedianDecode    float64 `json:"median_decode"`    // dictations only, seconds
	TypingMinutes   int     `json:"typing_minutes"`   // the estimate
}

type overview struct {
	Today   overviewSpan `json:"today"`
	AllTime overviewSpan `json:"all_time"`

	Weeks []overviewWeek `json:"weeks"` // oldest first, always overviewWeeks long

	// The legend under the chart: what fell inside the eight weeks drawn.
	WindowRecordings int     `json:"window_recordings"`
	WindowWords      int     `json:"window_words"`
	WindowSeconds    float64 `json:"window_seconds"`

	// Provenance for the all-time line: since when, and how much of that
	// stretch actually has something in it.
	FirstDay   string `json:"first_day"` // "" while nothing has been recorded
	ActiveDays int    `json:"active_days"`
	SpanDays   int    `json:"span_days"`
}

// buildOverview computes the Overview pane's numbers from history alone --
// nothing here is stored, so a rebuild after a restart looks the same as it
// did before. now is a parameter because "today" is exactly what's being
// measured, and a function that reads the clock cannot be tested.
func buildOverview(entries []history.Entry, meetings []history.Meeting, now time.Time) overview {
	today := now.Format("2006-01-02")

	// Build every bucket first, including empty ones, so a week with nothing
	// recorded still shows up as a column instead of vanishing from the chart.
	weeks := make([]overviewWeek, overviewWeeks)
	index := make(map[string]int, overviewWeeks)
	thisWeek := startOfWeek(now)
	for i := 0; i < overviewWeeks; i++ {
		monday := thisWeek.AddDate(0, 0, -7*(overviewWeeks-1-i))
		key := monday.Format("2006-01-02")
		weeks[i] = overviewWeek{Week: key}
		index[key] = i
	}
	labelWeeks(weeks)

	var out overview
	var todayDecodes, allDecodes []float64
	var first time.Time
	active := make(map[string]struct{})

	for _, e := range entries {
		day := e.Timestamp.Format("2006-01-02")
		words := len(strings.Fields(e.Text))

		// All time counts everything, however old: it is the one figure the
		// fourteen-day window used to make unanswerable.
		out.AllTime.Recordings++
		out.AllTime.Words += words
		out.AllTime.SpeakingSeconds += e.RecordingSeconds
		allDecodes = append(allDecodes, e.DurationSeconds)
		active[day] = struct{}{}
		if first.IsZero() || e.Timestamp.Before(first) {
			first = e.Timestamp
		}

		if i, ok := index[startOfWeek(e.Timestamp).Format("2006-01-02")]; ok {
			weeks[i].Dictations++
			out.WindowRecordings++
			out.WindowWords += words
			out.WindowSeconds += e.RecordingSeconds
		}

		if day == today {
			out.Today.Recordings++
			out.Today.Words += words
			out.Today.SpeakingSeconds += e.RecordingSeconds
			todayDecodes = append(todayDecodes, e.DurationSeconds)
		}
	}

	for _, m := range meetings {
		day := m.Start.Format("2006-01-02")

		out.AllTime.Recordings++
		out.AllTime.SpeakingSeconds += m.RecordingSeconds
		active[day] = struct{}{}
		if first.IsZero() || m.Start.Before(first) {
			first = m.Start
		}

		if i, ok := index[startOfWeek(m.Start).Format("2006-01-02")]; ok {
			weeks[i].Meetings++
			out.WindowRecordings++
			out.WindowSeconds += m.RecordingSeconds
		}

		if day == today {
			out.Today.Recordings++
			out.Today.SpeakingSeconds += m.RecordingSeconds
		}
	}

	out.Weeks = weeks
	out.Today.MedianDecode = median(todayDecodes)
	out.AllTime.MedianDecode = median(allDecodes)
	out.Today.TypingMinutes = typingMinutes(out.Today.Words)
	out.AllTime.TypingMinutes = typingMinutes(out.AllTime.Words)

	out.ActiveDays = len(active)
	if !first.IsZero() {
		out.FirstDay = first.Format("2006-01-02")
		// Inclusive of both ends: one recording made today is a span of one
		// day, not of zero. Rounded rather than truncated because a day
		// across a DST change is 23 or 25 hours long, not 24.
		from := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, first.Location())
		to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		out.SpanDays = int(math.Round(to.Sub(from).Hours()/24)) + 1
		if out.SpanDays < out.ActiveDays {
			out.SpanDays = out.ActiveDays
		}
	}

	return out
}

// 40 words/minute is a middling typing speed, not this user's actual one --
// the result is an estimate, and the UI marks it with "≈" accordingly.
func typingMinutes(words int) int {
	return int(float64(words)/40 + 0.5)
}

// Monday, in local time, with the clock thrown away. Weeks start on Monday
// because that is where a working week starts, and the chart is a chart of
// work.
func startOfWeek(t time.Time) time.Time {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	// Go counts Sunday as 0; shift so Monday is 0 and Sunday is 6.
	back := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -back)
}

// The axis names the month only where it changes -- and on the first column,
// which has no predecessor to have changed from. Eight repetitions of "Aug"
// would be eight labels saying the same thing.
func labelWeeks(weeks []overviewWeek) {
	var month time.Month
	for i := range weeks {
		monday, err := time.Parse("2006-01-02", weeks[i].Week)
		if err != nil {
			continue
		}
		if i == 0 || monday.Month() != month {
			weeks[i].Label = monday.Format("2 Jan")
		} else {
			weeks[i].Label = monday.Format("2")
		}
		month = monday.Month()
	}
}

// median, not mean: one slow model-load shouldn't make a day of otherwise
// fast decodes look slow.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
