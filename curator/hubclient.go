package curator

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
	// maxHubResponse bounds one hub response. A log page is at most
	// logPageLimit tokens of at most 98 304 bytes each, plus JSON framing.
	maxHubResponse = 8 << 20
	logPageLimit   = 50
)

// HubError is an error response from the hub (SPEC.md §8).
type HubError struct {
	Status int
	Code   string
	Detail string
}

func (e *HubError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("hub HTTP %d", e.Status)
	}
	return fmt.Sprintf("hub HTTP %d %s: %s", e.Status, e.Code, e.Detail)
}

// isCode reports whether err is a HubError with the given code.
func isCode(err error, code string) bool {
	he, ok := err.(*HubError)
	return ok && he.Code == code
}

type hubClient struct {
	base   string
	client *http.Client
}

func (h *hubClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, hubTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "text/plain")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxHubResponse+1))
	if err != nil {
		return err
	}
	if len(raw) > maxHubResponse {
		return fmt.Errorf("hub response to %s %s exceeds %d bytes", method, path, maxHubResponse)
	}
	if resp.StatusCode != http.StatusOK {
		he := &HubError{Status: resp.StatusCode}
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

func (h *hubClient) get(ctx context.Context, path string, out any) error {
	return h.do(ctx, http.MethodGet, path, nil, out)
}

// submit POSTs a token and returns the id the hub reports.
func (h *hubClient) submit(ctx context.Context, token string) (string, error) {
	var out struct{ ID string }
	if err := h.do(ctx, http.MethodPost, "/v1/submit", []byte(token), &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

type channelResp struct {
	Channel  string `json:"channel"`
	Manifest string `json:"manifest"`
	Genesis  string `json:"genesis"`
}

func (h *hubClient) channelBySlug(ctx context.Context, slug string) (*channelResp, error) {
	var out channelResp
	if err := h.get(ctx, "/v1/channels/"+url.PathEscape("#"+slug), &out); err != nil {
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

// logTokens returns every token in a channel's log, reading all pages.
func (h *hubClient) logTokens(ctx context.Context, channelID string) ([]string, error) {
	var out []string
	after := ""
	for page := 0; ; page++ {
		if page > 100_000 {
			return nil, fmt.Errorf("channel %s log: too many pages", channelID)
		}
		path := fmt.Sprintf("/v1/channels/%s/log?limit=%d", channelID, logPageLimit)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var p logPage
		if err := h.get(ctx, path, &p); err != nil {
			return nil, err
		}
		for _, it := range p.Items {
			out = append(out, it.Token)
		}
		// An empty page, or a cursor that did not move, is the end.
		if len(p.Items) == 0 || p.Next == "" || p.Next == after {
			return out, nil
		}
		after = p.Next
	}
}

type objectResp struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// stored reports whether the hub holds exactly this token under id.
func (h *hubClient) stored(ctx context.Context, id, token string) bool {
	var out objectResp
	if err := h.get(ctx, "/v1/objects/"+id, &out); err != nil {
		return false
	}
	return out.ID == id && strings.TrimSpace(out.Token) == token
}
