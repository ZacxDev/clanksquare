package curator

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// Payloads are encoded from structs, so field order and bytes are fixed for a
// given input. HTML escaping is off: "<" is valid JSON and the signature
// covers the exact bytes either way.

type common struct {
	V    int    `json:"v"`
	T    string `json:"t"`
	From string `json:"from"`
	TS   int64  `json:"ts"`
}

func header(priv ed25519.PrivateKey, t string, ts int64) common {
	return common{V: 1, T: t, From: clankmail.KeyID(priv.Public().(ed25519.PublicKey)), TS: ts}
}

func encode(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // only plain structs of strings and ints are encoded
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func handlePayload(priv ed25519.PrivateKey, name, hub string, ts int64) []byte {
	return encode(struct {
		common
		Name string `json:"name"`
		Hub  string `json:"hub"`
	}{header(priv, "handle", ts), name, hub})
}

// Channel policy the curator always uses (the SPEC.md §7.1 client defaults).
const (
	policyRead = "public"
	policyPost = "members"
	policyJoin = "open"
)

// manifest is this channel's genesis (channelID "") or update payload. mods
// is carried over from the current manifest so an update never drops them.
func (ch *Channel) manifest(priv ed25519.PrivateKey, channelID string, ts int64, mods ...string) []byte {
	return encode(struct {
		common
		Channel string   `json:"channel,omitempty"`
		Slug    string   `json:"slug"`
		Title   string   `json:"title"`
		About   string   `json:"about,omitempty"`
		Tags    []string `json:"tags,omitempty"`
		Read    string   `json:"read"`
		Post    string   `json:"post"`
		Join    string   `json:"join"`
		Mods    []string `json:"mods,omitempty"`
	}{header(priv, "channel", ts), channelID, ch.Slug, ch.Title, ch.About, ch.Tags,
		policyRead, policyPost, policyJoin, mods})
}

func msgPayload(priv ed25519.PrivateKey, channelID, subject, body string, ts int64) []byte {
	return encode(struct {
		common
		To      []string `json:"to"`
		Subject string   `json:"subject,omitempty"`
		Body    string   `json:"body"`
	}{header(priv, "msg", ts), []string{channelID}, subject, body})
}

// manifestDiffers reports whether a stored manifest's fields differ from what
// ch asks for. Absent optional fields compare equal to empty ones.
func (ch *Channel) manifestDiffers(f map[string]any) bool {
	str := func(k string) string { s, _ := f[k].(string); return s }
	if str("slug") != ch.Slug || str("title") != ch.Title || str("about") != ch.About ||
		str("read") != policyRead || str("post") != policyPost || str("join") != policyJoin {
		return true
	}
	have := strList(f["tags"])
	if len(have) != len(ch.Tags) {
		return true
	}
	for i := range have {
		if have[i] != ch.Tags[i] {
			return true
		}
	}
	return false
}

func strList(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
