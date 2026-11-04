package curator

import (
	"crypto/ed25519"

	"github.com/ZacxDev/clanksquare/internal/hubkit"
)

// Key file format: one line holding base64url (RFC 4648 §5, no padding) of
// the 32-byte Ed25519 seed, optionally followed by a newline. Nothing else.

// GenerateKeyFile writes a fresh key to path with mode 0600 and returns its
// k_ key id. It refuses to overwrite an existing file.
func GenerateKeyFile(path string) (string, error) { return hubkit.GenerateKeyFile(path) }

// LoadKeyFile reads a key file written by GenerateKeyFile.
func LoadKeyFile(path string) (ed25519.PrivateKey, error) { return hubkit.LoadKeyFile(path) }
