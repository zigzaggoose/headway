package gtfsstatic

import (
	"archive/zip"
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// bundle builds a zip in memory from file name → contents. Names are sorted
// so the same files always produce the same bytes, and so the same hash.
func bundle(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		body := files[name]
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip: %v", err)
	}
	return buf.Bytes()
}

// rows runs one file through its table's source and returns every row, or
// the first error.
func rows(t *testing.T, file, body string) ([][]any, error) {
	t.Helper()
	b := bundle(t, map[string]string{file: body})
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	for _, tb := range tables(48 * 3600) {
		if tb.file != file {
			continue
		}
		src, closer, err := open(zr.File[0], tb, 7)
		if err != nil {
			return nil, err
		}
		defer closer.Close()
		var out [][]any
		for src.Next() {
			v, _ := src.Values() // Values never fails; Err carries the error
			out = append(out, v)
		}
		return out, src.Err()
	}
	t.Fatalf("no table for %s", file)
	return nil, nil
}

func TestSource_StopTimes_ConvertsToSecondsAndNulls(t *testing.T) {
	got, err := rows(t, "stop_times.txt",
		`"trip_id","arrival_time","departure_time","stop_id","stop_sequence","pickup_type","drop_off_type"
"t1","25:10:00","25:11:30","s1","8","1",""
"t1","","","s2","9","",""
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	first := got[0]
	if first[0] != int64(7) || first[1] != "t1" || first[2] != 8 || first[3] != "s1" {
		t.Errorf("first row = %v", first)
	}
	if a, d := first[4].(*int), first[5].(*int); *a != 90600 || *d != 90690 {
		t.Errorf("times = %d, %d; want 90600, 90690 (past midnight kept as seconds)", *a, *d)
	}
	if first[6] != int16(1) || first[7] != int16(0) {
		t.Errorf("pickup, drop_off = %v, %v; want 1 and the default 0", first[6], first[7])
	}
	if a, d := got[1][4].(*int), got[1][5].(*int); a != nil || d != nil {
		t.Errorf("a non-timepoint stop's times = %v, %v; want NULL", a, d)
	}
}

func TestSource_ColumnsInAnyOrderWithABOM_AreFoundByName(t *testing.T) {
	got, err := rows(t, "routes.txt",
		"\xef\xbb\xbf\"route_type\",\"route_long_name\",\"route_id\"\n\"2\",\"City Circle\",\"APS_1a\"\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := got[0]
	if r[1] != "APS_1a" || r[5] != int16(2) || *r[4].(*string) != "City Circle" {
		t.Errorf("row = %v", r)
	}
	if r[2].(*string) != nil || r[3].(*string) != nil {
		t.Errorf("absent optional columns = %v, %v; want NULL", r[2], r[3])
	}
}

func TestSource_Calendar_ReadsDaysAndDates(t *testing.T) {
	got, err := rows(t, "calendar.txt",
		`"service_id","monday","tuesday","wednesday","thursday","friday","saturday","sunday","start_date","end_date"
"817.158.100","1","0","0","1","1","0","0","20260923","20260925"
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := got[0]
	wantDays := []bool{true, false, false, true, true, false, false}
	for i, w := range wantDays {
		if r[2+i] != w {
			t.Errorf("day %d = %v, want %v", i, r[2+i], w)
		}
	}
	if !r[9].(time.Time).Equal(time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start_date = %v", r[9])
	}
}

// Every rejection names the file and the line, so a bad bundle is found by
// reading the log rather than by bisecting a 1.2-million-line file.
func TestSource_BadRow_ErrorNamesFileAndLine(t *testing.T) {
	const stHeader = `"trip_id","arrival_time","departure_time","stop_id","stop_sequence"` + "\n"
	const ok = `"t1","08:00:00","08:00:00","s1","1"` + "\n"
	cases := []struct {
		name, file, body, want string
	}{
		{"a stop time beyond SERVICE_TIME_MAX_S (§9.2 case 2)", "stop_times.txt", stHeader + ok + `"t1","48:00:01","48:00:01","s2","2"`, "stop_times.txt line 3: arrival_time \"48:00:01\" is beyond"},
		{"a negative stop time (§9.2 case 2)", "stop_times.txt", stHeader + `"t1","-01:00:00","","s2","2"`, "stop_times.txt line 2: arrival_time"},
		{"a malformed stop time", "stop_times.txt", stHeader + `"t1","08:61:00","","s2","2"`, "line 2: arrival_time"},
		{"a missing stop_sequence", "stop_times.txt", stHeader + `"t1","08:00:00","","s2",""`, "line 2: stop_sequence is empty"},
		{"a missing trip_id", "stop_times.txt", stHeader + `"","08:00:00","","s2","2"`, "line 2: trip_id is empty"},
		{"a route without a type", "routes.txt", "route_id,route_type\nR1,\n", "routes.txt line 2: route_type is empty"},
		{"a stop without a name", "stops.txt", "stop_id,stop_name\nS1,\n", "stops.txt line 2: stop_name is empty"},
		{"a calendar day that is not 0 or 1", "calendar.txt", "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\nX,1,1,1,1,1,1,yes,20260101,20260102\n", "calendar.txt line 2: sunday is \"yes\""},
		{"an exception type other than 1 or 2", "calendar_dates.txt", "service_id,date,exception_type\nX,20260101,3\n", "calendar_dates.txt line 2: exception_type is 3"},
		{"an unreadable date", "calendar_dates.txt", "service_id,date,exception_type\nX,2026-01-01,1\n", "calendar_dates.txt line 2: date"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := rows(t, c.file, c.body)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}
