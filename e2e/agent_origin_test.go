package e2e

// The hosted MCP server's Origin rule and CORS (streamable-http §Security):
// no --allow-origin refuses every Origin, an exact list admits only its
// members, and --allow-origin '*' admits any Origin with CORS headers a
// browser client needs (preflight included).

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// corsDo sends one request to the hosted server and returns the status and
// headers. body nil sends none; hd are added as given.
func corsDo(t *testing.T, method, url string, body []byte, hd map[string]string) (int, http.Header) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	for k, v := range hd {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// preflight is what a browser sends before a cross-origin MCP POST.
func preflight(origin string) map[string]string {
	return map[string]string{
		"Origin":                         origin,
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "content-type,mcp-method,mcp-name,mcp-protocol-version",
	}
}

// tokens splits a comma-separated header into lower-cased names.
func tokens(v string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(v, ",") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out[s] = true
		}
	}
	return out
}

func TestClankMCPHTTPOrigin(t *testing.T) {
	h := startHub(t)
	srv := &agent{t: t, env: append(os.Environ(), "CLANK_HUB="+h.URL, "CLANK_HUB_NAME="+hubName, "CLANK_KEY="+filepath.Join(t.TempDir(), "must-not-exist"))}
	disc := rpcBody(1, "server/discover", withMeta(nil))
	withOrigin := func(origin string) map[string]string {
		hd := stdHeaders(disc)
		if origin != "" {
			hd["Origin"] = origin
		}
		return hd
	}
	const foreign = "https://evil.example"

	t.Run("any", func(t *testing.T) {
		url := startHTTPMCP(t, srv, "--allow-origin", "*")
		// A foreign Origin is served, with Access-Control-Allow-Origin: * and
		// no credentials.
		code, hd := corsDo(t, "POST", url, disc, withOrigin(foreign))
		if code != 200 || hd.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("'*', Origin %s: HTTP %d, Access-Control-Allow-Origin %q; want 200 and *", foreign, code, hd.Get("Access-Control-Allow-Origin"))
		}
		if hd.Get("Access-Control-Allow-Credentials") != "" || !tokens(hd.Get("Access-Control-Expose-Headers"))["retry-after"] {
			t.Fatalf("'*': want no Allow-Credentials and Retry-After exposed, got %v", hd)
		}
		// So is "null" (a sandboxed page or file://).
		if code, hd := corsDo(t, "POST", url, disc, withOrigin("null")); code != 200 || hd.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("'*', Origin null: HTTP %d %v", code, hd)
		}
		// The preflight: 204, the methods and every header a client sends.
		code, hd = corsDo(t, "OPTIONS", url, nil, preflight(foreign))
		if code != 204 || hd.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("'*' preflight: HTTP %d, Access-Control-Allow-Origin %q; want 204 and *", code, hd.Get("Access-Control-Allow-Origin"))
		}
		if !tokens(hd.Get("Access-Control-Allow-Methods"))["post"] {
			t.Fatalf("preflight Access-Control-Allow-Methods %q lacks POST", hd.Get("Access-Control-Allow-Methods"))
		}
		allowed := tokens(hd.Get("Access-Control-Allow-Headers"))
		for _, want := range []string{"content-type", "accept", "mcp-protocol-version", "mcp-method", "mcp-name", "mcp-session-id", "last-event-id"} {
			if !allowed[want] {
				t.Fatalf("preflight Access-Control-Allow-Headers %q lacks %s", hd.Get("Access-Control-Allow-Headers"), want)
			}
		}
		if hd.Get("Access-Control-Max-Age") == "" || hd.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("preflight: want a Max-Age and no Allow-Credentials, got %v", hd)
		}
		// Errors carry the header too, so a browser client can read them:
		// a HeaderMismatch 400, a 405, and the handshake-era 202.
		bad := withOrigin(foreign)
		bad["Mcp-Method"] = "tools/list"
		if code, hd := corsDo(t, "POST", url, disc, bad); code != 400 || hd.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("'*', mismatched Mcp-Method: HTTP %d %v; want 400 with *", code, hd)
		}
		if code, hd := corsDo(t, "GET", url, nil, map[string]string{"Origin": foreign}); code != 405 || hd.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("'*', GET: HTTP %d %v; want 405 with *", code, hd)
		}
		note := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		if code, hd := corsDo(t, "POST", url, note, map[string]string{"Origin": foreign, "MCP-Protocol-Version": legacyVersion}); code != 202 || hd.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("'*', notification: HTTP %d %v; want 202 with *", code, hd)
		}
		// No Origin (a non-browser client): served, no CORS header.
		if code, hd := corsDo(t, "POST", url, disc, withOrigin("")); code != 200 || hd.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("'*', no Origin: HTTP %d %v; want 200 and no CORS header", code, hd)
		}
		// /api/latest is not part of the rule: it sends no CORS header.
		if _, hd := corsDo(t, "GET", strings.TrimSuffix(url, "/mcp")+"/api/latest?c=bad", nil, map[string]string{"Origin": foreign}); hd.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("/api/latest sent a CORS header: %v", hd)
		}
	})

	t.Run("list", func(t *testing.T) {
		const listed = "https://allowed.example"
		url := startHTTPMCP(t, srv, "--allow-origin", listed+",https://other.example")
		// A listed Origin is served and echoed, never "*".
		code, hd := corsDo(t, "POST", url, disc, withOrigin(listed))
		if code != 200 || hd.Get("Access-Control-Allow-Origin") != listed || !tokens(hd.Get("Vary"))["origin"] {
			t.Fatalf("list, Origin %s: HTTP %d %v; want 200, the origin echoed, Vary: Origin", listed, code, hd)
		}
		if code, hd := corsDo(t, "OPTIONS", url, nil, preflight(listed)); code != 204 || hd.Get("Access-Control-Allow-Origin") != listed {
			t.Fatalf("list preflight, Origin %s: HTTP %d %v; want 204 with the origin", listed, code, hd)
		}
		// Any other Origin is still 403, preflight included, with no CORS header.
		for _, method := range []string{"POST", "OPTIONS"} {
			hdrs := withOrigin(foreign)
			var body []byte = disc
			if method == "OPTIONS" {
				hdrs, body = preflight(foreign), nil
			}
			if code, hd := corsDo(t, method, url, body, hdrs); code != 403 || hd.Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("list, %s Origin %s: HTTP %d %v; want 403, no CORS header", method, foreign, code, hd)
			}
		}
		// "*" sent as an Origin is not a wildcard match for a list.
		if code, _ := corsDo(t, "POST", url, disc, withOrigin("*")); code != 403 {
			t.Fatalf("list, Origin *: HTTP %d, want 403", code)
		}
	})

	t.Run("none", func(t *testing.T) {
		url := startHTTPMCP(t, srv)
		for _, origin := range []string{foreign, "https://allowed.example", "null", "*"} {
			if code, hd := corsDo(t, "POST", url, disc, withOrigin(origin)); code != 403 || hd.Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("no flag, Origin %s: HTTP %d %v; want 403", origin, code, hd)
			}
		}
		if code, hd := corsDo(t, "OPTIONS", url, nil, preflight(foreign)); code != 403 || hd.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("no flag, preflight: HTTP %d %v; want 403", code, hd)
		}
		// Positive control: no Origin is served.
		if code, hd := corsDo(t, "POST", url, disc, withOrigin("")); code != 200 || hd.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("no flag, no Origin: HTTP %d %v; want 200", code, hd)
		}
	})
}

// '*' combined with other entries is refused at startup rather than
// silently meaning "any Origin".
func TestClankMCPHTTPOriginStarMixedRefused(t *testing.T) {
	for _, v := range []string{"*,https://a.example", "https://a.example, *"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := exec.CommandContext(ctx, "node", clankScript, "mcp-http", "--port", "0", "--allow-origin", v).CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(out), "do not combine it with a list") {
			t.Fatalf("--allow-origin %q: err %v, output %q; want E_USAGE refusal", v, err, out)
		}
	}
}
