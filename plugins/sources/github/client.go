package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"

	// requestTimeout bounds one attempt; retries get a fresh timeout each.
	requestTimeout = 60 * time.Second

	// defaultMaxAttempts counts the first try, so four retries follow it.
	defaultMaxAttempts = 5

	// defaultBaseBackoff doubles per attempt up to maxBackoff, absent a server delay.
	defaultBaseBackoff = time.Second
	maxBackoff         = 30 * time.Second

	// maxRetryWait caps a server-requested delay; a longer one ends the sync round.
	maxRetryWait = 2 * time.Minute

	// maxResponseBytes guards against an unbounded read of a malformed body.
	maxResponseBytes = 32 << 20

	// maxErrorBody bounds how much of a failing body reaches an error message.
	maxErrorBody = 512

	// GitHub rejects a query whose potential node count exceeds 500,000, and PRs nest
	// two levels, so prPageSize * nestedPageSize^2 is the number that has to stay small.
	pageSize       = 50
	prPageSize     = 20
	nestedPageSize = 20
)

// client is the GitHub transport. The token only ever reaches the Authorization
// header: nothing here logs, and no error message carries a header.
type client struct {
	http        *http.Client
	baseURL     string
	token       string
	maxAttempts int
	baseBackoff time.Duration
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
}

func newClient(token, baseURL string) *client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &client{
		http:        &http.Client{Timeout: requestTimeout},
		baseURL:     strings.TrimSuffix(baseURL, "/"),
		token:       token,
		maxAttempts: defaultMaxAttempts,
		baseBackoff: defaultBaseBackoff,
		now:         time.Now,
		sleep:       sleepCtx,
	}
}

// GitHub Enterprise Server splits the APIs (REST /api/v3, GraphQL /api/graphql);
// github.com serves both under api.github.com.
func (c *client) graphqlURL() string {
	if root, ok := strings.CutSuffix(c.baseURL, "/api/v3"); ok {
		return root + "/api/graphql"
	}
	return c.baseURL + "/graphql"
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors graphQLErrors   `json:"errors"`
}

type graphQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type graphQLErrors []graphQLError

// A RATE_LIMITED entry is the primary GraphQL limit: HTTP 200 with no Retry-After,
// so it is marked retryable and falls back to exponential backoff.
func (e graphQLErrors) err() error {
	if len(e) == 0 {
		return nil
	}
	messages := make([]string, 0, len(e))
	rateLimited := false
	for _, ge := range e {
		if ge.Type == "RATE_LIMITED" {
			rateLimited = true
		}
		msg := ge.Message
		if ge.Type != "" {
			msg = ge.Type + ": " + msg
		}
		messages = append(messages, msg)
	}
	err := fmt.Errorf("graphql errors: %s", strings.Join(messages, "; "))
	if rateLimited {
		return &retryableError{err: err}
	}
	return err
}

type retryableError struct {
	err   error
	after time.Duration // server-requested delay; zero means exponential backoff
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func (c *client) execute(ctx context.Context, query string, variables map[string]any, out any) error {
	body, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return fmt.Errorf("encode graphql request: %w", err)
	}

	var resp graphQLResponse
	_, err = c.do(ctx, http.MethodPost, c.graphqlURL(), body, func(payload []byte) error {
		resp = graphQLResponse{}
		if err := json.Unmarshal(payload, &resp); err != nil {
			return fmt.Errorf("decode graphql response: %w", err)
		}
		return resp.Errors.err()
	})
	if err != nil {
		return err
	}
	if len(resp.Data) == 0 {
		return errors.New("graphql response carried no data")
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return fmt.Errorf("decode graphql data: %w", err)
	}
	return nil
}

// check validates a 200 payload and may itself demand a retry.
func (c *client) do(ctx context.Context, method, url string, body []byte, check func([]byte) error) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		payload, err := c.attempt(ctx, method, url, body)
		if err == nil && check != nil {
			err = check(payload)
		}
		if err == nil {
			return payload, nil
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

func (c *client) attempt(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s %s: %w", method, url, err)
		}
		return nil, &retryableError{err: fmt.Errorf("%s %s: %w", method, url, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, &retryableError{err: fmt.Errorf("read %s %s response: %w", method, url, readErr)}
	}
	if resp.StatusCode == http.StatusOK {
		return payload, nil
	}

	statusErr := fmt.Errorf("%s %s: status %d: %s", method, url, resp.StatusCode, apiMessage(payload))
	if retryableStatus(resp.StatusCode, resp.Header, payload) {
		return nil, &retryableError{err: statusErr, after: retryDelay(resp.Header, c.now())}
	}
	return nil, statusErr
}

// A bare 403 is a permissions or SSO problem, so it is retryable only with
// rate-limit evidence. GraphQL answers an internal timeout with 502.
func retryableStatus(status int, header http.Header, payload []byte) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		if header.Get("Retry-After") != "" || header.Get("X-RateLimit-Remaining") == "0" {
			return true
		}
		msg := strings.ToLower(apiMessage(payload))
		return strings.Contains(msg, "secondary rate limit") || strings.Contains(msg, "abuse detection")
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// Retry-After, else the primary-limit reset epoch; zero means back off exponentially.
func retryDelay(header http.Header, now time.Time) time.Duration {
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil {
			return time.Duration(seconds) * time.Second
		}
		if at, err := http.ParseTime(raw); err == nil {
			return at.Sub(now)
		}
	}
	if header.Get("X-RateLimit-Remaining") == "0" {
		if epoch, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(epoch, 0).Sub(now)
		}
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

// REST uses {"message": …}, GraphQL {"errors": [{"message": …}]}. Anything else is
// truncated so an HTML error page cannot flood an error string.
func apiMessage(payload []byte) string {
	var body struct {
		Message string        `json:"message"`
		Errors  graphQLErrors `json:"errors"`
	}
	if err := json.Unmarshal(payload, &body); err == nil {
		if body.Message != "" {
			return truncate(body.Message)
		}
		if len(body.Errors) > 0 {
			return truncate(body.Errors[0].Message)
		}
	}
	return truncate(strings.TrimSpace(string(payload)))
}

func truncate(s string) string {
	if len(s) <= maxErrorBody {
		return s
	}
	return s[:maxErrorBody] + "…"
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// An empty endCursor ends pagination even when hasNextPage is set.
func (p pageInfo) next() (string, bool) {
	if !p.HasNextPage || p.EndCursor == "" {
		return "", false
	}
	return p.EndCursor, true
}

type actor struct {
	Login string `json:"login"`
}

func (a *actor) login() string {
	if a == nil {
		return ""
	}
	return a.Login
}

type numberNode struct {
	Number int `json:"number"`
}

type commitNode struct {
	OID             string    `json:"oid"`
	URL             string    `json:"url"`
	MessageHeadline string    `json:"messageHeadline"`
	Message         string    `json:"message"`
	CommittedDate   time.Time `json:"committedDate"`
	AuthoredDate    time.Time `json:"authoredDate"`
	ChangedFiles    *int      `json:"changedFilesIfAvailable"`
	Author          *struct {
		Name string `json:"name"`
		User *actor `json:"user"`
	} `json:"author"`
	AssociatedPullRequests struct {
		Nodes []numberNode `json:"nodes"`
	} `json:"associatedPullRequests"`
}

// Falls back to the raw git author name, which is all an unlinked commit carries.
func (c *commitNode) author() string {
	if c.Author == nil {
		return ""
	}
	if login := c.Author.User.login(); login != "" {
		return login
	}
	return c.Author.Name
}

// A null changedFilesIfAvailable means the count is unknown, so the list is fetched.
func (c *commitNode) touchesFiles() bool {
	return c.ChangedFiles == nil || *c.ChangedFiles > 0
}

type commitHistory struct {
	PageInfo pageInfo     `json:"pageInfo"`
	Nodes    []commitNode `json:"nodes"`
}

type commitsResponse struct {
	Repository struct {
		DefaultBranchRef *struct {
			Target struct {
				History commitHistory `json:"history"`
			} `json:"target"`
		} `json:"defaultBranchRef"`
	} `json:"repository"`
}

type prCommitConnection struct {
	PageInfo pageInfo `json:"pageInfo"`
	Nodes    []struct {
		Commit struct {
			OID string `json:"oid"`
		} `json:"commit"`
	} `json:"nodes"`
}

type reviewCommentNode struct {
	DatabaseID int64     `json:"databaseId"`
	Body       string    `json:"body"`
	Path       string    `json:"path"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Author     *actor    `json:"author"`
}

type reviewCommentConnection struct {
	PageInfo pageInfo            `json:"pageInfo"`
	Nodes    []reviewCommentNode `json:"nodes"`
}

type reviewNode struct {
	ID         string                  `json:"id"`
	DatabaseID int64                   `json:"databaseId"`
	State      string                  `json:"state"`
	Body       string                  `json:"body"`
	URL        string                  `json:"url"`
	CreatedAt  time.Time               `json:"createdAt"`
	UpdatedAt  time.Time               `json:"updatedAt"`
	Author     *actor                  `json:"author"`
	Comments   reviewCommentConnection `json:"comments"`
}

type reviewConnection struct {
	PageInfo pageInfo     `json:"pageInfo"`
	Nodes    []reviewNode `json:"nodes"`
}

type prNode struct {
	Number                  int       `json:"number"`
	Title                   string    `json:"title"`
	Body                    string    `json:"body"`
	URL                     string    `json:"url"`
	CreatedAt               time.Time `json:"createdAt"`
	UpdatedAt               time.Time `json:"updatedAt"`
	HeadRefName             string    `json:"headRefName"`
	Author                  *actor    `json:"author"`
	ClosingIssuesReferences struct {
		Nodes []numberNode `json:"nodes"`
	} `json:"closingIssuesReferences"`
	Commits prCommitConnection `json:"commits"`
	Reviews reviewConnection   `json:"reviews"`
}

type prConnection struct {
	PageInfo pageInfo `json:"pageInfo"`
	Nodes    []prNode `json:"nodes"`
}

type pullRequestsResponse struct {
	Repository struct {
		PullRequests prConnection `json:"pullRequests"`
	} `json:"repository"`
}

type reviewsResponse struct {
	Repository struct {
		PullRequest struct {
			Reviews reviewConnection `json:"reviews"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

type reviewCommentsResponse struct {
	Node struct {
		Comments reviewCommentConnection `json:"comments"`
	} `json:"node"`
}

type prCommitsResponse struct {
	Repository struct {
		PullRequest struct {
			Commits prCommitConnection `json:"commits"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

type issueCommentNode struct {
	DatabaseID int64     `json:"databaseId"`
	Body       string    `json:"body"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Author     *actor    `json:"author"`
}

type issueCommentConnection struct {
	PageInfo pageInfo           `json:"pageInfo"`
	Nodes    []issueCommentNode `json:"nodes"`
}

type issueNode struct {
	Number    int                    `json:"number"`
	Title     string                 `json:"title"`
	Body      string                 `json:"body"`
	URL       string                 `json:"url"`
	CreatedAt time.Time              `json:"createdAt"`
	UpdatedAt time.Time              `json:"updatedAt"`
	Author    *actor                 `json:"author"`
	Comments  issueCommentConnection `json:"comments"`
}

type issueConnection struct {
	PageInfo pageInfo    `json:"pageInfo"`
	Nodes    []issueNode `json:"nodes"`
}

type issuesResponse struct {
	Repository struct {
		Issues issueConnection `json:"issues"`
	} `json:"repository"`
}

type issueCommentsResponse struct {
	Repository struct {
		Issue struct {
			Comments issueCommentConnection `json:"comments"`
		} `json:"issue"`
	} `json:"repository"`
}

// Top-level connections come back newest-first, and GitHub offers no ascending order
// for commit history, so an incremental sync can stop at the watermark.

const commitsQuery = `query LoreCommits($owner: String!, $name: String!, $first: Int!, $after: String, $since: GitTimestamp) {
  repository(owner: $owner, name: $name) {
    defaultBranchRef {
      target {
        ... on Commit {
          history(first: $first, after: $after, since: $since) {
            pageInfo { hasNextPage endCursor }
            nodes {
              oid
              url
              messageHeadline
              message
              committedDate
              authoredDate
              changedFilesIfAvailable
              author { name user { login } }
              associatedPullRequests(first: 10) { nodes { number } }
            }
          }
        }
      }
    }
  }
}`

const pullRequestsQuery = `query LorePullRequests($owner: String!, $name: String!, $first: Int!, $after: String, $nested: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: $first, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        title
        body
        url
        createdAt
        updatedAt
        headRefName
        author { login }
        closingIssuesReferences(first: $nested) { nodes { number } }
        commits(first: $nested) {
          pageInfo { hasNextPage endCursor }
          nodes { commit { oid } }
        }
        reviews(first: $nested) {
          pageInfo { hasNextPage endCursor }
          nodes {
            id
            databaseId
            state
            body
            url
            createdAt
            updatedAt
            author { login }
            comments(first: $nested) {
              pageInfo { hasNextPage endCursor }
              nodes { databaseId body path url createdAt updatedAt author { login } }
            }
          }
        }
      }
    }
  }
}`

const reviewsQuery = `query LoreReviews($owner: String!, $name: String!, $number: Int!, $first: Int!, $after: String, $nested: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviews(first: $first, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          databaseId
          state
          body
          url
          createdAt
          updatedAt
          author { login }
          comments(first: $nested) {
            pageInfo { hasNextPage endCursor }
            nodes { databaseId body path url createdAt updatedAt author { login } }
          }
        }
      }
    }
  }
}`

const reviewCommentsQuery = `query LoreReviewComments($id: ID!, $first: Int!, $after: String) {
  node(id: $id) {
    ... on PullRequestReview {
      comments(first: $first, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { databaseId body path url createdAt updatedAt author { login } }
      }
    }
  }
}`

const prCommitsQuery = `query LorePRCommits($owner: String!, $name: String!, $number: Int!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      commits(first: $first, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { commit { oid } }
      }
    }
  }
}`

const issuesQuery = `query LoreIssues($owner: String!, $name: String!, $first: Int!, $after: String, $since: DateTime, $nested: Int!) {
  repository(owner: $owner, name: $name) {
    issues(first: $first, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}, filterBy: {since: $since}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        title
        body
        url
        createdAt
        updatedAt
        author { login }
        comments(first: $nested) {
          pageInfo { hasNextPage endCursor }
          nodes { databaseId body url createdAt updatedAt author { login } }
        }
      }
    }
  }
}`

const issueCommentsQuery = `query LoreIssueComments($owner: String!, $name: String!, $number: Int!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      comments(first: $first, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { databaseId body url createdAt updatedAt author { login } }
      }
    }
  }
}`

// eachCommit walks default-branch history newest-first. since filters on committed
// date server-side; nil means a full backfill.
func (c *client) eachCommit(ctx context.Context, r repo, since any, visit func(*commitNode) bool) error {
	var after any
	for {
		var resp commitsResponse
		vars := map[string]any{"owner": r.owner, "name": r.name, "first": pageSize, "after": after, "since": since}
		if err := c.execute(ctx, commitsQuery, vars, &resp); err != nil {
			return fmt.Errorf("fetch commits: %w", err)
		}
		ref := resp.Repository.DefaultBranchRef
		if ref == nil {
			return nil // empty repository: no default branch, no history
		}
		history := ref.Target.History
		for i := range history.Nodes {
			if !visit(&history.Nodes[i]) {
				return nil
			}
		}
		cursor, ok := history.PageInfo.next()
		if !ok {
			return nil
		}
		after = cursor
	}
}

// GitHub offers no since filter here, so visit stops the walk at the watermark.
func (c *client) eachPullRequest(ctx context.Context, r repo, visit func(*prNode) bool) error {
	var after any
	for {
		var resp pullRequestsResponse
		vars := map[string]any{"owner": r.owner, "name": r.name, "first": prPageSize, "after": after, "nested": nestedPageSize}
		if err := c.execute(ctx, pullRequestsQuery, vars, &resp); err != nil {
			return fmt.Errorf("fetch pull requests: %w", err)
		}
		page := resp.Repository.PullRequests
		for i := range page.Nodes {
			if !visit(&page.Nodes[i]) {
				return nil
			}
		}
		cursor, ok := page.PageInfo.next()
		if !ok {
			return nil
		}
		after = cursor
	}
}

// since is the server-side filterBy watermark; the issues connection excludes PRs.
func (c *client) eachIssue(ctx context.Context, r repo, since any, visit func(*issueNode) bool) error {
	var after any
	for {
		var resp issuesResponse
		vars := map[string]any{"owner": r.owner, "name": r.name, "first": pageSize, "after": after, "since": since, "nested": nestedPageSize}
		if err := c.execute(ctx, issuesQuery, vars, &resp); err != nil {
			return fmt.Errorf("fetch issues: %w", err)
		}
		page := resp.Repository.Issues
		for i := range page.Nodes {
			if !visit(&page.Nodes[i]) {
				return nil
			}
		}
		cursor, ok := page.PageInfo.next()
		if !ok {
			return nil
		}
		after = cursor
	}
}

func (c *client) reviews(ctx context.Context, r repo, number int, first reviewConnection) ([]reviewNode, error) {
	nodes := first.Nodes
	pi := first.PageInfo
	for {
		cursor, ok := pi.next()
		if !ok {
			return nodes, nil
		}
		var resp reviewsResponse
		vars := map[string]any{
			"owner": r.owner, "name": r.name, "number": number,
			"first": nestedPageSize, "after": cursor, "nested": nestedPageSize,
		}
		if err := c.execute(ctx, reviewsQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("fetch reviews of #%d: %w", number, err)
		}
		page := resp.Repository.PullRequest.Reviews
		nodes = append(nodes, page.Nodes...)
		pi = page.PageInfo
	}
}

func (c *client) reviewComments(ctx context.Context, reviewID string, first reviewCommentConnection) ([]reviewCommentNode, error) {
	nodes := first.Nodes
	pi := first.PageInfo
	for {
		cursor, ok := pi.next()
		if !ok {
			return nodes, nil
		}
		var resp reviewCommentsResponse
		vars := map[string]any{"id": reviewID, "first": nestedPageSize, "after": cursor}
		if err := c.execute(ctx, reviewCommentsQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("fetch review comments: %w", err)
		}
		nodes = append(nodes, resp.Node.Comments.Nodes...)
		pi = resp.Node.Comments.PageInfo
	}
}

func (c *client) commitOIDs(ctx context.Context, r repo, number int, first prCommitConnection) ([]string, error) {
	oids := make([]string, 0, len(first.Nodes))
	pi := first.PageInfo
	for _, n := range first.Nodes {
		oids = append(oids, n.Commit.OID)
	}
	for {
		cursor, ok := pi.next()
		if !ok {
			return oids, nil
		}
		var resp prCommitsResponse
		vars := map[string]any{"owner": r.owner, "name": r.name, "number": number, "first": nestedPageSize, "after": cursor}
		if err := c.execute(ctx, prCommitsQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("fetch commits of #%d: %w", number, err)
		}
		page := resp.Repository.PullRequest.Commits
		for _, n := range page.Nodes {
			oids = append(oids, n.Commit.OID)
		}
		pi = page.PageInfo
	}
}

func (c *client) issueComments(ctx context.Context, r repo, number int, first issueCommentConnection) ([]issueCommentNode, error) {
	nodes := first.Nodes
	pi := first.PageInfo
	for {
		cursor, ok := pi.next()
		if !ok {
			return nodes, nil
		}
		var resp issueCommentsResponse
		vars := map[string]any{"owner": r.owner, "name": r.name, "number": number, "first": nestedPageSize, "after": cursor}
		if err := c.execute(ctx, issueCommentsQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("fetch comments of #%d: %w", number, err)
		}
		page := resp.Repository.Issue.Comments
		nodes = append(nodes, page.Nodes...)
		pi = page.PageInfo
	}
}

// GraphQL exposes only a changed-file count, so this is the connector's one REST
// call. Renames contribute both the new and the previous path.
func (c *client) commitFiles(ctx context.Context, r repo, oid string) ([]string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s", c.baseURL, r.owner, r.name, oid)
	payload, err := c.do(ctx, http.MethodGet, url, nil, nil)
	if err != nil {
		return nil, err
	}
	var body struct {
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("decode commit files: %w", err)
	}
	paths := make([]string, 0, len(body.Files))
	for _, f := range body.Files {
		if f.Filename != "" {
			paths = append(paths, f.Filename)
		}
		if f.PreviousFilename != "" {
			paths = append(paths, f.PreviousFilename)
		}
	}
	return paths, nil
}
