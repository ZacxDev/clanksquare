package curator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// fetchSource reads one source of any kind into a Feed. Every kind yields the
// same Item shape, so what happens next (filters, dedupe, caps, posting) is
// shared and kind-blind.
func (r *runner) fetchSource(ctx context.Context, ch *Channel, fc FeedConfig) (*Feed, error) {
	switch fc.Kind {
	case KindXSearch:
		return r.fetchXSearch(ctx, ch, fc)
	case KindDiscord:
		return r.fetchDiscord(ctx, ch, fc)
	}
	f, err := FetchFeed(ctx, r.Client, fc.URL)
	if err == nil && fc.TitleFromSummary {
		titlesFromSummaries(f)
	}
	return f, err
}

// maxDerivedTitle bounds a title made from a summary or a post's text; the
// whole text still goes in the excerpt.
const maxDerivedTitle = 140

// titlesFromSummaries gives every untitled item the start of its summary as
// its title.
func titlesFromSummaries(f *Feed) {
	for i := range f.Items {
		if f.Items[i].Title == "" {
			f.Items[i].Title = Truncate(f.Items[i].Summary, maxDerivedTitle)
		}
	}
}

// skipError is a source deliberately not read this run (not a failure: it
// counts neither as ok nor as failed).
type skipError struct {
	level string // INFO or WARN
	msg   string
}

func (e *skipError) Error() string { return e.msg }

func skipf(level, format string, args ...any) error {
	return &skipError{level: level, msg: fmt.Sprintf(format, args...)}
}

// maxSecretBytes bounds a secret file.
const maxSecretBytes = 4096

// secret reads a secret from a file once per run. Its value is never logged
// or put in an error: errors name the file only.
func (r *runner) secret(what, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no %s file configured", what)
	}
	if v, ok := r.secrets[path]; ok {
		return v, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return "", fmt.Errorf("read %s file %s: %v", what, path, err)
	}
	if len(raw) > maxSecretBytes {
		return "", fmt.Errorf("%s file %s is over %d bytes", what, path, maxSecretBytes)
	}
	v := strings.TrimSpace(string(raw))
	if v == "" || strings.ContainsAny(v, " \t\r\n") {
		return "", fmt.Errorf("%s file %s: want one token on one line", what, path)
	}
	r.secrets[path] = v
	return v, nil
}

// sourceID is a stable id for an xsearch source: its handles and query.
func (f FeedConfig) sourceID() string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.Join(f.AllowedXHandles, ",")) + "\n" + f.Query))
	return hex.EncodeToString(h[:6])
}

// Snowflakes: X and Discord ids carry their creation time in the top bits.
const (
	twitterEpochMS = 1288834974657
	discordEpochMS = 1420070400000
)

func snowflakeTime(id string, epochMS int64) time.Time {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n>>22 == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(n>>22) + epochMS).UTC()
}

// discordSnowflakeAt is the smallest Discord id created at t.
func discordSnowflakeAt(t time.Time) string {
	ms := t.UnixMilli() - discordEpochMS
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

// snowflakeLess compares two decimal ids numerically.
func snowflakeLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
