package ui

import (
	"sort"
	"strings"
	"time"

	"voxlog-go/internal/history"
)

const overviewWindowDays = 14

type overviewDay struct {
	Day        string `json:"day"`        // "2006-01-02"
	Label      string `json:"label"`      // one letter, the weekday
	Dictations int    `json:"dictations"`
	Meetings   int    `json:"meetings"`
}

type overview struct {
	Recordings      int           `json:"recordings"`       // today, both kinds
	Words           int           `json:"words"`            // today's dictations
	SpeakingSeconds float64       `json:"speaking_seconds"` // today, both kinds
	MedianDecode    float64       `json:"median_decode"`    // today's dictations, seconds
	TypingMinutes   int           `json:"typing_minutes"`   // the estimate
	Days            []overviewDay `json:"days"`             // oldest first, always 14 long
	TotalRecordings int           `json:"total_recordings"` // over those 14 days
	TotalWords      int           `json:"total_words"`
	TotalSeconds    float64       `json:"total_seconds"`
}

// buildOverview computes the Overview pane's numbers from history alone --
// nothing here is stored, so a rebuild after a restart looks the same as it
// did before. now is a parameter because "today" is exactly what's being
// measured, and a function that reads the clock cannot be tested.
func buildOverview(entries []history.Entry, meetings []history.Meeting, now time.Time) overview {
	today := now.Format("2006-01-02")

	// Build every bucket first, including empty ones, so a day with nothing
	// recorded still shows up as a column instead of vanishing from the chart.
	days := make([]overviewDay, overviewWindowDays)
	index := make(map[string]int, overviewWindowDays)
	for i := 0; i < overviewWindowDays; i++ {
		d := now.AddDate(0, 0, -(overviewWindowDays-1-i))
		day := d.Format("2006-01-02")
		days[i] = overviewDay{Day: day, Label: d.Format("Mon")[:1]}
		index[day] = i
	}

	var out overview
	var todayDurations []float64

	for _, e := range entries {
		day := e.Timestamp.Format("2006-01-02")
		i, ok := index[day]
		if !ok {
			continue // outside the fourteen-day window: counted in nothing
		}
		days[i].Dictations++
		out.TotalRecordings++
		words := len(strings.Fields(e.Text))
		out.TotalWords += words
		out.TotalSeconds += e.RecordingSeconds
		if day == today {
			out.Recordings++
			out.Words += words
			out.SpeakingSeconds += e.RecordingSeconds
			todayDurations = append(todayDurations, e.DurationSeconds)
		}
	}

	for _, m := range meetings {
		day := m.Start.Format("2006-01-02")
		i, ok := index[day]
		if !ok {
			continue
		}
		days[i].Meetings++
		out.TotalRecordings++
		out.TotalSeconds += m.RecordingSeconds
		if day == today {
			out.Recordings++
			out.SpeakingSeconds += m.RecordingSeconds
		}
	}

	out.Days = days
	out.MedianDecode = median(todayDurations)
	// 40 words/minute is a middling typing speed, not this user's actual one --
	// the result is an estimate, and the UI marks it with "≈" accordingly.
	out.TypingMinutes = int(float64(out.Words)/40 + 0.5)

	return out
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
