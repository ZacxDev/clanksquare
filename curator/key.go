package curator

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// Key file format: one line holding base64url (RFC 4648 §5, no padding) of
// the 32-byte Ed25519 seed, optionally followed by a newline. Nothing else.

// GenerateKeyFile writes a fresh key to path with mode 0600 and returns its
// k_ key id. It refuses to overwrite an existing file.
func GenerateKeyFile(path string) (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(base64.RawURLEncoding.EncodeToString(priv.Seed()) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return "", werr
	}
	return clankmail.KeyID(pub), nil
}

// LoadKeyFile reads a key file written by GenerateKeyFile.
func LoadKeyFile(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSuffix(string(raw), "\n")
	seed, err := base64.RawURLEncoding.DecodeString(s)
	// Canonical form only (SPEC.md §2 discipline): re-encode and compare.
	if err != nil || len(seed) != ed25519.SeedSize || base64.RawURLEncoding.EncodeToString(seed) != s {
		return nil, fmt.Errorf("key file %s: want base64url of a 32-byte Ed25519 seed", path)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	// A key the spec rejects (small order) would make every submit fail.
	if _, err := clankmail.Verify(clankmail.Sign(priv, handlePayload(priv, "probe", "hub.example", 1))); err != nil {
		return nil, errors.New("key file " + path + ": key is not usable: " + err.Error())
	}
	return priv, nil
}
