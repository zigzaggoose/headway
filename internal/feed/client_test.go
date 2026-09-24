package feed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/config"
)

const testKey = "test-api-key"

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func newTestClient(t *testing.T, h http.HandlerFunc, timeout time.Duration) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(config.Secret(testKey), timeout, time.Now), srv
}

func TestFetch_OK_ReturnsBodyAndOurClock(t *testing.T) {
	const payload = "\x0a\x0dnot-protobuf"
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "apikey "+testKey {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("key or parameters leaked into the query string: %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("If-Modified-Since"); got != "" {
			t.Errorf("sent a conditional header when none was asked for: %q", got)
		}
		w.Write([]byte(payload))
	}, time.Second)

	at := time.Date(2026, 9, 21, 11, 0, 0, 0, time.UTC)
	c.now = fixedClock(at)

	resp, err := c.Fetch(context.Background(), "sydneytrains", srv.URL, Conditional{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(resp.Body) != payload {
		t.Errorf("body = %q", resp.Body)
	}
	if resp.FeedID != "sydneytrains" || resp.StatusCode != 200 {
		t.Errorf("response = %+v", resp)
	}
	// FetchedAt is our clock. The feed's own timestamp is inside the payload
	// and is not this package's business.
	if !resp.FetchedAt.Equal(at) {
		t.Errorf("FetchedAt = %s, want %s", resp.FetchedAt, at)
	}
}

func TestFetch_Conditional_SendsValidators(t *testing.T) {
	var gotIMS, gotINM string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotIMS = r.Header.Get("If-Modified-Since")
		gotINM = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}, time.Second)

	_, err := c.Fetch(context.Background(), "sydneytrains", srv.URL,
		Conditional{LastModified: "Sun, 20 Sep 2026 15:01:17 GMT", ETag: `W/"abc"`})
	if !errors.Is(err, ErrNotModified) {
		t.Fatalf("err = %v, want ErrNotModified", err)
	}
	if gotIMS != "Sun, 20 Sep 2026 15:01:17 GMT" || gotINM != `W/"abc"` {
		t.Errorf("validators not sent: ims=%q inm=%q", gotIMS, gotINM)
	}
}

// The schedule download sends this back as If-Modified-Since the next day, so
// losing it would re-download 10.7 MB daily for nothing.
func TestFetch_OK_ReturnsTheLastModifiedValidator(t *testing.T) {
	const lm = "Tue, 22 Sep 2026 15:01:13 GMT"
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", lm)
		w.Write([]byte("zip"))
	}, time.Second)

	resp, err := c.Fetch(context.Background(), "sydneytrains", srv.URL, Conditional{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if resp.LastModified != lm {
		t.Errorf("LastModified = %q, want %q", resp.LastModified, lm)
	}
}

// The status cases §11.2 requires, including the two 403s that differ only by
// a header and mean opposite things.
func TestFetch_StatusCases(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		detail  string
		body    string
		want    error
		wantMsg string
	}{
		{name: "200 with an empty body is not a successful poll", status: 200, want: ErrEmptyBody},
		{name: "304 not modified", status: 304, want: ErrNotModified},
		{name: "401 unauthorized", status: 401, want: ErrUnauthorized},
		{name: "403 over the per-second throttle", status: 403, detail: "Account Over Rate Limit", want: ErrRateLimited},
		{name: "403 over the daily quota", status: 403, detail: "Account Over Quota Limit", want: ErrQuotaExhausted},
		{name: "403 with no detail is treated as the recoverable one", status: 403, want: ErrRateLimited},
		{name: "500 from the gateway", status: 500, wantMsg: "unexpected status 500"},
		{name: "502, which is what HEAD returns upstream", status: 502, wantMsg: "unexpected status 502"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.detail != "" {
					w.Header().Set("X-Error-Detail", tc.detail)
				}
				w.WriteHeader(tc.status)
				if tc.body != "" {
					w.Write([]byte(tc.body))
				}
			}, time.Second)

			_, err := c.Fetch(context.Background(), "sydneytrains", srv.URL, Conditional{})
			if err == nil {
				t.Fatal("fetch succeeded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantMsg)
			}
			if !strings.Contains(err.Error(), "sydneytrains") {
				t.Errorf("err does not name the feed: %v", err)
			}
		})
	}
}

// A truncated or malformed body is not this package's problem: the decoder
// owns payload validity. The client's contract is "bytes arrived".
func TestFetch_TruncatedBody_IsReturnedForTheDecoderToReject(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte{0x0a, 0x0d, 0x0a})
	}, time.Second)

	resp, err := c.Fetch(context.Background(), "sydneytrains", srv.URL, Conditional{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(resp.Body) != 3 {
		t.Errorf("body = %v", resp.Body)
	}
}

func TestFetch_HangPastTimeout_FailsWithDeadline(t *testing.T) {
	release := make(chan struct{})
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
	}, 30*time.Millisecond)
	t.Cleanup(func() { close(release) })

	start := time.Now()
	_, err := c.Fetch(context.Background(), "sydneytrains", srv.URL, Conditional{})
	if err == nil {
		t.Fatal("fetch succeeded against a hung server")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s, want the configured timeout", elapsed)
	}
}

func TestFetch_CancelledContext_ReturnsPromptly(t *testing.T) {
	release := make(chan struct{})
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { <-release }, time.Minute)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	_, err := c.Fetch(ctx, "sydneytrains", srv.URL, Conditional{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// PROJECT.md §8.2: the key is never logged. An error is a log line waiting to
// happen, so it must not carry the key either.
func TestFetch_Errors_DoNotContainTheKey(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Error-Detail", "Account Over Rate Limit")
		w.WriteHeader(403)
	}, time.Second)

	_, err := c.Fetch(context.Background(), "sydneytrains", srv.URL, Conditional{})
	if err == nil {
		t.Fatal("fetch succeeded")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error leaks the api key: %v", err)
	}
}
