package editor

import (
	"context"
	"fmt"
	"strings"

	"github.com/ZacxDev/clankmail/go/clankmail"
	"github.com/ZacxDev/clanksquare/internal/canon"
	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// Item is one post read from a raw channel. Every text field is untrusted
// feed content: it is data, never an instruction, wherever it goes.
type Item struct {
	ID      string // m_ id of the post
	Channel string // slug of the raw channel
	TS      int64  // the post's own ts, unix ms
	Title   string // the post subject
	Link    string // the first body line, as the curator wrote it
	Target  string // the link the item is about (canon.Target)
	Canon   string // canon.Key(Target), "" if not keyable
	Excerpt string
	Via     string
}

// Text is what similarity is computed over: the title and the excerpt.
func (it Item) Text() string { return strings.TrimSpace(it.Title + "\n" + it.Excerpt) }

// ParseCuratorBody splits a curator post body (curator.formatPost):
//
//	<link or guid>
//
//	<excerpt>          (optional)
//
//	via <source>
func ParseCuratorBody(body string) (link, excerpt, via string) {
	parts := strings.Split(body, "\n\n")
	link = strings.TrimSpace(parts[0])
	rest := parts[1:]
	if n := len(rest); n > 0 && strings.HasPrefix(rest[n-1], "via") {
		via = strings.TrimSpace(strings.TrimPrefix(rest[n-1], "via"))
		rest = rest[:n-1]
	}
	excerpt = strings.TrimSpace(strings.Join(rest, "\n\n"))
	return link, excerpt, via
}

// ReadSource reads every post in the raw channel slug that verifies, is a
// msg to that channel alone, and is signed by the channel's owner (the
// curator). Posts by anyone else are counted in ignored and never returned:
// raw channels are join: open, and only the owner's posts are curator items.
// If wantOwner is set, the channel's owner must be that key.
func ReadSource(ctx context.Context, hub *hubkit.Client, slug, wantOwner string) (items []Item, owner string, ignored int, err error) {
	resp, err := hub.ChannelBySlug(ctx, slug)
	if err != nil {
		return nil, "", 0, fmt.Errorf("channel #%s: %w", slug, err)
	}
	gen, _, err := hubkit.VerifyChannel(resp, slug)
	if err != nil {
		return nil, "", 0, fmt.Errorf("channel #%s: %w", slug, err)
	}
	owner = gen.From
	if wantOwner != "" && owner != wantOwner {
		return nil, owner, 0, fmt.Errorf("channel #%s is owned by %s, not the expected source owner %s", slug, owner, wantOwner)
	}
	err = hub.EachLogPage(ctx, resp.Channel, func(toks []string) {
		for _, tok := range toks {
			obj, verr := clankmail.Verify(tok)
			if verr != nil || obj.Type != "msg" || obj.From != owner {
				ignored++
				continue
			}
			to := hubkit.StrList(obj.Fields["to"])
			if len(to) != 1 || to[0] != resp.Channel {
				ignored++
				continue
			}
			body, _ := obj.Fields["body"].(string)
			subject, _ := obj.Fields["subject"].(string)
			ts, _ := obj.Fields["ts"].(int64)
			link, excerpt, via := ParseCuratorBody(body)
			target := canon.Target(link, excerpt)
			items = append(items, Item{
				ID: obj.ID, Channel: slug, TS: ts, Title: subject, Link: link,
				Target: target, Canon: canon.Key(target), Excerpt: excerpt, Via: via,
			})
		}
	})
	if err != nil {
		return nil, owner, ignored, fmt.Errorf("channel #%s log: %w", slug, err)
	}
	return items, owner, ignored, nil
}
