package servicetime

import (
	"strings"
	"testing"
	"time"
)

// Expected instants are written against fixed offsets rather than Loc, so a
// wrong tz rule in the code cannot also be the rule the test checks against.
var (
	aest = time.FixedZone("AEST", 10*3600)
	aedt = time.FixedZone("AEDT", 11*3600)
)

// The two transitions nearest the time of writing. October: 02:00 AEST
// becomes 03:00 AEDT, a 23-hour day. April: 03:00 AEDT becomes 02:00 AEST, a
// 25-hour day. Hard-coded on purpose (§11.1).
var (
	october = date(2026, 10, 4)
	april   = date(2027, 4, 4)
	plain   = date(2026, 9, 23)
)

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

const hour = 3600

func TestParseGTFSTime_ValidInput_ReturnsSeconds(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"midnight is zero", "00:00:00", 0},
		{"a time past midnight keeps counting hours (§9.2 case 1)", "25:10:00", 90600},
		{"exactly 24:00:00 is the next midnight, not zero", "24:00:00", 86400},
		{"an unpadded minute is tolerated", "07:5:00", 7*hour + 5*60},
		{"an unpadded hour is tolerated", "7:05:00", 7*hour + 5*60},
		{"the last second of a two-day service", "47:59:59", 48*hour - 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseGTFSTime(c.in)
			if err != nil {
				t.Fatalf("ParseGTFSTime(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("ParseGTFSTime(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestParseGTFSTime_InvalidInput_ReturnsErrorNamingInput(t *testing.T) {
	cases := []struct{ name, in string }{
		{"a negative time is rejected (§9.2 case 2)", "-01:00:00"},
		{"a leading plus is rejected", "+1:00:00"},
		{"empty is rejected", ""},
		{"two fields are rejected", "12:00"},
		{"four fields are rejected", "12:00:00:00"},
		{"an empty field is rejected", "12::00"},
		{"sixty minutes is rejected", "12:60:00"},
		{"sixty seconds is rejected", "12:00:60"},
		{"letters are rejected", "ab:00:00"},
		{"surrounding space is rejected", " 1:00:00"},
		{"an hour too large for an int is rejected", "99999999999999999999:00:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseGTFSTime(c.in)
			if err == nil {
				t.Fatalf("ParseGTFSTime(%q) = %d, want an error", c.in, got)
			}
			if !strings.Contains(err.Error(), c.in) {
				t.Errorf("error %q does not name the input %q", err, c.in)
			}
		})
	}
}

func TestServiceDayStart_EachKindOfDay_IsNoonMinusTwelveHours(t *testing.T) {
	cases := []struct {
		name string
		d    time.Time
		want time.Time
	}{
		{"an ordinary day starts at local midnight", plain, time.Date(2026, 9, 23, 0, 0, 0, 0, aest)},
		{"the October transition day starts at 23:00 the evening before", october, time.Date(2026, 10, 3, 23, 0, 0, 0, aest)},
		{"the April transition day starts at 01:00", april, time.Date(2027, 4, 4, 1, 0, 0, 0, aedt)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ServiceDayStart(c.d); !got.Equal(c.want) {
				t.Errorf("ServiceDayStart(%s) = %s, want %s", c.d.Format(time.DateOnly), got, c.want)
			}
		})
	}
}

func TestServiceDayStart_DateInAnyLocation_NamesTheSameDay(t *testing.T) {
	want := ServiceDayStart(october)
	for _, d := range []time.Time{
		time.Date(2026, 10, 4, 0, 0, 0, 0, Loc),
		time.Date(2026, 10, 4, 23, 59, 0, 0, time.FixedZone("UTC-10", -10*3600)),
	} {
		if got := ServiceDayStart(d); !got.Equal(want) {
			t.Errorf("ServiceDayStart(%s) = %s, want %s", d, got, want)
		}
	}
}

func TestAtServiceOffset_AcrossTransitions_CountsRealSeconds(t *testing.T) {
	cases := []struct {
		name    string
		d       time.Time
		seconds int
		want    time.Time
	}{
		{"25:10 on an ordinary day is 01:10 the next morning", plain, 25*hour + 10*60, time.Date(2026, 9, 24, 1, 10, 0, 0, aest)},
		// §9.2 case 3. The day starts at 23:00, so two hours in is 01:00,
		// still before the change; the nonexistent 02:00 is never produced.
		{"02:00 on the October transition is 01:00 AEST", october, 2 * hour, time.Date(2026, 10, 4, 1, 0, 0, 0, aest)},
		{"24:00 on the October transition is midnight AEDT", october, 24 * hour, time.Date(2026, 10, 5, 0, 0, 0, 0, aedt)},
		// §9.2 case 4.
		{"25:00 on the April transition is 01:00 the next day", april, 25 * hour, time.Date(2027, 4, 5, 1, 0, 0, 0, aest)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AtServiceOffset(c.d, c.seconds); !got.Equal(c.want) {
				t.Errorf("AtServiceOffset(%s, %d) = %s, want %s", c.d.Format(time.DateOnly), c.seconds, got, c.want)
			}
		})
	}
}

// §9.2 case 5: on the April transition 02:30 happens twice. They must stay
// two instants, told apart by their offset.
func TestAtServiceOffset_AprilRepeatedHour_GivesTwoDistinctInstants(t *testing.T) {
	first := AtServiceOffset(april, 90*60)
	second := AtServiceOffset(april, 150*60)

	if second.Sub(first) != time.Hour {
		t.Fatalf("instants %s and %s are %s apart, want 1h", first, second, second.Sub(first))
	}
	for _, got := range []time.Time{first, second} {
		if clock := got.In(Loc).Format("15:04"); clock != "02:30" {
			t.Errorf("%s renders locally as %s, want 02:30", got, clock)
		}
	}
	if a, b := first.In(Loc).Format("-07:00"), second.In(Loc).Format("-07:00"); a != "+11:00" || b != "+10:00" {
		t.Errorf("offsets = %s then %s, want +11:00 then +10:00", a, b)
	}
}

func TestCandidateServiceDates_ByTimeOfDay_ReturnsOwnerThenPreviousDay(t *testing.T) {
	const overlap = 6 * time.Hour
	cases := []struct {
		name    string
		at      time.Time
		overlap time.Duration
		want    []time.Time
	}{
		{"mid-morning belongs to today only", time.Date(2026, 9, 23, 9, 0, 0, 0, aest), overlap, []time.Time{plain}},
		// §9.2 case 6: a trip that started 23:50 yesterday is still running.
		{"00:30 is today, then yesterday", time.Date(2026, 9, 23, 0, 30, 0, 0, aest), overlap, []time.Time{plain, date(2026, 9, 22)}},
		{"the last moment inside the overlap still includes yesterday", time.Date(2026, 9, 23, 5, 59, 59, 0, aest), overlap, []time.Time{plain, date(2026, 9, 22)}},
		{"the end of the overlap drops yesterday", time.Date(2026, 9, 23, 6, 0, 0, 0, aest), overlap, []time.Time{plain}},
		{"a zero overlap never includes yesterday", time.Date(2026, 9, 23, 0, 30, 0, 0, aest), 0, []time.Time{plain}},
		{"23:30 before the October change already belongs to the next day", time.Date(2026, 10, 3, 23, 30, 0, 0, aest), overlap, []time.Time{october, date(2026, 10, 3)}},
		{"00:30 on the April change still belongs to the previous day", time.Date(2027, 4, 4, 0, 30, 0, 0, aedt), overlap, []time.Time{date(2027, 4, 3)}},
		{"01:30 on the April change is the new day, then the previous one", time.Date(2027, 4, 4, 1, 30, 0, 0, aedt), overlap, []time.Time{april, date(2027, 4, 3)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CandidateServiceDates(c.at, c.overlap)
			if !sameDates(got, c.want) {
				t.Errorf("CandidateServiceDates(%s, %s) = %v, want %v", c.at, c.overlap, fmtDates(got), fmtDates(c.want))
			}
		})
	}
}

// TZ is documented as affecting log rendering only (§8). The zone an instant
// arrives in must not change its service date.
func TestCandidateServiceDates_InstantInAnyZone_GivesTheSameDates(t *testing.T) {
	at := time.Date(2026, 9, 23, 0, 30, 0, 0, aest)
	want := CandidateServiceDates(at, 6*time.Hour)
	for _, loc := range []*time.Location{time.UTC, time.Local, time.FixedZone("UTC-10", -10*3600)} {
		if got := CandidateServiceDates(at.In(loc), 6*time.Hour); !sameDates(got, want) {
			t.Errorf("in %s: %v, want %v", loc, fmtDates(got), fmtDates(want))
		}
	}
}

func sameDates(a, b []time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

func fmtDates(ds []time.Time) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Format(time.DateOnly)
	}
	return out
}
