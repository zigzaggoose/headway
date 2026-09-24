package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
)

// Feed is one entry in the feed catalogue. The catalogue is a committed JSON
// file rather than an environment variable because a list of objects does not
// fit one legibly, and it carries no secrets. PROJECT.md §8.1.
//
// A URL in this file is only ever correct if it has been confirmed with a
// single curl against the live API. The TfNSW endpoint set has moved between
// v1 and v2 per mode, and a wrong path returns 404 on every poll.
type Feed struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	RealtimeURL string `json:"realtime_url"`
	ScheduleURL string `json:"schedule_url"`
	RouteType   int    `json:"route_type"`
	Enabled     bool   `json:"enabled"`
}

type catalogue struct {
	Feeds []Feed `json:"feeds"`
}

// LoadFeeds reads and validates the feed catalogue at path.
//
// Unknown JSON fields are rejected. A misspelled key would otherwise be
// discarded in silence, leaving a feed pointed at a zero-valued URL or running
// with "enabled" unset.
func LoadFeeds(path string) ([]Feed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open feed catalogue (%s): %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var c catalogue
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse feed catalogue (%s): %w", path, err)
	}
	if len(c.Feeds) == 0 {
		return nil, fmt.Errorf("feed catalogue (%s) declares no feeds", path)
	}

	var errs []error
	seen := make(map[string]struct{}, len(c.Feeds))
	for i, fd := range c.Feeds {
		where := fmt.Sprintf("feeds[%d]", i)
		switch {
		case fd.ID == "":
			errs = append(errs, fmt.Errorf("%s has an empty id", where))
		default:
			where = fmt.Sprintf("feed %q", fd.ID)
			if _, dup := seen[fd.ID]; dup {
				errs = append(errs, fmt.Errorf("%s is declared twice", where))
			}
			seen[fd.ID] = struct{}{}
		}
		if fd.Label == "" {
			errs = append(errs, fmt.Errorf("%s has an empty label", where))
		}
		if fd.RouteType < 0 {
			errs = append(errs, fmt.Errorf("%s has a negative route_type (%d)", where, fd.RouteType))
		}
		errs = append(errs,
			checkFeedURL(where, "realtime_url", fd.RealtimeURL),
			checkFeedURL(where, "schedule_url", fd.ScheduleURL),
		)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid feed catalogue (%s): %w", path, err)
	}
	return c.Feeds, nil
}

// checkFeedURL requires https: the API key travels in a request header, and
// plain http would put it on the wire in clear.
func checkFeedURL(where, field, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s has an empty %s", where, field)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s has an unparseable %s (%q): %w", where, field, raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%s has a non-https %s (%q)", where, field, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%s has a %s with no host (%q)", where, field, raw)
	}
	return nil
}

// SelectFeeds resolves which feeds to poll. ENABLED_FEEDS, when set,
// replaces the catalogue's "enabled" flags entirely, so an operator can turn a
// feed on for one deployment without editing a committed file. When it is
// unset, every feed marked enabled in the catalogue is polled.
//
// An id that is not in the catalogue is an error rather than a warning: a typo
// would otherwise quietly stop ingestion for that feed and look like an
// upstream outage.
func SelectFeeds(all []Feed, enabled []string) ([]Feed, error) {
	if len(enabled) == 0 {
		var out []Feed
		for _, f := range all {
			if f.Enabled {
				out = append(out, f)
			}
		}
		if len(out) == 0 {
			return nil, errors.New("no feed in the catalogue is enabled and ENABLED_FEEDS is empty")
		}
		return out, nil
	}

	var out []Feed
	var errs []error
	for _, id := range enabled {
		i := slices.IndexFunc(all, func(f Feed) bool { return f.ID == id })
		if i < 0 {
			errs = append(errs, fmt.Errorf("ENABLED_FEEDS names %q, which is not in the feed catalogue", id))
			continue
		}
		if slices.ContainsFunc(out, func(f Feed) bool { return f.ID == id }) {
			errs = append(errs, fmt.Errorf("ENABLED_FEEDS names %q twice", id))
			continue
		}
		out = append(out, all[i])
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}
