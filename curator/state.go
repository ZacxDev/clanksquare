package curator

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// State is a cache of what the curator has posted. The channel log is the
// truth: a channel with no entry here is rebuilt from its log (rebuildSeen).
//
// File format (JSON, version 1):
//
//	{"version":1,"channels":{"c_…":{"slug":"…","seen":{"<dedupe key>":<first-seen unix ms>},
//	                                 "pending":{"<dedupe key>":"<signed token>"}}}}
//
// Seen keys are never forgotten. A pending entry is a write-ahead record: it
// is saved before its token is submitted and replaced by a seen key once the
// hub confirms it, so a post the hub stored but never confirmed (a lost
// answer, a crash, SIGTERM) is resolved by resending the same bytes instead
// of being signed and posted a second time.
type State struct {
	Version  int                      `json:"version"`
	Channels map[string]*ChannelState `json:"channels"`
}

// ChannelState is one channel's seen set, keyed by channel id so a slug that
// comes to name a different channel never inherits another channel's set.
type ChannelState struct {
	Slug    string            `json:"slug"`
	Seen    map[string]int64  `json:"seen"`
	Pending map[string]string `json:"pending,omitempty"`
}

func newState() *State { return &State{Version: 1, Channels: map[string]*ChannelState{}} }

// LoadState reads a state file. A missing file returns (nil, nil): there is
// nothing cached and every channel is rebuilt from its log. An unreadable or
// malformed file is an error the caller treats the same way, loudly.
func LoadState(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("state %s: %w", path, err)
	}
	if s.Version != 1 {
		return nil, fmt.Errorf("state %s: unsupported version %d", path, s.Version)
	}
	if s.Channels == nil {
		s.Channels = map[string]*ChannelState{}
	}
	for id, cs := range s.Channels {
		if cs == nil || cs.Seen == nil && len(cs.Pending) == 0 {
			delete(s.Channels, id) // rebuilt from the log
			continue
		}
		if cs.Seen == nil {
			cs.Seen = map[string]int64{}
		}
		cs.rekey()
	}
	return &s, nil
}

// Save writes the state atomically: a temp file in the same directory,
// synced, then renamed over the old one. A crash leaves the old or the new
// file, never a partial one.
func (s *State) Save(path string) error {
	raw, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	return hubkit.WriteFileAtomic(path, append(raw, '\n'))
}

// rekey re-derives every key with the current NormalizeLink, so a key saved
// by an older normalisation (an arXiv link with its version, before every
// version mapped to https://arxiv.org/abs/<id>) still matches the key the same
// item gets now. Keys that collapse into one keep the earliest first-seen
// time; pending entries that collapse keep the one whose old key sorts
// first. Keys are keyFromLine outputs, and NormalizeLink only ever narrows
// what it maps (never widens what it accepts), so a link key stays a link key
// and a guid key stays as it is.
func (cs *ChannelState) rekey() {
	seen := make(map[string]int64, len(cs.Seen))
	for k, at := range cs.Seen {
		k = rekeyOne(k)
		if prev, ok := seen[k]; !ok || at < prev {
			seen[k] = at
		}
	}
	cs.Seen = seen
	if len(cs.Pending) > 0 {
		keys := make([]string, 0, len(cs.Pending))
		for k := range cs.Pending {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pending := make(map[string]string, len(cs.Pending))
		for _, k := range keys {
			nk := rekeyOne(k)
			if _, dup := pending[nk]; !dup {
				pending[nk] = cs.Pending[k]
			}
		}
		cs.Pending = pending
	}
}

func rekeyOne(k string) string {
	if strings.HasPrefix(k, "guid:") {
		return k
	}
	if n := NormalizeLink(k); n != "" {
		return n
	}
	return k
}
