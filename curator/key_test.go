package curator

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

func TestKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curator.key")
	id, err := GenerateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v, want 0600", fi.Mode().Perm())
	}
	priv, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if clankmail.KeyID(priv.Public().(ed25519.PublicKey)) != id {
		t.Error("loaded key does not match the printed id")
	}
	if !strings.HasPrefix(id, "k_") || len(id) != 54 {
		t.Errorf("id %q is not a k_ key id", id)
	}
	before, _ := os.ReadFile(path)
	if _, err := GenerateKeyFile(path); err == nil {
		t.Fatal("GenerateKeyFile overwrote an existing file")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("refused overwrite still changed the file")
	}
	for name, content := range map[string]string{
		"padded": strings.TrimSuffix(string(before), "\n") + "=\n",
		"short":  "AAAA\n",
		"crlf":   strings.TrimSuffix(string(before), "\n") + "\r\n",
		"empty":  "",
	} {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, []byte(content), 0o600)
		if _, err := LoadKeyFile(p); err == nil {
			t.Errorf("LoadKeyFile accepted a %s key file", name)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	ok := func() *Config {
		return &Config{Hub: "hub.example", HubURL: "https://hub.example/", Channels: []Channel{{
			Slug: "news", Title: "News", Feeds: []FeedConfig{{URL: "https://feed.example/a"}}}}}
	}
	c := ok()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.MaxAgeHours != 48 || c.Channels[0].MaxPerRun != 5 || c.Channels[0].MaxAgeHours != 48 || c.HubURL != "https://hub.example" {
		t.Errorf("defaults not applied: %+v", c)
	}
	bad := map[string]func(*Config){
		"no hub":       func(c *Config) { c.Hub = "" },
		"bad hub":      func(c *Config) { c.Hub = "Hub_Example" },
		"no hub url":   func(c *Config) { c.HubURL = "" },
		"bad handle":   func(c *Config) { c.Handle = "X" },
		"bad slug":     func(c *Config) { c.Channels[0].Slug = "#news" },
		"long title":   func(c *Config) { c.Channels[0].Title = strings.Repeat("t", 81) },
		"bad tag":      func(c *Config) { c.Channels[0].Tags = []string{"Bad Tag"} },
		"dup slug":     func(c *Config) { c.Channels = append(c.Channels, c.Channels[0]) },
		"no feeds":     func(c *Config) { c.Channels[0].Feeds = nil },
		"feed scheme":  func(c *Config) { c.Channels[0].Feeds[0].URL = "file:///etc/passwd" },
		"max per run":  func(c *Config) { c.Channels[0].MaxPerRun = 51 },
		"max age > 1y": func(c *Config) { c.MaxAgeHours = 365*24 + 1 },
	}
	for name, mut := range bad {
		c := ok()
		mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig("../cmd/curator/example-config.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Channels) != 4 {
		t.Errorf("example has %d channels, want 4", len(c.Channels))
	}
	p := filepath.Join(t.TempDir(), "typo.json")
	os.WriteFile(p, []byte(`{"hub":"hub.example","hub_url":"https://hub.example","channels":[{"slug":"ab","title":"t","feeds":[{"url":"https://f.example","includes":["x"]}]}]}`), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Error("an unknown field (a typo of include) was accepted")
	}
}
