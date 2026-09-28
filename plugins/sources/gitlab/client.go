package gitlab

import (
	"context"
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

	// pageSize is GitLab's documented maximum for offset pagination.
	pageSize = 100

	apiPath = "/api/v4"
)

// client is the GitLab transport. The token only ever reaches the PRIVATE-TOKEN
// header: nothing here logs, and no error message carries a header.
type client struct {
	http        *http.Client
	apiBase     string // "<root>/api/v4"
	token       string
	maxAttempts int
	baseBackoff time.Duration
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
}

func newClient(root, token string) *client {
	return &client{
		http:        &http.Client{Timeout: requestTimeout},
		apiBase:     root + apiPath,
		token:       token,
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

// get decodes a 200 body into out and returns the response header, which carries
// the pagination cursor.
func (c *client) get(ctx context.Context, endpoint string, out any) (http.Header, error) {
	for attempt := 1; ; attempt++ {
		payload, header, err := c.attempt(ctx, endpoint)
		if err == nil {
			if err := json.Unmarshal(payload, out); err != nil {
				return nil, fmt.Errorf("decode %s response: %w", redact(endpoint), err)
			}
			return header, nil
		}

		var retry *retryableError
		if !errors.As(err, &retry) {
			return nil, err
		}
		if attempt >= c.maxAttempts {
			return nil, fmt.Errorf("gave up after %d attempts: %w", attempt, retry.err)
		}
		wait := retry.after
		if wait <= 0 {
			wait = backoff(c.baseBackoff, attempt)
		}
		if wait > maxRetryWait {
			return nil, fmt.Errorf("requested retry delay %s exceeds %s: %w", wait, maxRetryWait, retry.err)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func (c *client) attempt(ctx context.Context, endpoint string) ([]byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build GET %s: %w", redact(endpoint), err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("GET %s: %w", redact(endpoint), err)
		}
		return nil, nil, &retryableError{err: fmt.Errorf("GET %s: %w", redact(endpoint), err)}
	}
	defer func() { _ = resp.Body.Close() }()

	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, nil, &retryableError{err: fmt.Errorf("read GET %s response: %w", redact(endpoint), readErr)}
	}
	if resp.StatusCode == http.StatusOK {
		return payload, resp.Header, nil
	}

	statusErr := fmt.Errorf("GET %s: status %d: %s", redact(endpoint), resp.StatusCode, apiMessage(payload))
	if retryableStatus(resp.StatusCode) {
		return nil, nil, &retryableError{err: statusErr, after: retryDelay(resp.Header, c.now())}
	}
	return nil, nil, statusErr
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// RateLimit-Reset is an epoch second on GitLab.com and absent on many
// self-managed instances, so Retry-After is the only portable delay; zero means
// back off exponentially.
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

// A private_token query parameter is a documented alternative to the header, so
// an endpoint echoed into an error is scrubbed before it can carry a credential.
func redact(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "the requested URL"
	}
	query := parsed.Query()
	for _, secret := range []string{"private_token", "access_token"} {
		if query.Has(secret) {
			query.Set(secret, "REDACTED")
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// GitLab answers with {"message": …} for most failures and {"error": …} for
// OAuth-scope failures; anything else is truncated so an HTML error page cannot
// flood an error string.
func apiMessage(payload []byte) string {
	var body struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
	}
	if err := json.Unmarshal(payload, &body); err == nil {
		if text, ok := messageText(body.Message); ok {
			return truncate(text)
		}
		if body.Error != "" {
			return truncate(body.Error)
		}
	}
	return truncate(strings.TrimSpace(string(payload)))
}

// "message" is a string on a routing failure and an object of field errors on a
// validation failure, so the object is reported verbatim rather than guessed at.
func messageText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, text != ""
	}
	return string(raw), true
}

func truncate(s string) string {
	if len(s) <= maxErrorBody {
		return s
	}
	return s[:maxErrorBody] + "…"
}

type author struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

// A note left by a deleted user has no author at all.
func (a *author) display() string {
	switch {
	case a == nil:
		return ""
	case a.Username != "":
		return a.Username
	}
	return a.Name
}

type mergeRequest struct {
	IID             int       `json:"iid"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	State           string    `json:"state"`
	Author          *author   `json:"author"`
	WebURL          string    `json:"web_url"`
	SourceBranch    string    `json:"source_branch"`
	SHA             string    `json:"sha"`
	MergeCommitSHA  string    `json:"merge_commit_sha"`
	SquashCommitSHA string    `json:"squash_commit_sha"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type issue struct {
	IID         int       `json:"iid"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	State       string    `json:"state"`
	Author      *author   `json:"author"`
	WebURL      string    `json:"web_url"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// individual_note distinguishes a standalone comment from a resolvable thread;
// a thread's first note opened it and the rest are replies.
type discussion struct {
	ID             string `json:"id"`
	IndividualNote bool   `json:"individual_note"`
	Notes          []note `json:"notes"`
}

type note struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"` // DiffNote | DiscussionNote | null
	Body      string    `json:"body"`
	Author    *author   `json:"author"`
	System    bool      `json:"system"`
	Resolved  bool      `json:"resolved"`
	Position  *position `json:"position"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// position anchors a diff note. Only the paths matter here: line numbers belong
// to a diff version the index does not store.
type position struct {
	NewPath string `json:"new_path"`
	OldPath string `json:"old_path"`
}

// paths lists the file the note is anchored to, plus the pre-rename path when
// the note straddles a rename.
func (p *position) paths() []string {
	if p == nil {
		return nil
	}
	if p.OldPath == "" || p.OldPath == p.NewPath {
		return []string{p.NewPath}
	}
	if p.NewPath == "" {
		return []string{p.OldPath}
	}
	return []string{p.NewPath, p.OldPath}
}

// path is the file a diff note reads as being about: the post-change path, or
// the pre-change one when the file was deleted.
func (p *position) path() string {
	paths := p.paths()
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

type commit struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Message       string    `json:"message"`
	AuthorName    string    `json:"author_name"`
	WebURL        string    `json:"web_url"`
	AuthoredDate  time.Time `json:"authored_date"`
	CommittedDate time.Time `json:"committed_date"`
}

type diff struct {
	NewPath     string `json:"new_path"`
	OldPath     string `json:"old_path"`
	RenamedFile bool   `json:"renamed_file"`
}

// paged walks GitLab's offset pagination and returns every element. Callers
// buffer whole collections anyway: a unit is only emitted once its dependents
// have been fetched.
func paged[T any](ctx context.Context, c *client, path string, query url.Values) ([]T, error) {
	var all []T
	for page := 1; page > 0; {
		q := url.Values{}
		for key, values := range query {
			q[key] = values
		}
		q.Set("per_page", strconv.Itoa(pageSize))
		q.Set("page", strconv.Itoa(page))

		var batch []T
		header, err := c.get(ctx, c.apiBase+path+"?"+q.Encode(), &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		page = nextPage(header, page, len(batch))
	}
	return all, nil
}

// X-Next-Page is empty on the last page. GitLab omits the pagination headers
// entirely once a query is too expensive to count, so a full page is the
// remaining evidence that another one exists.
func nextPage(header http.Header, page, got int) int {
	if raw, ok := header["X-Next-Page"]; ok {
		next, err := strconv.Atoi(strings.TrimSpace(strings.Join(raw, "")))
		if err != nil || next <= page {
			return 0
		}
		return next
	}
	if got < pageSize {
		return 0
	}
	return page + 1
}

// since, when set, is an RFC 3339 instant; GitLab reads updated_after inclusively,
// so the watermark item comes back and the caller refilters it out.
func (c *client) mergeRequests(ctx context.Context, p project, since string) ([]mergeRequest, error) {
	query := url.Values{"order_by": {"updated_at"}, "sort": {"asc"}}
	if since != "" {
		query.Set("updated_after", since)
	}
	mrs, err := paged[mergeRequest](ctx, c, "/projects/"+p.encoded+"/merge_requests", query)
	if err != nil {
		return nil, fmt.Errorf("merge requests: %w", err)
	}
	return mrs, nil
}

func (c *client) mergeRequestDiscussions(ctx context.Context, p project, iid int) ([]discussion, error) {
	path := "/projects/" + p.encoded + "/merge_requests/" + strconv.Itoa(iid) + "/discussions"
	discussions, err := paged[discussion](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("discussions of !%d: %w", iid, err)
	}
	return discussions, nil
}

func (c *client) mergeRequestCommits(ctx context.Context, p project, iid int) ([]commit, error) {
	path := "/projects/" + p.encoded + "/merge_requests/" + strconv.Itoa(iid) + "/commits"
	commits, err := paged[commit](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("commits of !%d: %w", iid, err)
	}
	return commits, nil
}

// The commits endpoint has no sort parameter — history is newest-first — so since
// is what keeps an incremental round small.
func (c *client) commits(ctx context.Context, p project, since string) ([]commit, error) {
	query := url.Values{}
	if since != "" {
		query.Set("since", since)
	}
	commits, err := paged[commit](ctx, c, "/projects/"+p.encoded+"/repository/commits", query)
	if err != nil {
		return nil, fmt.Errorf("commits: %w", err)
	}
	return commits, nil
}

func (c *client) commitDiff(ctx context.Context, p project, sha string) ([]diff, error) {
	path := "/projects/" + p.encoded + "/repository/commits/" + url.PathEscape(sha) + "/diff"
	diffs, err := paged[diff](ctx, c, path, nil)
	if err != nil {
		return nil, fmt.Errorf("diff of %s: %w", sha, err)
	}
	return diffs, nil
}

func (c *client) issues(ctx context.Context, p project, since string) ([]issue, error) {
	query := url.Values{"order_by": {"updated_at"}, "sort": {"asc"}}
	if since != "" {
		query.Set("updated_after", since)
	}
	issues, err := paged[issue](ctx, c, "/projects/"+p.encoded+"/issues", query)
	if err != nil {
		return nil, fmt.Errorf("issues: %w", err)
	}
	return issues, nil
}

func (c *client) issueNotes(ctx context.Context, p project, iid int) ([]note, error) {
	path := "/projects/" + p.encoded + "/issues/" + strconv.Itoa(iid) + "/notes"
	notes, err := paged[note](ctx, c, path, url.Values{"order_by": {"created_at"}, "sort": {"asc"}})
	if err != nil {
		return nil, fmt.Errorf("notes of #%d: %w", iid, err)
	}
	return notes, nil
}
