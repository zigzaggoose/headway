// Package feed fetches one feed's bytes on a schedule without exceeding the
// upstream quota. It is an HTTP client with a clock: it knows nothing about
// protobuf, GTFS semantics or Postgres, and it never inspects a payload beyond
// checking that one arrived. PROJECT.md §4.2 (2).
package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zigzaggoose/transitlateagain/internal/config"
	"github.com/zigzaggoose/transitlateagain/internal/metrics"
)

// Response is one fetched body. FetchedAt is our clock; the feed's own
// timestamp lives inside the payload and belongs to the decoder.
type Response struct {
	FeedID     string
	Body       []byte
	FetchedAt  time.Time
	StatusCode int
	// LastModified is the response's validator, sent back as If-Modified-Since
	// on the next schedule download. The realtime feeds never set it.
	LastModified string
}

// The outcomes §9.5 requires be told apart. They are sentinels rather than
// status codes because two of them share a status code and differ only by a
// header, and because the caller's response to each is different: one backs
// off this feed, the other stops every feed until the quota resets.
var (
	// ErrRateLimited is HTTP 403 with X-Error-Detail: Account Over Rate Limit.
	// The path is right and we are polling too fast.
	ErrRateLimited = errors.New("upstream rate limit exceeded")

	// ErrQuotaExhausted is HTTP 403 with X-Error-Detail: Account Over Quota
	// Limit. The quota is per account, so this stops every feed, not this one.
	ErrQuotaExhausted = errors.New("upstream daily quota exhausted")

	// ErrUnauthorized is HTTP 401: the key is wrong, expired, or rotated
	// underneath a running process. Never fatal — the key may be replaced
	// without restarting us.
	ErrUnauthorized = errors.New("upstream rejected the api key")

	// ErrNotModified is HTTP 304, a successful poll carrying no new data.
	ErrNotModified = errors.New("not modified")

	// ErrEmptyBody is HTTP 200 with nothing in it. Not an error at the
	// transport layer, but it is one here: there is no feed in an empty body.
	ErrEmptyBody = errors.New("empty body")
)

// StatusError is any other unexpected status.
type StatusError struct {
	StatusCode int
	Detail     string // X-Error-Detail, when the gateway sets one
}

func (e *StatusError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("unexpected status %d (%s)", e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("unexpected status %d", e.StatusCode)
}

// Client fetches feed bodies. One client is shared by every poller: its
// transport pools connections, which matters when the same host is polled
// every fifteen seconds for the life of the process.
type Client struct {
	http      *http.Client
	key       config.Secret
	userAgent string
	now       func() time.Time
}

// NewClient returns a Client whose requests time out after timeout. The
// timeout is on the whole request including the body read, not just the
// headers, because a gateway that stalls mid-body would otherwise hold the
// poller past its next tick.
func NewClient(key config.Secret, timeout time.Duration, now func() time.Time) *Client {
	return &Client{
		http:      &http.Client{Timeout: timeout},
		key:       key,
		userAgent: "transitlateagain/0.1 (+https://github.com/zigzaggoose/transitlateagain)",
		now:       now,
	}
}

// Conditional carries the validators to send on a request. The realtime feeds
// return neither an ETag nor a Last-Modified and answer HEAD with 502, so this
// is only ever populated for the static schedule download (§8.1, §9.5 case 9).
type Conditional struct {
	LastModified string
	ETag         string
}

// Fetch performs one GET. It returns a Response only for a 200 with a body;
// every other outcome is an error the caller can match with errors.Is.
//
// Every upstream request, realtime or schedule, passes through here, so this
// is where the request metrics are recorded.
func (c *Client) Fetch(ctx context.Context, feedID, url string, cond Conditional) (Response, error) {
	start := c.now()
	resp, err := c.fetch(ctx, feedID, url, cond)
	// A request our own shutdown cancelled says nothing about the upstream.
	if ctx.Err() == nil {
		metrics.FeedRequests.WithLabelValues(feedID, outcome(err)).Inc()
		metrics.FeedRequestDuration.WithLabelValues(feedID).Observe(c.now().Sub(start).Seconds())
	}
	return resp, err
}

// outcome is the §10.3 label for a Fetch result.
func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrNotModified):
		return "not_modified"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrQuotaExhausted):
		return "quota"
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "error"
	}
}

func (c *Client) fetch(ctx context.Context, feedID, url string, cond Conditional) (Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Response{}, fmt.Errorf("build request (feed=%s): %w", feedID, err)
	}
	// The documented format is "apikey <TOKEN>", and it goes in a header: as a
	// query parameter it would be logged by every proxy in the path.
	req.Header.Set("Authorization", "apikey "+c.key.Reveal())
	req.Header.Set("User-Agent", c.userAgent)
	if cond.LastModified != "" {
		req.Header.Set("If-Modified-Since", cond.LastModified)
	}
	if cond.ETag != "" {
		req.Header.Set("If-None-Match", cond.ETag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// The URL is in the error but the key is in a header, so this is safe
		// to log. Do not add the request headers to it.
		return Response{}, fmt.Errorf("fetch feed %s: %w", feedID, err)
	}
	defer func() {
		// Drain before close so the connection returns to the pool rather than
		// being torn down: this host is polled every fifteen seconds forever.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	fetchedAt := c.now()
	detail := resp.Header.Get("X-Error-Detail")

	switch resp.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return Response{}, fmt.Errorf("read feed %s body: %w", feedID, err)
		}
		if len(body) == 0 {
			return Response{}, fmt.Errorf("feed %s: %w", feedID, ErrEmptyBody)
		}
		return Response{
			FeedID:       feedID,
			Body:         body,
			FetchedAt:    fetchedAt,
			StatusCode:   resp.StatusCode,
			LastModified: resp.Header.Get("Last-Modified"),
		}, nil

	case http.StatusNotModified:
		return Response{}, fmt.Errorf("feed %s: %w", feedID, ErrNotModified)

	case http.StatusUnauthorized:
		return Response{}, fmt.Errorf("feed %s: %w", feedID, ErrUnauthorized)

	case http.StatusForbidden:
		return Response{}, fmt.Errorf("feed %s: %w", feedID, classify403(detail))

	default:
		return Response{}, fmt.Errorf("feed %s: %w", feedID, &StatusError{StatusCode: resp.StatusCode, Detail: detail})
	}
}

// classify403 tells the two 403s apart. They differ only by a header, and
// confusing them is expensive in both directions: treating a quota stop as a
// rate limit burns the rest of the day's requests on retries, and treating a
// rate limit as a quota stop idles every feed until midnight for what a few
// seconds of backoff would have fixed. An unrecognised 403 is treated as the
// rate limit, which is the recoverable one.
func classify403(detail string) error {
	switch d := strings.ToLower(detail); {
	case strings.Contains(d, "quota"):
		return ErrQuotaExhausted
	case strings.Contains(d, "rate"):
		return ErrRateLimited
	default:
		return ErrRateLimited
	}
}
