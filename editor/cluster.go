package editor

import (
	"io"
	"sort"
	"strings"
)

// Clusterer assigns raw items to stories by canonical URL: an item joins the
// open story that already has its canonical URL, and otherwise starts a new
// one. No model is called. (Embedding clustering was removed in round 0 of
// the 2026-11-08 audit: on 617 live items `measure` found 617 unique
// canonical URLs and 0 near-duplicate pairs. Re-run `measure` when a topic
// draws on several sources before bringing it back.)
type Clusterer struct {
	Cfg   ClusterConfig
	State *State
	Log   io.Writer
}

// storyID is the id of the story a raw item starts.
func storyID(firstItem string) string {
	s := strings.TrimPrefix(firstItem, "m_")
	if len(s) > 20 {
		s = s[:20]
	}
	return "s_" + s
}

func (c *Clusterer) open(st *Story, ts int64) bool {
	return ts-st.Created <= int64(c.Cfg.WindowHours)*3600_000 && ts >= st.Created-int64(c.Cfg.WindowHours)*3600_000
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func toStoryItem(it Item) StoryItem {
	return StoryItem{ID: it.ID, Channel: it.Channel, TS: it.TS, Title: it.Title, Link: it.Target,
		Canon: it.Canon, Via: it.Via, Excerpt: clipText(it.Excerpt, maxExcerpt)}
}

func (c *Clusterer) addTo(st *Story, it Item) {
	st.Items = append(st.Items, toStoryItem(it))
	if it.TS > st.Last {
		st.Last = it.TS
	}
	if it.Canon != "" && !contains(st.Canon, it.Canon) {
		st.Canon = append(st.Canon, it.Canon)
	}
	c.State.Items[it.ID] = st.ID
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Assign clusters every item not yet assigned, oldest first, and returns the
// ids of the stories it created. An item joins the story whose canonical URL
// it shares if that story is open for it (the item is within WindowHours of
// the story's first item); otherwise, and always for an item with no
// canonical URL, it starts a new story.
func (c *Clusterer) Assign(items []Item) []string {
	var todo []Item
	for _, it := range items {
		if _, done := c.State.Items[it.ID]; !done {
			todo = append(todo, it)
		}
	}
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].TS < todo[j].TS })

	byCanon := map[string]*Story{}
	for _, st := range c.State.Stories {
		for _, k := range st.Canon {
			if prev, ok := byCanon[k]; !ok || st.Created > prev.Created {
				byCanon[k] = st
			}
		}
	}
	var created []string
	for _, it := range todo {
		if it.Canon != "" {
			if st, ok := byCanon[it.Canon]; ok && c.open(st, it.TS) {
				c.addTo(st, it)
				continue
			}
		}
		st := &Story{ID: storyID(it.ID), Created: it.TS, Last: it.TS}
		c.State.Stories[st.ID] = st
		c.addTo(st, it)
		created = append(created, st.ID)
		if it.Canon != "" {
			byCanon[it.Canon] = st
		}
	}
	return created
}
