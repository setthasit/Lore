package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	requestTimeout = 60 * time.Second

	// defaultMaxAttempts counts the first try, so four retries follow it.
	defaultMaxAttempts = 5

	defaultBaseBackoff = time.Second
	maxBackoff         = 30 * time.Second

	// maxRetryWait caps a server-requested delay; a longer one ends the sync round.
	maxRetryWait = 2 * time.Minute

	maxResponseBytes = 32 << 20
	maxErrorBody     = 512

	searchPageSize  = 50
	commentPageSize = 100

	searchFields = "summary,description,created,updated,reporter"
)

// Jira Cloud renders a colonless numeric offset at millisecond precision, which
// time.RFC3339 cannot parse.
const jiraTimeLayout = "2006-01-02T15:04:05.000-0700"

type timestamp struct{ time.Time }

func (t *timestamp) UnmarshalJSON(payload []byte) error {
	var raw string
	if err := json.Unmarshal(payload, &raw); err != nil {
		return err
	}
	if raw == "" {
		t.Time = time.Time{}
		return nil
	}
	at, err := time.Parse(jiraTimeLayout, raw)
	if err != nil {
		return fmt.Errorf("parse jira timestamp %q: %w", raw, err)
	}
	t.Time = at
	return nil
}

// client is the Jira Cloud transport. The credentials only ever reach the
// Authorization header: nothing here logs, and no error message carries a header.
type client struct {
	http        *http.Client
	baseURL     string
	basicAuth   string
	maxAttempts int
	baseBackoff time.Duration
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
}

func newClient(baseURL, email, token string) *client {
	return &client{
		http:        &http.Client{Timeout: requestTimeout},
		baseURL:     baseURL,
		basicAuth:   "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token)),
		maxAttempts: defaultMaxAttempts,
		baseBackoff: defaultBaseBackoff,
		now:         time.Now,
		sleep:       sleepCtx,
	}
}

type retryableError struct {
	err   error
	after time.Duration // server-requested delay; zero means exponential backoff
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func (c *client) get(ctx context.Context, endpoint string, out any) error {
	for attempt := 1; ; attempt++ {
		payload, err := c.attempt(ctx, endpoint)
		if err == nil {
			if err := json.Unmarshal(payload, out); err != nil {
				return fmt.Errorf("decode %s response: %w", endpoint, err)
			}
			return nil
		}

		var retry *retryableError
		if !errors.As(err, &retry) {
			return err
		}
		if attempt >= c.maxAttempts {
			return fmt.Errorf("gave up after %d attempts: %w", attempt, retry.err)
		}
		wait := retry.after
		if wait <= 0 {
			wait = backoff(c.baseBackoff, attempt)
		}
		if wait > maxRetryWait {
			return fmt.Errorf("requested retry delay %s exceeds %s: %w", wait, maxRetryWait, retry.err)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (c *client) attempt(ctx context.Context, endpoint string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET %s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", c.basicAuth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("GET %s: %w", endpoint, err)
		}
		return nil, &retryableError{err: fmt.Errorf("GET %s: %w", endpoint, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, &retryableError{err: fmt.Errorf("read GET %s response: %w", endpoint, readErr)}
	}
	if resp.StatusCode == http.StatusOK {
		return payload, nil
	}

	statusErr := fmt.Errorf("GET %s: status %d: %s", endpoint, resp.StatusCode, apiMessage(payload))
	if retryableStatus(resp.StatusCode) {
		return nil, &retryableError{err: statusErr, after: retryDelay(resp.Header, c.now())}
	}
	return nil, statusErr
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// X-RateLimit-* is inconsistently present on Jira Cloud, so Retry-After is the
// only trustworthy delay; zero means back off exponentially.
func retryDelay(header http.Header, now time.Time) time.Duration {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil {
		return at.Sub(now)
	}
	return 0
}

func backoff(base time.Duration, attempt int) time.Duration {
	d := base << (attempt - 1)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Jira answers with {"errorMessages": […]}; anything else is truncated so an HTML
// error page cannot flood an error string.
func apiMessage(payload []byte) string {
	var body struct {
		ErrorMessages []string `json:"errorMessages"`
	}
	if err := json.Unmarshal(payload, &body); err == nil && len(body.ErrorMessages) > 0 {
		return truncate(body.ErrorMessages[0])
	}
	return truncate(strings.TrimSpace(string(payload)))
}

func truncate(s string) string {
	if len(s) <= maxErrorBody {
		return s
	}
	return s[:maxErrorBody] + "…"
}

type searchPage struct {
	IsLast        bool    `json:"isLast"`
	Issues        []issue `json:"issues"`
	NextPageToken string  `json:"nextPageToken"`
}

type issue struct {
	Key    string      `json:"key"`
	Fields issueFields `json:"fields"`
}

type issueFields struct {
	Summary     string    `json:"summary"`
	Description *adfNode  `json:"description"`
	Created     timestamp `json:"created"`
	Updated     timestamp `json:"updated"`
	Reporter    *user     `json:"reporter"`
}

type user struct {
	DisplayName string `json:"displayName"`
}

func (u *user) name() string {
	if u == nil {
		return ""
	}
	return u.DisplayName
}

type commentPage struct {
	Comments []comment `json:"comments"`
	Total    int       `json:"total"`
}

type comment struct {
	ID      string    `json:"id"`
	Body    *adfNode  `json:"body"`
	Created timestamp `json:"created"`
	Updated timestamp `json:"updated"`
	Author  *user     `json:"author"`
}

// isLast, not an absent token, ends the walk: the envelope carries a token only
// while more pages remain.
func (c *client) eachIssue(ctx context.Context, jql string, visit func(*issue) bool) error {
	for token := ""; ; {
		query := url.Values{
			"jql":        {jql},
			"fields":     {searchFields},
			"maxResults": {strconv.Itoa(searchPageSize)},
		}
		if token != "" {
			query.Set("nextPageToken", token)
		}

		var page searchPage
		if err := c.get(ctx, c.baseURL+"/rest/api/3/search/jql?"+query.Encode(), &page); err != nil {
			return err
		}
		for i := range page.Issues {
			if !visit(&page.Issues[i]) {
				return nil
			}
		}
		if page.IsLast || page.NextPageToken == "" {
			return nil
		}
		token = page.NextPageToken
	}
}

// The comment endpoint pages by offset rather than by token, and caps maxResults
// server-side, so the walk advances by what each page actually returned.
func (c *client) comments(ctx context.Context, key string) ([]comment, error) {
	var all []comment
	for startAt := 0; ; {
		query := url.Values{
			"startAt":    {strconv.Itoa(startAt)},
			"maxResults": {strconv.Itoa(commentPageSize)},
			"orderBy":    {"created"},
		}
		endpoint := c.baseURL + "/rest/api/3/issue/" + url.PathEscape(key) + "/comment?" + query.Encode()

		var page commentPage
		if err := c.get(ctx, endpoint, &page); err != nil {
			return nil, fmt.Errorf("comments of %s: %w", key, err)
		}
		all = append(all, page.Comments...)
		startAt += len(page.Comments)
		if len(page.Comments) == 0 || startAt >= page.Total {
			return all, nil
		}
	}
}
