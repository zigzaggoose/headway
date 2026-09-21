package config

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// loader reads typed values out of a lookup function, recording a problem
// rather than returning one so that a single startup attempt reports every
// mistake. A value that fails to parse falls back to its default, which keeps
// the rest of validation meaningful instead of cascading into nonsense.
type loader struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (l *loader) raw(key string) (string, bool) {
	v, ok := l.lookup(key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (l *loader) bad(key, value, want string) {
	l.errs = append(l.errs, fmt.Errorf("%s is %q, want %s", key, value, want))
}

func (l *loader) required(key string) string {
	v, ok := l.raw(key)
	if !ok {
		l.errs = append(l.errs, fmt.Errorf("%s is required and is not set", key))
	}
	return v
}

func (l *loader) str(key, def string) string {
	if v, ok := l.raw(key); ok {
		return v
	}
	return def
}

func (l *loader) list(key string) []string {
	v, ok := l.raw(key)
	if !ok {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (l *loader) dur(key string, def time.Duration) time.Duration {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.bad(key, v, `a duration such as "15s", "2m" or "24h"`)
		return def
	}
	return d
}

func (l *loader) int(key string, def int) int {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.bad(key, v, "a whole number")
		return def
	}
	return n
}

func (l *loader) float(key string, def float64) float64 {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.bad(key, v, "a number")
		return def
	}
	return f
}

func (l *loader) boolean(key string, def bool) bool {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.bad(key, v, "true or false")
		return def
	}
	return b
}

func (l *loader) enum(key, def string, allowed ...string) string {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	for _, a := range allowed {
		if strings.EqualFold(v, a) {
			return a
		}
	}
	l.bad(key, v, "one of "+strings.Join(allowed, ", "))
	return def
}

func (l *loader) level(key string, def slog.Level) slog.Level {
	v, ok := l.raw(key)
	if !ok {
		return def
	}
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(v)); err != nil {
		l.bad(key, v, "one of debug, info, warn, error")
		return def
	}
	return lv
}

// timeOfDay parses "HH:MM" in local time. Minutes are not optional: "3" is
// more likely a mistake than an intent to run at 03:00.
func (l *loader) timeOfDay(key string, defHour, defMin int) (hour, minute int) {
	v, ok := l.raw(key)
	if !ok {
		return defHour, defMin
	}
	t, err := time.Parse("15:04", v)
	if err != nil {
		l.bad(key, v, `a local time of day such as "03:30"`)
		return defHour, defMin
	}
	return t.Hour(), t.Minute()
}
