package editor

import (
	"sort"
	"time"
)

// Picking by rank.
//
// A top channel posts at most decide.top_k_per_day stories per UTC day. Each
// run ranks the channel's eligible stories by P(post), highest first, ties by
// story id (ascending), and posts the first (budget - posted today) of them
// whose P(post) is at least decide.min_p_post.
//
// Eligible: decided for the topic, not posted (seen) and not pending, and
// first seen (Story.Created) less than decide.window_hours (72) ago. A story
// that is not picked today stays eligible and is ranked again on the next
// run, against that day's stories, until it leaves the window; it is never
// re-decided (its P(post) is the one it got when it first appeared). Once it
// is older than the window it is never posted.
//
// Posted today is counted from BOTH the state file's seen set and the
// channel's own log (every run reads it; rebuildSeen), and pending posts
// count too, so losing the state file can never post a second day's worth.

// Candidate is one eligible story with its P(post).
type Candidate struct {
	Story string
	PPost float64
}

// RankPick orders cands by P(post) descending, ties by story id ascending,
// drops those below floor, and returns at most budget of them. It is pure.
func RankPick(cands []Candidate, budget int, floor float64) []Candidate {
	if budget <= 0 {
		return nil
	}
	sorted := append([]Candidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].PPost != sorted[j].PPost {
			return sorted[i].PPost > sorted[j].PPost
		}
		return sorted[i].Story < sorted[j].Story
	})
	var out []Candidate
	for _, c := range sorted {
		if len(out) == budget {
			break
		}
		if c.PPost < floor {
			break // sorted: every later one is lower
		}
		out = append(out, c)
	}
	return out
}

// utcDay returns the [start, end) of now's UTC day, unix ms.
func utcDay(now time.Time) (int64, int64) {
	u := now.UTC()
	start := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return start.UnixMilli(), start.Add(24 * time.Hour).UnixMilli()
}

// PostedToday counts the stories this channel has posted in now's UTC day:
// the seen entries (state merged with the log; see mergeSeen) dated today,
// plus every pending post (written ahead, maybe stored), which counts as
// today whatever its date: a pending post may already be on the hub.
func PostedToday(cs *ChannelState, now time.Time) int {
	start, end := utcDay(now)
	n := 0
	for sid, at := range cs.Seen {
		if _, pending := cs.Pending[sid]; pending {
			continue
		}
		if at >= start && at < end {
			n++
		}
	}
	return n + len(cs.Pending)
}

// mergeSeen adds the log's posted stories to the state's seen set, keeping
// the earlier date of a story in both.
func mergeSeen(seen, fromLog map[string]int64) {
	for sid, at := range fromLog {
		if prev, ok := seen[sid]; !ok || at < prev {
			seen[sid] = at
		}
	}
}
