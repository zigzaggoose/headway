package gtfsstatic

import (
	"archive/zip"
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/zigzaggoose/headway/internal/servicetime"
)

// table describes one GTFS file and the table it loads into. convert turns a
// record, addressed by column name, into the table's values after version_id.
type table struct {
	file     string
	name     string
	columns  []string // the table's columns, version_id first
	required bool
	convert  func(row) ([]any, error)
}

// row gives a record's fields by header name. GTFS allows columns in any
// order and allows optional ones to be absent, so position means nothing.
type row struct {
	rec []string
	idx map[string]int
}

func (r row) str(col string) string {
	if i, ok := r.idx[col]; ok && i < len(r.rec) {
		return r.rec[i]
	}
	return ""
}

// null is str with "" as NULL: in GTFS an empty optional field is absent,
// not an empty string.
func (r row) null(col string) *string {
	if s := r.str(col); s != "" {
		return &s
	}
	return nil
}

func (r row) need(col string) (string, error) {
	if s := r.str(col); s != "" {
		return s, nil
	}
	return "", fmt.Errorf("%s is empty", col)
}

func (r row) int(col string, def int) (int, error) {
	s := r.str(col)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", col, err)
	}
	return n, nil
}

func (r row) optInt(col string) (*int, error) {
	if r.str(col) == "" {
		return nil, nil
	}
	n, err := r.int(col, 0)
	return &n, err
}

func (r row) float(col string) (*float64, error) {
	s := r.str(col)
	if s == "" {
		return nil, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", col, err)
	}
	return &f, nil
}

func (r row) date(col string) (time.Time, error) {
	s, err := r.need(col)
	if err != nil {
		return time.Time{}, err
	}
	d, err := time.Parse("20060102", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", col, err)
	}
	return d, nil
}

// stopTime is a GTFS time as seconds from the service day's start, or NULL
// when empty: GTFS leaves non-timepoint stops blank.
func (r row) stopTime(col string, maxS int) (*int, error) {
	s := r.str(col)
	if s == "" {
		return nil, nil
	}
	n, err := servicetime.ParseGTFSTime(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", col, err)
	}
	if n > maxS {
		return nil, fmt.Errorf("%s %q is beyond SERVICE_TIME_MAX_S (%d)", col, s, maxS)
	}
	return &n, nil
}

// tables lists what Headway loads. The timetable tables reference only
// schedule_versions, not each other, so their order does not matter.
// Everything else in the bundle (shapes, occupancies, TfNSW's vehicle files)
// is skipped: occupancies alone is 48 MB of data nothing here reads.
func tables(maxStopTimeS int) []table {
	return []table{
		{file: "routes.txt", name: "routes", required: true,
			columns: []string{"version_id", "route_id", "agency_id", "short_name", "long_name", "route_type"},
			convert: func(r row) ([]any, error) {
				id, err := r.need("route_id")
				if err != nil {
					return nil, err
				}
				rt, err := r.int("route_type", -1)
				if err != nil {
					return nil, err
				}
				if rt < 0 {
					return nil, errors.New("route_type is empty")
				}
				return []any{id, r.null("agency_id"), r.null("route_short_name"), r.null("route_long_name"), int16(rt)}, nil
			}},
		{file: "stops.txt", name: "stops", required: true,
			columns: []string{"version_id", "stop_id", "stop_code", "name", "lat", "lon", "parent_station", "location_type"},
			convert: func(r row) ([]any, error) {
				id, err := r.need("stop_id")
				if err != nil {
					return nil, err
				}
				name, err := r.need("stop_name")
				if err != nil {
					return nil, err
				}
				lat, err := r.float("stop_lat")
				if err != nil {
					return nil, err
				}
				lon, err := r.float("stop_lon")
				if err != nil {
					return nil, err
				}
				lt, err := r.int("location_type", 0)
				if err != nil {
					return nil, err
				}
				return []any{id, r.null("stop_code"), name, lat, lon, r.null("parent_station"), int16(lt)}, nil
			}},
		{file: "trips.txt", name: "trips", required: true,
			columns: []string{"version_id", "trip_id", "route_id", "service_id", "direction_id", "headsign"},
			convert: func(r row) ([]any, error) {
				var vals [3]string
				for i, c := range []string{"trip_id", "route_id", "service_id"} {
					v, err := r.need(c)
					if err != nil {
						return nil, err
					}
					vals[i] = v
				}
				dir, err := r.optInt("direction_id")
				if err != nil {
					return nil, err
				}
				return []any{vals[0], vals[1], vals[2], dir, r.null("trip_headsign")}, nil
			}},
		{file: "stop_times.txt", name: "stop_times", required: true,
			columns: []string{"version_id", "trip_id", "stop_sequence", "stop_id", "arrival_s", "departure_s", "pickup_type", "drop_off_type"},
			convert: func(r row) ([]any, error) {
				tripID, err := r.need("trip_id")
				if err != nil {
					return nil, err
				}
				stopID, err := r.need("stop_id")
				if err != nil {
					return nil, err
				}
				seq, err := r.int("stop_sequence", -1)
				if err != nil {
					return nil, err
				}
				if seq < 0 {
					return nil, errors.New("stop_sequence is empty")
				}
				arr, err := r.stopTime("arrival_time", maxStopTimeS)
				if err != nil {
					return nil, err
				}
				dep, err := r.stopTime("departure_time", maxStopTimeS)
				if err != nil {
					return nil, err
				}
				pu, err := r.int("pickup_type", 0)
				if err != nil {
					return nil, err
				}
				do, err := r.int("drop_off_type", 0)
				if err != nil {
					return nil, err
				}
				return []any{tripID, seq, stopID, arr, dep, int16(pu), int16(do)}, nil
			}},
		// calendar.txt and calendar_dates.txt are each optional in GTFS as long
		// as one is present; the Sydney Trains bundle has no calendar_dates.
		{file: "calendar.txt", name: "calendar",
			columns: []string{"version_id", "service_id", "mon", "tue", "wed", "thu", "fri", "sat", "sun", "start_date", "end_date"},
			convert: func(r row) ([]any, error) {
				id, err := r.need("service_id")
				if err != nil {
					return nil, err
				}
				out := []any{id}
				for _, d := range []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"} {
					v := r.str(d)
					if v != "0" && v != "1" {
						return nil, fmt.Errorf("%s is %q, want 0 or 1", d, v)
					}
					out = append(out, v == "1")
				}
				for _, d := range []string{"start_date", "end_date"} {
					t, err := r.date(d)
					if err != nil {
						return nil, err
					}
					out = append(out, t)
				}
				return out, nil
			}},
		{file: "calendar_dates.txt", name: "calendar_dates",
			columns: []string{"version_id", "service_id", "service_date", "exception_type"},
			convert: func(r row) ([]any, error) {
				id, err := r.need("service_id")
				if err != nil {
					return nil, err
				}
				d, err := r.date("date")
				if err != nil {
					return nil, err
				}
				et, err := r.int("exception_type", 0)
				if err != nil {
					return nil, err
				}
				if et != 1 && et != 2 {
					return nil, fmt.Errorf("exception_type is %d, want 1 or 2", et)
				}
				return []any{id, d, int16(et)}, nil
			}},
	}
}

// source streams one zipped CSV into COPY without holding it in memory:
// stop_times.txt is 114 MB uncompressed and the VM has 1 GB. It implements
// pgx.CopyFromSource.
type source struct {
	t       table
	version int64
	r       *csv.Reader
	idx     map[string]int
	line    int
	vals    []any
	err     error
}

func open(f *zip.File, t table, version int64) (*source, io.Closer, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", t.file, err)
	}
	// A UTF-8 byte order mark is legal at the start of a GTFS file. It has to
	// go before the CSV reader sees it: in front of a quoted header it is a
	// parse error, not just an odd first column name.
	br := bufio.NewReader(rc)
	if bom, _ := br.Peek(3); string(bom) == "\xef\xbb\xbf" { // a short file just has no BOM
		_, _ = br.Discard(3) // cannot fail: Peek has already buffered these bytes
	}
	r := csv.NewReader(br)
	r.ReuseRecord = true
	// GTFS does not promise every row has every column.
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		_ = rc.Close() // the header error is the one worth reporting
		return nil, nil, fmt.Errorf("%s header: %w", t.file, err)
	}
	idx := make(map[string]int, len(header))
	for i, h := range header {
		idx[h] = i
	}
	return &source{t: t, version: version, r: r, idx: idx, line: 1}, rc, nil
}

func (s *source) Next() bool {
	rec, err := s.r.Read()
	if err == io.EOF {
		return false
	}
	s.line++
	if err != nil {
		s.err = fmt.Errorf("%s line %d: %w", s.t.file, s.line, err)
		return false
	}
	vals, err := s.t.convert(row{rec: rec, idx: s.idx})
	if err != nil {
		s.err = fmt.Errorf("%s line %d: %w", s.t.file, s.line, err)
		return false
	}
	s.vals = append([]any{s.version}, vals...)
	return true
}

func (s *source) Values() ([]any, error) { return s.vals, nil }
func (s *source) Err() error             { return s.err }
