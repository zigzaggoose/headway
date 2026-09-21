package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCatalogue(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feeds.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write catalogue: %v", err)
	}
	return path
}

// The committed catalogue is the one that ships; if it stops parsing, the
// service stops starting.
func TestLoadFeeds_CommittedCatalogue_IsValid(t *testing.T) {
	feeds, err := LoadFeeds(filepath.Join("..", "..", "config", "feeds.json"))
	if err != nil {
		t.Fatalf("load committed catalogue: %v", err)
	}
	if len(feeds) == 0 {
		t.Fatal("committed catalogue declares no feeds")
	}
	for _, f := range feeds {
		if !strings.HasPrefix(f.RealtimeURL, "https://api.transport.nsw.gov.au/") {
			t.Errorf("feed %q has an unexpected realtime host: %s", f.ID, f.RealtimeURL)
		}
	}
}

func TestLoadFeeds_Rejects(t *testing.T) {
	const good = `"label":"L","realtime_url":"https://h/r","schedule_url":"https://h/s","route_type":2,"enabled":true`

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "an unknown field, which is how a misspelled key hides",
			body: `{"feeds":[{"id":"a","labl":"L","realtime_url":"https://h/r","schedule_url":"https://h/s","route_type":2,"enabled":true}]}`,
			want: "labl",
		},
		{
			name: "a duplicate id",
			body: `{"feeds":[{"id":"a",` + good + `},{"id":"a",` + good + `}]}`,
			want: "declared twice",
		},
		{
			name: "an empty id",
			body: `{"feeds":[{"id":"",` + good + `}]}`,
			want: "empty id",
		},
		{
			name: "a plain http url, which would put the API key on the wire",
			body: `{"feeds":[{"id":"a","label":"L","realtime_url":"http://h/r","schedule_url":"https://h/s","route_type":2,"enabled":true}]}`,
			want: "non-https",
		},
		{
			name: "a missing schedule url",
			body: `{"feeds":[{"id":"a","label":"L","realtime_url":"https://h/r","schedule_url":"","route_type":2,"enabled":true}]}`,
			want: "empty schedule_url",
		},
		{
			name: "an empty catalogue",
			body: `{"feeds":[]}`,
			want: "declares no feeds",
		},
		{
			name: "malformed json",
			body: `{"feeds":[`,
			want: "parse feed catalogue",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFeeds(writeCatalogue(t, tc.body))
			if err == nil {
				t.Fatal("catalogue accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadFeeds_MissingFile_Errors(t *testing.T) {
	_, err := LoadFeeds(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("missing catalogue accepted")
	}
	if !strings.Contains(err.Error(), "open feed catalogue") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestSelectFeeds(t *testing.T) {
	all := []Feed{
		{ID: "trains", Enabled: true},
		{ID: "ferries", Enabled: false},
		{ID: "metro", Enabled: true},
	}

	t.Run("no list selects the feeds the catalogue enables", func(t *testing.T) {
		got, err := SelectFeeds(all, nil)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if len(got) != 2 || got[0].ID != "trains" || got[1].ID != "metro" {
			t.Errorf("selected %v", ids(got))
		}
	})

	t.Run("a list overrides the enabled flag", func(t *testing.T) {
		got, err := SelectFeeds(all, []string{"ferries"})
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if len(got) != 1 || got[0].ID != "ferries" {
			t.Errorf("selected %v, want [ferries]", ids(got))
		}
	})

	t.Run("an unknown id is an error, not a silent drop", func(t *testing.T) {
		_, err := SelectFeeds(all, []string{"trains", "tarins"})
		if err == nil {
			t.Fatal("unknown feed id accepted")
		}
		if !strings.Contains(err.Error(), "tarins") {
			t.Errorf("error does not quote the typo: %v", err)
		}
	})

	t.Run("a repeated id is an error", func(t *testing.T) {
		if _, err := SelectFeeds(all, []string{"trains", "trains"}); err == nil {
			t.Fatal("repeated feed id accepted")
		}
	})

	t.Run("a catalogue with nothing enabled and no list is an error", func(t *testing.T) {
		if _, err := SelectFeeds([]Feed{{ID: "trains"}}, nil); err == nil {
			t.Fatal("empty selection accepted")
		}
	})
}

func ids(feeds []Feed) []string {
	out := make([]string, len(feeds))
	for i, f := range feeds {
		out[i] = f.ID
	}
	return out
}

// HEADWAY_ENABLED_FEEDS is resolved during Load, so a typo there must fail
// startup rather than quietly reduce the feed set.
func TestLoad_EnabledFeedsTypo_FailsStartup(t *testing.T) {
	env := baseEnv(t)
	env["HEADWAY_ENABLED_FEEDS"] = "sydneytrains, ferrys"
	_, err := loadWith(t, env)
	if err == nil {
		t.Fatal("load accepted an unknown feed id")
	}
	if !strings.Contains(err.Error(), "ferrys") {
		t.Errorf("error does not quote the typo: %v", err)
	}
}

func TestLoad_EnabledFeeds_SelectsNamedFeedsInOrder(t *testing.T) {
	env := baseEnv(t)
	env["HEADWAY_ENABLED_FEEDS"] = "ferries,sydneytrains"
	cfg, err := loadWith(t, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := ids(cfg.Feeds); len(got) != 2 || got[0] != "ferries" || got[1] != "sydneytrains" {
		t.Errorf("feeds = %v", got)
	}
}
