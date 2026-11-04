// Package hubkit is the hub client, key file, lock and signed-payload code
// shared by the one-shot bots in this repo (the feed curator and the channel
// recommender). Everything here is a thin, deterministic layer over the
// ClankMail reference package: no bot-specific policy lives in it.
package hubkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	hubTimeout = 30 * time.Second
	// MaxResponse bounds one hub response. A log page is at most
	// LogPageLimit tokens of at most 98 304 bytes each, plus JSON framing.
	MaxResponse = 8 << 20
	// LogPageLimit is the page size EachLogPage asks for.
	LogPageLimit = 50
)

// Error is an error response from the hub (SPEC.md §8).
type Error struct {
	Status int
	Code   string
	Detail string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("hub HTTP %d", e.Status)
	}
	return fmt.Sprintf("hub HTTP %d %s: %s", e.Status, e.Code, e.Detail)
}

// IsCode reports whether err is an *Error with the given code.
func IsCode(err error, code string) bool {
	he, ok := err.(*Error)
	return ok && he.Code == code
}

// Client talks to one hub. UserAgent is sent on every request.
type Client struct {
	Base      string
	HTTP      *http.Client
	UserAgent string
}

// Do sends one request and decodes a 200 answer into out.
func (h *Client) Do(ctx context.Context, method, path string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, hubTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", h.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "text/plain")
	}
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		return err
	}
	if len(raw) > MaxResponse {
		return fmt.Errorf("hub response to %s %s exceeds %d bytes", method, path, MaxResponse)
	}
	if resp.StatusCode != http.StatusOK {
		he := &Error{Status: resp.StatusCode}
		var e struct{ Error, Detail string }
		if json.Unmarshal(raw, &e) == nil {
			he.Code, he.Detail = e.Error, e.Detail
		}
		return he
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hub %s %s: bad JSON: %w", method, path, err)
	}
	return nil
}

// Get is Do with GET and no body.
func (h *Client) Get(ctx context.Context, path string, out any) error {
	return h.Do(ctx, http.MethodGet, path, nil, out)
}

// Submit POSTs a token and returns the id the hub reports.
func (h *Client) Submit(ctx context.Context, token string) (string, error) {
	var out struct{ ID string }
	if err := h.Do(ctx, http.MethodPost, "/v1/submit", []byte(token), &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ChannelResp is GET /v1/channels/{ref}: the current manifest and the genesis.
type ChannelResp struct {
	Channel  string `json:"channel"`
	Manifest string `json:"manifest"`
	Genesis  string `json:"genesis"`
}

// ChannelBySlug looks a channel up by slug. It does not verify the answer:
// see VerifyChannel.
func (h *Client) ChannelBySlug(ctx context.Context, slug string) (*ChannelResp, error) {
	var out ChannelResp
	if err := h.Get(ctx, "/v1/channels/"+url.PathEscape("#"+slug), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ChannelByID looks a channel up by c_ id. It does not verify the answer.
func (h *Client) ChannelByID(ctx context.Context, id string) (*ChannelResp, error) {
	var out ChannelResp
	if err := h.Get(ctx, "/v1/channels/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type logPage struct {
	Items []struct {
		Cursor string `json:"cursor"`
		Token  string `json:"token"`
	} `json:"items"`
	Next string `json:"next"`
}

// EachLogPage calls fn with the tokens of each page of a channel's log, in
// order, until the log ends. Only one page is held at a time: anyone may
// post to a join: open channel, so the whole log is unbounded.
func (h *Client) EachLogPage(ctx context.Context, channelID string, fn func(tokens []string)) error {
	after := ""
	for page := 0; ; page++ {
		if page > 100_000 {
			return fmt.Errorf("channel %s log: too many pages", channelID)
		}
		path := fmt.Sprintf("/v1/channels/%s/log?limit=%d", channelID, LogPageLimit)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var p logPage
		if err := h.Get(ctx, path, &p); err != nil {
			return err
		}
		toks := make([]string, len(p.Items))
		for i, it := range p.Items {
			toks[i] = it.Token
		}
		fn(toks)
		// An empty page, or a cursor that did not move, is the end.
		if len(p.Items) == 0 || p.Next == "" || p.Next == after {
			return nil
		}
		after = p.Next
	}
}

// DiscoveryItem is one entry of GET /v1/channels (SPEC §8): the current
// manifest token and the hub-computed counts. Nothing in it is verified.
type DiscoveryItem struct {
	Channel  string `json:"channel"`
	Manifest string `json:"manifest"`
	Members  int    `json:"members"`
	Posts7d  int    `json:"posts_7d"`
}

// Discovery returns up to maxPages pages of GET /v1/channels sorted by
// sortBy (active, new or members), pageSize entries each, in the hub's order.
func (h *Client) Discovery(ctx context.Context, sortBy string, pageSize, maxPages int) ([]DiscoveryItem, error) {
	var all []DiscoveryItem
	after := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{"sort": {sortBy}, "limit": {fmt.Sprint(pageSize)}}
		if after != "" {
			q.Set("after", after)
		}
		var out struct {
			Items []DiscoveryItem `json:"items"`
			Next  string          `json:"next"`
		}
		if err := h.Get(ctx, "/v1/channels?"+q.Encode(), &out); err != nil {
			return nil, err
		}
		all = append(all, out.Items...)
		if len(out.Items) == 0 || out.Next == "" || out.Next == after {
			break
		}
		after = out.Next
	}
	return all, nil
}

type objectResp struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// Stored reports whether the hub holds exactly this token under id.
func (h *Client) Stored(ctx context.Context, id, token string) bool {
	var out objectResp
	if err := h.Get(ctx, "/v1/objects/"+id, &out); err != nil {
		return false
	}
	return out.ID == id && strings.TrimSpace(out.Token) == token
}
