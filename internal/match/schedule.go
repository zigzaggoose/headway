package match

import "time"

// NoTime marks a stop the timetable leaves blank (a non-timepoint).
const NoTime int32 = -1

// StopTime is one scheduled call. Times are seconds from the start of the
// service day and may exceed 86400 (§9.2).
type StopTime struct {
	Seq    int32
	StopID string
	ArrS   int32 // NoTime when blank
	DepS   int32 // NoTime when blank
}

// Trip is one scheduled trip, its calls in ascending stop_sequence.
type Trip struct {
	RouteID     string
	ServiceID   string
	DirectionID *int16
	Headsign    string
	Stops       []StopTime
}

// Route is what the API shows about a route.
type Route struct {
	ShortName string
	LongName  string
	Type      int16 // GTFS route_type
}

type service struct {
	days       [7]bool // indexed by time.Weekday, so Sunday is 0
	start, end string  // YYYY-MM-DD, inclusive; "" when only calendar_dates names the service
	added      map[string]bool
	removed    map[string]bool
}

// Schedule is one feed's timetable in memory. It is built once by the
// store, published to the Matcher, and never modified afterwards, so any
// number of goroutines may read it.
type Schedule struct {
	VersionID int64
	FeedID    string

	trips    map[string]*Trip
	services map[string]*service
	routes   map[string]Route
	stops    map[string]string // stop_id → name
	// Every stop_id string appears in thousands of stop times. Interning
	// keeps one copy of each instead of one allocation per row; on the
	// Sydney Trains bundle that is 1,215 strings instead of 1.24 million.
	stopIDs map[string]string
}

// NewSchedule returns an empty schedule for the builder methods below.
func NewSchedule(versionID int64, feedID string) *Schedule {
	return &Schedule{
		VersionID: versionID,
		FeedID:    feedID,
		trips:     make(map[string]*Trip),
		services:  make(map[string]*service),
		routes:    make(map[string]Route),
		stops:     make(map[string]string),
		stopIDs:   make(map[string]string),
	}
}

// AddRoute records a route.
func (s *Schedule) AddRoute(routeID string, r Route) { s.routes[routeID] = r }

// AddStop records a stop's name.
func (s *Schedule) AddStop(stopID, name string) { s.stops[stopID] = name }

// Route returns a route by id.
func (s *Schedule) Route(routeID string) (Route, bool) {
	r, ok := s.routes[routeID]
	return r, ok
}

// Routes calls fn for every route, in no particular order.
func (s *Schedule) Routes(fn func(id string, r Route)) {
	for id, r := range s.routes {
		fn(id, r)
	}
}

// hasStops is false for a schedule built without stops.txt, where every
// stop_id would otherwise look unknown.
func (s *Schedule) hasStops() bool { return len(s.stops) > 0 }

// Stop returns a stop's name by id.
func (s *Schedule) Stop(stopID string) (string, bool) {
	n, ok := s.stops[stopID]
	return n, ok
}

// AddTrip adds a trip with no stops yet.
func (s *Schedule) AddTrip(tripID string, t Trip) {
	s.trips[tripID] = &t
}

// AddStopTime appends a call to its trip. Calls must arrive in ascending
// stop_sequence per trip. It reports false for a trip that was never added.
func (s *Schedule) AddStopTime(tripID string, st StopTime) bool {
	t, ok := s.trips[tripID]
	if !ok {
		return false
	}
	if id, ok := s.stopIDs[st.StopID]; ok {
		st.StopID = id
	} else {
		s.stopIDs[st.StopID] = st.StopID
	}
	t.Stops = append(t.Stops, st)
	return true
}

// AddCalendar records a calendar.txt row. days runs Monday to Sunday, as the
// file does.
func (s *Schedule) AddCalendar(serviceID string, mondayFirst [7]bool, start, end time.Time) {
	svc := s.service(serviceID)
	for i, on := range mondayFirst {
		svc.days[(i+1)%7] = on
	}
	svc.start, svc.end = start.Format(time.DateOnly), end.Format(time.DateOnly)
}

// AddCalendarDate records a calendar_dates.txt row: 1 adds the date, 2
// removes it.
func (s *Schedule) AddCalendarDate(serviceID string, d time.Time, exceptionType int) {
	svc := s.service(serviceID)
	key := d.Format(time.DateOnly)
	if exceptionType == 1 {
		svc.added[key] = true
	} else {
		svc.removed[key] = true
	}
}

func (s *Schedule) service(id string) *service {
	svc, ok := s.services[id]
	if !ok {
		svc = &service{added: map[string]bool{}, removed: map[string]bool{}}
		s.services[id] = svc
	}
	return svc
}

// Trip returns a trip by id.
func (s *Schedule) Trip(tripID string) (*Trip, bool) {
	t, ok := s.trips[tripID]
	return t, ok
}

// Trips is the number of trips, for logging a load.
func (s *Schedule) Trips() int { return len(s.trips) }

// RunsOn reports whether a service operates on service date d. Exceptions
// in calendar_dates override the weekly pattern in calendar.
func (s *Schedule) RunsOn(serviceID string, d time.Time) bool {
	svc, ok := s.services[serviceID]
	if !ok {
		return false
	}
	key := d.Format(time.DateOnly)
	switch {
	case svc.removed[key]:
		return false
	case svc.added[key]:
		return true
	case svc.start == "":
		return false
	}
	return key >= svc.start && key <= svc.end && svc.days[d.Weekday()]
}

// ActiveShare is the fraction of services running on d. A bundle whose
// calendar no longer covers today scores near zero (§9.1 case 17).
func (s *Schedule) ActiveShare(d time.Time) float64 {
	if len(s.services) == 0 {
		return 0
	}
	n := 0
	for id := range s.services {
		if s.RunsOn(id, d) {
			n++
		}
	}
	return float64(n) / float64(len(s.services))
}
