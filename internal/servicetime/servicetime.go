// Package servicetime is GTFS service-day arithmetic in Australia/Sydney: stop
// times that exceed 24:00:00, and service days that are 23 or 25 hours long.
// PROJECT.md §9.2.
//
// A service date is a time.Time whose calendar date, read in its own location,
// names the day. Returned dates are midnight UTC so they format and compare
// the same everywhere; nothing here reads the process's local zone.
package servicetime

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Loc is Australia/Sydney, loaded once at init.
var Loc *time.Location

func init() {
	var err error
	Loc, err = time.LoadLocation("Australia/Sydney")
	if err != nil {
		// Running on in UTC would shift every service date by ten or eleven
		// hours without a single error, so refuse to run at all. main imports
		// time/tzdata so the distroless image cannot reach this.
		panic(fmt.Sprintf("servicetime: load Australia/Sydney: %v", err))
	}
}

// ParseGTFSTime turns "25:10:00" into 90600. Hours above 23 are legal, and
// unpadded fields ("7:5:00") are tolerated because some producers emit them.
// The upper bound is SERVICE_TIME_MAX_S, which the schedule loader applies
// because only it knows the row number to report.
func ParseGTFSTime(s string) (seconds int, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("parse gtfs time %q: want HH:MM:SS", s)
	}
	var n [3]int
	for i, p := range parts {
		// Atoi alone would accept "+5" and "-1"; a stop time has neither.
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return 0, fmt.Errorf("parse gtfs time %q: field %q is not a number", s, p)
		}
		if n[i], err = strconv.Atoi(p); err != nil {
			return 0, fmt.Errorf("parse gtfs time %q: %w", s, err)
		}
	}
	if n[1] > 59 || n[2] > 59 {
		return 0, fmt.Errorf("parse gtfs time %q: minutes and seconds must be below 60", s)
	}
	return n[0]*3600 + n[1]*60 + n[2], nil
}

// ServiceDayStart returns the instant GTFS measures stop times from: noon on
// the service date, minus twelve hours. On most days that is local midnight.
// On the October transition it is 23:00 the previous evening, and on the April
// transition it is 01:00, because noon is on the far side of the clock change
// and the twelve hours are counted in real time. Deriving it from local
// midnight is what breaks on those two days.
func ServiceDayStart(d time.Time) time.Time {
	y, m, day := d.Date()
	return time.Date(y, m, day, 12, 0, 0, 0, Loc).Add(-12 * time.Hour)
}

// AtServiceOffset converts (service date, seconds) into an absolute instant.
// Add counts real seconds, so a clock change inside the offset needs no
// special case.
func AtServiceOffset(serviceDate time.Time, seconds int) time.Time {
	return ServiceDayStart(serviceDate).Add(time.Duration(seconds) * time.Second)
}

// CandidateServiceDates returns the service dates instant t could belong to,
// newest first. The first is the day whose service had most recently started
// at t. The day before joins it while t is within overlap of that start,
// because a trip that began late the previous evening is still running.
//
// Ownership goes by ServiceDayStart, not by the local calendar date, which
// differs for an hour a year in each direction: 23:30 before the October
// change already belongs to the next day, and 00:30 on the April change
// still belongs to the previous one.
func CandidateServiceDates(t time.Time, overlap time.Duration) []time.Time {
	y, m, d := t.In(Loc).Date()
	// The owner is tomorrow, today or yesterday by calendar date, and
	// yesterday's service always started before t, so the loop ends.
	for i := 1; ; i-- {
		owner := time.Date(y, m, d+i, 0, 0, 0, 0, time.UTC)
		start := ServiceDayStart(owner)
		if start.After(t) {
			continue
		}
		if t.Sub(start) < overlap {
			return []time.Time{owner, owner.AddDate(0, 0, -1)}
		}
		return []time.Time{owner}
	}
}
