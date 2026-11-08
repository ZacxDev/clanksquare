package curator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// discord: one Discord channel read by a bot with the REST API
// (GET /channels/{id}/messages?after=…&limit=100). The intended channel is
// one in a server the operator owns, into which other servers' announcement
// channels are Followed: each followed post arrives as a message from a
// webhook named after its source ("Server #channel"), which becomes the item's
// "via" label. Message content is untrusted data, reduced to plain text and
// bounded like any feed text.

const (
	discordDefaultBase  = "https://discord.com"
	discordPageLimit    = 100
	maxDiscordPages     = 5
	maxDiscordRetries   = 3
	maxDiscordWait      = 60 * time.Second
	maxDiscordBodyBytes = 4 << 20
	discordTimeout      = 20 * time.Second
	// Discord requires this User-Agent form for bots.
	discordUserAgent = "DiscordBot (https://github.com/ZacxDev/clanksquare, " + Version + ")"
)

type discordMessage struct {
	ID        string `json:"id"`
	Type      int    `json:"type"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Author    struct {
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
	} `json:"author"`
	Embeds []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		URL         string `json:"url"`
	} `json:"embeds"`
}

func (r *runner) fetchDiscord(ctx context.Context, ch *Channel, fc FeedConfig) (*Feed, error) {
	token, err := r.secret("Discord bot token", r.DiscordTokenFile)
	if err != nil {
		return nil, err
	}
	// The cursor: only messages after the newest one already read. With none
	// (first run, lost state) start at the channel's max age: anything older
	// would be skipped as too old anyway.
	after := r.state.DiscordCursors[fc.ChannelID]
	if after == "" {
		after = discordSnowflakeAt(r.Now().Add(-time.Duration(ch.MaxAgeHours) * time.Hour))
	}
	var msgs []discordMessage
	newest := after
	for page := 0; page < maxDiscordPages; page++ {
		batch, err := r.discordGet(ctx, token, fc.ChannelID, newest)
		if err != nil {
			if len(msgs) == 0 {
				return nil, err
			}
			r.logf("WARN", "channel=%s discord %s: page %d failed (%v): keeping the %d messages read", ch.Slug, fc.ChannelID, page+1, err, len(msgs))
			break
		}
		for _, m := range batch {
			if !snowflakeRe.MatchString(m.ID) || !snowflakeLess(after, m.ID) {
				continue // malformed, or not after the cursor
			}
			msgs = append(msgs, m)
			if snowflakeLess(newest, m.ID) {
				newest = m.ID
			}
		}
		if len(batch) < discordPageLimit {
			break
		}
	}
	// The cursor moves only forward, and only to ids actually read. It is
	// persisted with the rest of the state at the next save.
	if newest != after && snowflakeLess(r.state.DiscordCursors[fc.ChannelID], newest) {
		r.state.DiscordCursors[fc.ChannelID] = newest
	}
	sort.Slice(msgs, func(i, j int) bool { return snowflakeLess(msgs[i].ID, msgs[j].ID) })
	f := &Feed{Title: "Discord"}
	for _, m := range msgs {
		if it, ok := discordItem(m, fc); ok {
			f.Items = append(f.Items, it)
		}
	}
	return f, nil
}

// discordGet fetches one page, honouring the rate limit headers: a 429 waits
// its retry_after and retries (at most maxDiscordRetries times, each wait at
// most maxDiscordWait), and an exhausted bucket (X-RateLimit-Remaining: 0)
// waits X-RateLimit-Reset-After before returning, so the next page cannot
// trip it.
func (r *runner) discordGet(ctx context.Context, token, channelID, after string) ([]discordMessage, error) {
	base := r.discordBase
	if base == "" {
		base = discordDefaultBase
	}
	u := base + "/api/v10/channels/" + url.PathEscape(channelID) + "/messages?after=" + url.QueryEscape(after) + "&limit=" + strconv.Itoa(discordPageLimit)
	for attempt := 0; ; attempt++ {
		status, hdr, body, err := r.discordDo(ctx, token, u)
		if err != nil {
			return nil, err
		}
		if status == http.StatusTooManyRequests {
			wait := retryAfter(hdr, body)
			if attempt >= maxDiscordRetries || wait > maxDiscordWait {
				return nil, fmt.Errorf("discord rate limited (HTTP 429, retry after %s)", wait)
			}
			if err := r.sleep(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("discord HTTP %d: %s", status, Truncate(PlainText(string(body)), 200))
		}
		var msgs []discordMessage
		if err := json.Unmarshal(body, &msgs); err != nil {
			return nil, fmt.Errorf("discord response: %w", err)
		}
		if hdr.Get("X-RateLimit-Remaining") == "0" {
			if wait := secondsHeader(hdr.Get("X-RateLimit-Reset-After")); wait > 0 {
				if err := r.sleep(ctx, min(wait, maxDiscordWait)); err != nil {
					return msgs, nil
				}
			}
		}
		return msgs, nil
	}
}

func (r *runner) discordDo(ctx context.Context, token, u string) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, discordTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("User-Agent", discordUserAgent)
	resp, err := r.Client.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("discord request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscordBodyBytes+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(body) > maxDiscordBodyBytes {
		return 0, nil, nil, fmt.Errorf("discord response exceeds %d bytes", maxDiscordBodyBytes)
	}
	return resp.StatusCode, resp.Header, body, nil
}

// retryAfter is a 429's wait: the JSON body's retry_after (seconds, may be
// fractional), else the Retry-After header, else one second.
func retryAfter(h http.Header, body []byte) time.Duration {
	var b struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &b) == nil && b.RetryAfter > 0 {
		return time.Duration(b.RetryAfter * float64(time.Second))
	}
	if d := secondsHeader(h.Get("Retry-After")); d > 0 {
		return d
	}
	return time.Second
}

func secondsHeader(v string) time.Duration {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f <= 0 || f > 86400 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

var urlRe = regexp.MustCompile(`https?://[^\s<>()\[\]"'` + "`" + `]+`)

// firstURL is the first http(s) URL in s, without trailing punctuation.
func firstURL(s string) string {
	for _, m := range urlRe.FindAllString(s, 8) {
		m = strings.TrimRight(m, ".,;:!?*_~|>")
		if NormalizeLink(m) != "" {
			return m
		}
	}
	return ""
}

// discordItem maps a message to an item: the title is the first non-empty
// line of the content (else the first embed's title), the excerpt the rest of
// the content and the embeds' text, the link the first URL in the message
// (else the first embed's URL, else the message's permalink), and the source
// label the author's name (a follower webhook is named after its source).
func discordItem(m discordMessage, fc FeedConfig) (Item, bool) {
	if m.Type != 0 && m.Type != 19 { // default and reply; not joins, pins, follow notices
		return Item{}, false
	}
	content := clip(m.Content, maxRawField)
	var title string
	var rest []string
	for _, line := range strings.Split(content, "\n") {
		if t := PlainText(line); t != "" {
			if title == "" {
				title = t
			} else {
				rest = append(rest, t)
			}
		}
	}
	link := firstURL(content)
	for _, e := range m.Embeds {
		if title == "" {
			title = PlainText(e.Title)
		} else if t := PlainText(e.Title); t != "" {
			rest = append(rest, t)
		}
		if d := PlainText(clip(e.Description, maxRawField)); d != "" {
			rest = append(rest, d)
		}
		if link == "" {
			if l := NormalizeLink(e.URL); l != "" {
				link = l
			}
		}
	}
	if link == "" {
		link = "https://discord.com/channels/" + fc.GuildID + "/" + fc.ChannelID + "/" + m.ID
	}
	date, err := time.Parse(time.RFC3339Nano, m.Timestamp)
	if err != nil {
		date = snowflakeTime(m.ID, discordEpochMS)
	}
	author := PlainText(m.Author.Username)
	if author == "" {
		author = PlainText(m.Author.GlobalName)
	}
	src := "Discord"
	if author != "" {
		src = author + " (Discord)"
	}
	if title == "" {
		title = "Post by " + src
	}
	return Item{
		GUID: "discord:" + m.ID, Link: link, Title: Truncate(title, maxSubject),
		Summary: PlainText(strings.Join(rest, " — ")), Date: date.UTC(), Author: author, Source: src,
	}, true
}
