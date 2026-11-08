package editor

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ZacxDev/clanksquare/internal/canon"
)

// ChannelMeasure is one raw channel's duplicate statistics.
type ChannelMeasure struct {
	Slug         string
	Items        int
	Unkeyed      int // items whose link has no canonical key
	UniqueCanon  int // distinct canonical keys among keyed items
	DupItems     int // keyed items whose key an earlier item in the channel had
	NearDupPairs int // pairs within the channel that are near-duplicates
	// NearDupNewPairs are near-duplicate pairs whose canonical keys differ:
	// what URL canonicalisation alone would miss.
	NearDupNewPairs int
}

// DupRate is the share of items that repeat an earlier item's canonical key.
func (c ChannelMeasure) DupRate() float64 {
	if c.Items == 0 {
		return 0
	}
	return float64(c.DupItems) / float64(c.Items)
}

// Collapse is one canonical key that more than one distinct raw link mapped to.
type Collapse struct {
	Key   string
	Links []string
}

// Measure is the result of a measure run.
type Measure struct {
	Channels        []ChannelMeasure
	Items           int
	UniqueCanon     int
	CrossChannelDup int // canonical keys seen in more than one channel
	NearDupPairs    int
	NearDupNewPairs int
	Collapses       []Collapse // keys reached from ≥2 distinct raw links, sorted
	ExamplePairs    [][2]Item  // up to 5 near-duplicate pairs with differing keys
	// MinHamming is the closest pair's SimHash distance (-1: fewer than two
	// non-empty texts), and HammingLE[d] the pairs at distance <= d for
	// d = 0..16: the distribution the near-dup cut is read against.
	MinHamming int
	HammingLE  [17]int
	// Control is a real item's text with its last word dropped: its
	// distance to the original shows the detector can fire on this data.
	Control     string
	ControlDist int
}

// Measure computes duplicate statistics over items (any order). It is pure.
func MeasureItems(items []Item, slugs []string) *Measure {
	m := &Measure{Items: len(items)}
	bySlug := map[string][]Item{}
	for _, it := range items {
		bySlug[it.Channel] = append(bySlug[it.Channel], it)
	}
	hashes := make([]uint64, len(items))
	empty := make([]bool, len(items))
	for i, it := range items {
		hashes[i] = SimHash(it.Text())
		empty[i] = len(tokens(it.Text())) == 0
	}
	keyChans := map[string]map[string]bool{}
	keyLinks := map[string]map[string]bool{}
	for _, it := range items {
		if it.Canon == "" {
			continue
		}
		if keyChans[it.Canon] == nil {
			keyChans[it.Canon] = map[string]bool{}
			keyLinks[it.Canon] = map[string]bool{}
		}
		keyChans[it.Canon][it.Channel] = true
		keyLinks[it.Canon][it.Target] = true
	}
	m.UniqueCanon = len(keyChans)
	for k, cs := range keyChans {
		if len(cs) > 1 {
			m.CrossChannelDup++
		}
		if len(keyLinks[k]) > 1 {
			var ls []string
			for l := range keyLinks[k] {
				ls = append(ls, l)
			}
			sort.Strings(ls)
			m.Collapses = append(m.Collapses, Collapse{Key: k, Links: ls})
		}
	}
	sort.Slice(m.Collapses, func(i, j int) bool { return m.Collapses[i].Key < m.Collapses[j].Key })

	for _, slug := range slugs {
		its := bySlug[slug]
		sort.SliceStable(its, func(i, j int) bool { return its[i].TS < its[j].TS })
		cm := ChannelMeasure{Slug: slug, Items: len(its)}
		seen := map[string]bool{}
		for _, it := range its {
			if it.Canon == "" {
				cm.Unkeyed++
				continue
			}
			if seen[it.Canon] {
				cm.DupItems++
			}
			seen[it.Canon] = true
		}
		cm.UniqueCanon = len(seen)
		m.Channels = append(m.Channels, cm)
	}
	chanIdx := map[string]int{}
	for i, c := range m.Channels {
		chanIdx[c.Slug] = i
	}
	m.MinHamming = -1
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			if empty[i] || empty[j] {
				continue
			}
			d := Hamming(hashes[i], hashes[j])
			if m.MinHamming < 0 || d < m.MinHamming {
				m.MinHamming = d
			}
			for k := d; k < len(m.HammingLE); k++ {
				m.HammingLE[k]++
			}
			if d > NearDupBits {
				continue
			}
			m.NearDupPairs++
			differ := items[i].Canon != items[j].Canon || items[i].Canon == ""
			if differ {
				m.NearDupNewPairs++
				if len(m.ExamplePairs) < 5 {
					m.ExamplePairs = append(m.ExamplePairs, [2]Item{items[i], items[j]})
				}
			}
			if items[i].Channel == items[j].Channel {
				if c, ok := chanIdx[items[i].Channel]; ok {
					m.Channels[c].NearDupPairs++
					if differ {
						m.Channels[c].NearDupNewPairs++
					}
				}
			}
		}
	}
	for i, it := range items {
		if empty[i] {
			continue
		}
		toks := tokens(it.Text())
		if len(toks) < 20 {
			continue
		}
		cut := strings.Join(toks[:len(toks)-1], " ")
		m.Control = it.ID
		m.ControlDist = Hamming(hashes[i], SimHash(cut))
		break
	}
	return m
}

// Write prints the report.
func (m *Measure) Write(w io.Writer) {
	fmt.Fprintf(w, "total items=%d unique_canonical_urls=%d cross_channel_dup_urls=%d near_dup_pairs=%d near_dup_pairs_with_distinct_urls=%d\n",
		m.Items, m.UniqueCanon, m.CrossChannelDup, m.NearDupPairs, m.NearDupNewPairs)
	for _, c := range m.Channels {
		fmt.Fprintf(w, "channel=%s items=%d unkeyed=%d unique_canonical=%d dup_items=%d dup_rate=%.4f near_dup_pairs=%d near_dup_pairs_with_distinct_urls=%d\n",
			c.Slug, c.Items, c.Unkeyed, c.UniqueCanon, c.DupItems, c.DupRate(), c.NearDupPairs, c.NearDupNewPairs)
	}
	fmt.Fprintf(w, "simhash: near-dup cut <= %d bits; closest pair %d bits; pairs within d bits:", NearDupBits, m.MinHamming)
	for _, d := range []int{3, 6, 8, 10, 12, 16} {
		fmt.Fprintf(w, " d%d=%d", d, m.HammingLE[d])
	}
	fmt.Fprintln(w)
	if m.Control != "" {
		fmt.Fprintf(w, "simhash positive control: %s with its last word dropped is %d bits from the original (near-dup=%v)\n", m.Control, m.ControlDist, m.ControlDist <= NearDupBits)
	}
	fmt.Fprintf(w, "canonical keys reached from more than one raw link: %d\n", len(m.Collapses))
	for i, c := range m.Collapses {
		if i == 10 {
			fmt.Fprintf(w, "  … %d more\n", len(m.Collapses)-10)
			break
		}
		fmt.Fprintf(w, "  %s <= %v\n", c.Key, c.Links)
	}
	if len(m.Collapses) == 0 {
		a, b := "https://arxiv.org/abs/2610.40556v1", "https://arxiv.org/pdf/2610.40556v2"
		fmt.Fprintf(w, "  (none in the data) constructed control: %s -> %s, %s -> %s\n", a, canon.Key(a), b, canon.Key(b))
	}
	for _, p := range m.ExamplePairs {
		fmt.Fprintf(w, "near-dup example: [%s] %q %s | [%s] %q %s\n", p[0].Channel, p[0].Title, p[0].Canon, p[1].Channel, p[1].Title, p[1].Canon)
	}
}
