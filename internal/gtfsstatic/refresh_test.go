package gtfsstatic

import (
	"testing"
	"time"
)

func TestRefreshNext_DailyAt0330_StaysOnTheWallClock(t *testing.T) {
	aest := time.FixedZone("AEST", 10*3600)
	aedt := time.FixedZone("AEDT", 11*3600)
	daily := Refresh{Hour: 3, Minute: 30, Interval: 24 * time.Hour}
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"before 03:30 it is today", time.Date(2026, 9, 23, 1, 0, 0, 0, aest), time.Date(2026, 9, 23, 3, 30, 0, 0, aest)},
		{"after 03:30 it is tomorrow", time.Date(2026, 9, 23, 3, 31, 0, 0, aest), time.Date(2026, 9, 24, 3, 30, 0, 0, aest)},
		{"exactly 03:30 is tomorrow, not now", time.Date(2026, 9, 23, 3, 30, 0, 0, aest), time.Date(2026, 9, 24, 3, 30, 0, 0, aest)},
		// The October change skips 02:00-03:00; 03:30 still exists, now AEDT,
		// and is only 23 hours after the previous one.
		{"across the October change it is 03:30 AEDT", time.Date(2026, 10, 3, 3, 30, 0, 0, aest), time.Date(2026, 10, 4, 3, 30, 0, 0, aedt)},
		// The April change makes the day 25 hours long, so 24 h from 03:30
		// AEDT is 02:30 AEST, which comes first.
		{"across the April change the interval arrives first", time.Date(2027, 4, 3, 3, 30, 0, 0, aedt), time.Date(2027, 4, 4, 2, 30, 0, 0, aest)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := daily.next(c.now); !got.Equal(c.want) {
				t.Errorf("next(%s) = %s, want %s", c.now, got, c.want)
			}
		})
	}
}

func TestRefreshNext_ShortInterval_WinsOverTheTimeOfDay(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	r := Refresh{Hour: 3, Minute: 30, Interval: 6 * time.Hour}
	if got := r.next(now); !got.Equal(now.Add(6 * time.Hour)) {
		t.Errorf("next = %s, want %s", got, now.Add(6*time.Hour))
	}
}
