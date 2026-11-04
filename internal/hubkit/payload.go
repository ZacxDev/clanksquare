package hubkit

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// Payloads are encoded from structs, so field order and bytes are fixed for a
// given input. HTML escaping is off: "<" is valid JSON and the signature
// covers the exact bytes either way.

// Common is the header every signed object carries (SPEC §3).
type Common struct {
	V    int    `json:"v"`
	T    string `json:"t"`
	From string `json:"from"`
	TS   int64  `json:"ts"`
}

// Header is the Common header of an object of type t signed by priv.
func Header(priv ed25519.PrivateKey, t string, ts int64) Common {
	return Common{V: 1, T: t, From: KeyIDOf(priv), TS: ts}
}

// KeyIDOf is priv's k_ key id.
func KeyIDOf(priv ed25519.PrivateKey) string {
	return clankmail.KeyID(priv.Public().(ed25519.PublicKey))
}

// Encode marshals a payload struct without HTML escaping or a trailing newline.
func Encode(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // only plain structs of strings and ints are encoded
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// MsgPayload is a msg to one channel.
func MsgPayload(priv ed25519.PrivateKey, channelID, subject, body string, ts int64) []byte {
	return Encode(struct {
		Common
		To      []string `json:"to"`
		Subject string   `json:"subject,omitempty"`
		Body    string   `json:"body"`
	}{Header(priv, "msg", ts), []string{channelID}, subject, body})
}

// probePayload is a throwaway handle payload used to check that a key signs
// objects the spec accepts.
func probePayload(priv ed25519.PrivateKey) []byte {
	return Encode(struct {
		Common
		Name string `json:"name"`
		Hub  string `json:"hub"`
	}{Header(priv, "handle", 1), "probe", "hub.example"})
}

// TokenID is the m_ id of a token (the hash of its payload), or "" if the
// token is not four dot-separated parts with a base64url payload.
func TokenID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ""
	}
	return clankmail.ObjectID(payload)
}

// StrList is the strings of a decoded JSON array; anything else is skipped.
func StrList(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// VerifyChannel checks a channel record the hub served (§4, §7.1): both
// tokens verify, the genesis hashes to the channel id, and the manifest is the
// genesis or an update of this channel signed by its owner. With slug != "",
// the manifest must carry that slug. It returns the genesis and the manifest.
func VerifyChannel(resp *ChannelResp, slug string) (gen, man *clankmail.Object, err error) {
	gen, gerr := clankmail.Verify(resp.Genesis)
	man, merr := clankmail.Verify(resp.Manifest)
	switch {
	case gerr != nil || merr != nil:
		return nil, nil, fmt.Errorf("unverifiable channel record (genesis: %v, manifest: %v)", gerr, merr)
	case gen.Type != "channel" || gen.Fields["channel"] != nil || clankmail.ChannelID(gen.Payload) != resp.Channel:
		return nil, nil, fmt.Errorf("genesis does not match channel id %s", resp.Channel)
	case man.Type != "channel" || man.From != gen.From ||
		man.ID != gen.ID && man.Fields["channel"] != resp.Channel || slug != "" && man.Fields["slug"] != slug:
		return nil, nil, fmt.Errorf("manifest is not a valid manifest of %s", resp.Channel)
	}
	return gen, man, nil
}

// WriteFileAtomic writes data to path: a temp file in the same directory,
// synced, chmod 0600, then renamed over the old one. A crash leaves the old
// or the new file, never a partial one.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
