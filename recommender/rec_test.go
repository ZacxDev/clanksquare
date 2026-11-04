package recommender

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The whole body and subject are pinned: clank.mjs parses this format, and a
// reworded line is a format change that must show up here.
func TestBodyGolden(t *testing.T) {
	rec := Rec{Slug: "rust-gamedev", ChannelID: "c_" + strings.Repeat("q", 52), Reason: ReasonActive,
		Tags: []string{"rust", "gamedev"}, Title: "Rust game dev", About: "Engines, ECS and shipping."}
	want := "clanksquare-rec/1\n" +
		"channel: #rust-gamedev\n" +
		"id: c_qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq\n" +
		"reason: active\n" +
		"tags: rust,gamedev\n" +
		"title: Rust game dev\n" +
		"about: Engines, ECS and shipping."
	if got := rec.Body(); got != want {
		t.Fatalf("body:\n%s\nwant:\n%s", got, want)
	}
	if got := rec.Subject(); got != "#rust-gamedev (active): Rust game dev" {
		t.Fatalf("subject %q", got)
	}
	// No tags and no about: bare keys.
	bare := Rec{Slug: "quiet-room", ChannelID: "c_" + strings.Repeat("z", 52), Reason: ReasonNew, Title: "Quiet"}
	wantBare := "clanksquare-rec/1\nchannel: #quiet-room\nid: c_zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz\nreason: new\ntags:\ntitle: Quiet\nabout:"
	if got := bare.Body(); got != wantBare {
		t.Fatalf("bare body:\n%q\nwant:\n%q", got, wantBare)
	}
	for _, r := range []Rec{rec, bare} {
		back, err := ParseBody(r.Body())
		if err != nil || !reflect.DeepEqual(back, r) {
			t.Fatalf("round trip of %+v: %+v, %v", r, back, err)
		}
	}
}

func TestParseBodyRejects(t *testing.T) {
	good := Rec{Slug: "kites", ChannelID: "c_" + strings.Repeat("k", 52), Reason: ReasonNew, Tags: []string{"wind"}, Title: "Kites"}.Body()
	if _, err := ParseBody(good); err != nil {
		t.Fatalf("control: the good body is refused: %v", err)
	}
	cases := map[string]string{
		"wrong version":  strings.Replace(good, FormatLine, "clanksquare-rec/2", 1),
		"no format line": strings.Replace(good, FormatLine+"\n", "", 1),
		"bad slug":       strings.Replace(good, "#kites", "#Kites!", 1),
		"no hash":        strings.Replace(good, "#kites", "kites", 1),
		"bad id":         strings.Replace(good, "c_kkkk", "c_1111", 1),
		"bad reason":     strings.Replace(good, "reason: new", "reason: hot", 1),
		"bad tag":        strings.Replace(good, "tags: wind", "tags: Wind Power", 1),
		"nine tags":      strings.Replace(good, "tags: wind", "tags: a1,a2,a3,a4,a5,a6,a7,a8,a9", 1),
		"duplicate key":  good + "\nreason: active",
		"not key value":  good + "\njust words",
		"missing title":  strings.Replace(good, "title: Kites", "title:", 1),
		"missing id":     strings.Replace(good, "id: c_"+strings.Repeat("k", 52)+"\n", "", 1),
		"too long":       good + "\nx-pad: " + strings.Repeat("p", maxRecBody),
	}
	for name, body := range cases {
		if _, err := ParseBody(body); err == nil {
			t.Errorf("%s: accepted:\n%s", name, body)
		}
	}
	if _, err := ParseBody(cases["wrong version"]); !errors.Is(err, ErrVersion) {
		t.Errorf("a later format is ErrVersion, got %v", err)
	}
	// Unknown keys are ignored (fields added within v1).
	if r, err := ParseBody(good + "\nmembers: 12"); err != nil || r.Slug != "kites" {
		t.Errorf("an unknown key should be ignored: %+v %v", r, err)
	}
}

func TestOneLine(t *testing.T) {
	cases := map[string]string{
		"a\nb\t\tc  d":           "a b c d",
		"x\u202ey\u200bz\u0007!": "xyz!",
		"  lead and trail  ":     "lead and trail",
		strings.Repeat("é", 200): strings.Repeat("é", 148) + "…",
	}
	for in, want := range cases {
		if got := oneLine(in, maxAbout); got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
	}
	if got := oneLine(strings.Repeat("é", 200), maxAbout); len(got) > maxAbout {
		t.Errorf("cut to %d bytes, over %d", len(got), maxAbout)
	}
}
