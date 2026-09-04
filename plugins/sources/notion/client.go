package notion

import (
	"bytes"
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
	defaultBaseURL = "https://api.notion.com"

	// Notion rejects any request that omits this header.
	apiVersion = "2026-03-11"

	requestTimeout = 60 * time.Second

	// defaultMaxAttempts counts the first try, so four retries follow it.
	defaultMaxAttempts = 5

	defaultBaseBackoff = time.Second
	maxBackoff         = 30 * time.Second

	// maxRetryWait caps a server-requested delay; a longer one ends the sync round.
	maxRetryWait = 2 * time.Minute

	maxResponseBytes = 32 << 20
	maxErrorBody     = 512

	// pageSize is the Notion maximum for search and for block children.
	pageSize = 100

	untitled = "Untitled"
)

type client struct {
	http        *http.Client
	baseURL     string
	token       string
	maxAttempts int
	baseBackoff time.Duration
	sleep       func(context.Context, time.Duration) error
}

func newClient(token, baseURL string) *client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &client{
		http:        &http.Client{},
		baseURL:     strings.TrimSuffix(baseURL, "/"),
		token:       token,
		maxAttempts: defaultMaxAttempts,
		baseBackoff: defaultBaseBackoff,
		sleep:       sleepCtx,
	}
}

type richText struct {
	PlainText string `json:"plain_text"`
	Href      string `json:"href"`
}

type property struct {
	Type  string     `json:"type"`
	Title []richText `json:"title"`
}

const (
	parentPage  = "page_id"
	parentBlock = "block_id"
)

type parentRef struct {
	Type    string `json:"type"`
	PageID  string `json:"page_id"`
	BlockID string `json:"block_id"`
}

// A workspace, database or data source parent ends the chain: none of them can sit under a page.
func (r parentRef) id() string {
	switch r.Type {
	case parentPage:
		return r.PageID
	case parentBlock:
		return r.BlockID
	}
	return ""
}

type page struct {
	ID             string              `json:"id"`
	CreatedTime    time.Time           `json:"created_time"`
	LastEditedTime time.Time           `json:"last_edited_time"`
	URL            string              `json:"url"`
	InTrash        bool                `json:"in_trash"`
	Archived       bool                `json:"archived"`
	Parent         parentRef           `json:"parent"`
	Properties     map[string]property `json:"properties"`
}

// archived is the deprecated alias of in_trash; either one means the page is gone.
func (p *page) trashed() bool { return p.InTrash || p.Archived }

// The title property's key is workspace-defined, so it is found by type instead.
func (p *page) title() string {
	for _, prop := range p.Properties {
		if prop.Type != "title" {
			continue
		}
		if text := plainText(prop.Title); text != "" {
			return text
		}
	}
	return untitled
}

type blockPayload struct {
	RichText []richText   `json:"rich_text"`
	Checked  bool         `json:"checked"`
	Language string       `json:"language"`
	Title    string       `json:"title"`
	Cells    [][]richText `json:"cells"`
}

type block struct {
	ID          string
	Type        string
	HasChildren bool
	Parent      parentRef
	Payload     blockPayload
	Children    []block
}

func (b *block) UnmarshalJSON(data []byte) error {
	var header struct {
		ID          string    `json:"id"`
		Type        string    `json:"type"`
		HasChildren bool      `json:"has_children"`
		Parent      parentRef `json:"parent"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	var byKey map[string]json.RawMessage
	if err := json.Unmarshal(data, &byKey); err != nil {
		return err
	}
	b.ID, b.Type, b.HasChildren, b.Parent = header.ID, header.Type, header.HasChildren, header.Parent
	// Payload shapes vary per block type; an unrecognised one renders as nothing rather than failing the page.
	_ = json.Unmarshal(byKey[header.Type], &b.Payload)
	return nil
}

type listPage struct {
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

func (l listPage) next() (string, bool) { return l.NextCursor, l.HasMore && l.NextCursor != "" }

type pageList struct {
	listPage
	Results []page `json:"results"`
}

type blockList struct {
	listPage
	Results []block `json:"results"`
}

type searchRequest struct {
	Query       string       `json:"query,omitempty"`
	Filter      searchFilter `json:"filter"`
	Sort        searchSort   `json:"sort"`
	StartCursor string       `json:"start_cursor,omitempty"`
	PageSize    int          `json:"page_size"`
}

type searchFilter struct {
	Property string `json:"property"`
	Value    string `json:"value"`
}

type searchSort struct {
	Timestamp string `json:"timestamp"`
	Direction string `json:"direction"`
}

// eachPage walks search results oldest-edited-first; query empty searches every visible page.
func (c *client) eachPage(ctx context.Context, query string, visit func(*page) bool) error {
	cursor := ""
	for {
		body, err := json.Marshal(searchRequest{
			Query:       query,
			Filter:      searchFilter{Property: "object", Value: "page"},
			Sort:        searchSort{Timestamp: "last_edited_time", Direction: "ascending"},
			StartCursor: cursor,
			PageSize:    pageSize,
		})
		if err != nil {
			return fmt.Errorf("encode search request: %w", err)
		}
		var list pageList
		if err := c.request(ctx, http.MethodPost, "/v1/search", body, &list); err != nil {
			return err
		}
		for i := range list.Results {
			if !visit(&list.Results[i]) {
				return nil
			}
		}
		next, ok := list.next()
		if !ok {
			return nil
		}
		cursor = next
	}
}

func (c *client) blockTree(ctx context.Context, id string, depth int) ([]block, error) {
	blocks, err := c.blockChildren(ctx, id)
	if err != nil {
		return nil, err
	}
	if depth >= maxBlockDepth {
		return blocks, nil
	}
	for i := range blocks {
		b := &blocks[i]
		if !b.HasChildren || b.Type == blockChildPage {
			continue
		}
		if b.Children, err = c.blockTree(ctx, b.ID, depth+1); err != nil {
			return nil, err
		}
	}
	return blocks, nil
}

func (c *client) blockChildren(ctx context.Context, id string) ([]block, error) {
	var blocks []block
	cursor := ""
	for {
		path := "/v1/blocks/" + id + "/children?page_size=" + strconv.Itoa(pageSize)
		if cursor != "" {
			path += "&start_cursor=" + url.QueryEscape(cursor)
		}
		var list blockList
		if err := c.request(ctx, http.MethodGet, path, nil, &list); err != nil {
			return nil, err
		}
		blocks = append(blocks, list.Results...)
		next, ok := list.next()
		if !ok {
			return blocks, nil
		}
		cursor = next
	}
}

func (c *client) pageByID(ctx context.Context, id string) (*page, error) {
	var p page
	if err := c.request(ctx, http.MethodGet, "/v1/pages/"+id, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *client) pageParent(ctx context.Context, id string) (parentRef, error) {
	p, err := c.pageByID(ctx, id)
	if err != nil {
		return parentRef{}, err
	}
	return p.Parent, nil
}

func (c *client) blockParent(ctx context.Context, id string) (parentRef, error) {
	var b block
	if err := c.request(ctx, http.MethodGet, "/v1/blocks/"+id, nil, &b); err != nil {
		return parentRef{}, err
	}
	return b.Parent, nil
}

type retryableError struct {
	err   error
	after time.Duration // server-requested delay; zero means exponential backoff
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func (c *client) request(ctx context.Context, method, path string, body []byte, out any) error {
	payload, err := c.do(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

func (c *client) do(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		payload, err := c.attempt(ctx, method, url, body)
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
	attemptCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(attemptCtx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, url, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Notion-Version", apiVersion)
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
	if resp.StatusCode/100 == 2 {
		return payload, nil
	}

	statusErr := fmt.Errorf("%s %s: status %d: %s", method, url, resp.StatusCode, apiMessage(payload))
	if retryableStatus(resp.StatusCode) {
		return nil, &retryableError{err: statusErr, after: retryDelay(resp.Header)}
	}
	return nil, statusErr
}

// 529 is Notion's own service_overload; every other overload answer is a plain 5xx.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

// Notion states Retry-After in whole seconds and never as an HTTP-date.
func retryDelay(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After")))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
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

// A failure carries {"code":…,"message":…}; anything else is truncated so an error
// page cannot flood an error string.
func apiMessage(payload []byte) string {
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &body); err == nil && body.Message != "" {
		if body.Code != "" {
			return truncate(body.Code + ": " + body.Message)
		}
		return truncate(body.Message)
	}
	return truncate(strings.TrimSpace(string(payload)))
}

func truncate(s string) string {
	if len(s) <= maxErrorBody {
		return s
	}
	return s[:maxErrorBody] + "…"
}
