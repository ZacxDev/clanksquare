package e2e

// MCP 2025-11-25 alongside 2026-07-28: the initialize handshake, then
// requests without _meta, on stdio and on the hosted Streamable HTTP server.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const legacyVersion = "2025-11-25"

func legacyInit(version string) map[string]any {
	return map[string]any{"protocolVersion": version, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "e2e-legacy", "version": "0"}}
}

// checkLegacyInit: the initialize answer per 2025-11-25 lifecycle.
func checkLegacyInit(t *testing.T, resp map[string]any, want string) {
	t.Helper()
	res, _ := resp["result"].(map[string]any)
	si, _ := res["serverInfo"].(map[string]any)
	caps, _ := res["capabilities"].(map[string]any)
	if res == nil || res["protocolVersion"] != want || si["name"] != "clanksquare" || si["version"] != "0.1.1" || caps["tools"] == nil {
		t.Fatalf("initialize: want protocolVersion %s, serverInfo and tools capability, got %v", want, resp)
	}
}

// legacyToolText: a 2025-11-25 tools/call result (no resultType required).
func legacyToolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	res, _ := resp["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if res == nil || len(content) != 1 || res["isError"] != false {
		t.Fatalf("tools/call: %v", resp)
	}
	return content[0].(map[string]any)["text"].(string)
}

func TestClankMCPStdioLegacy(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	a.ok("init")
	a.ok("create", "old-room", "--title", "Old room")
	a.ok("post", "#old-room", "for older clients")
	p := startMCP(t, a)

	// Before initialize, a request without _meta is neither era: refused,
	// pointing at both ways in.
	if code, data := rpcErr(t, p.callRaw("tools/list", map[string]any{})); code != -32602 || fmt.Sprint(data["supported"]) != "[2026-07-28 2025-11-25]" {
		t.Fatalf("tools/list before initialize: %v %v", code, data)
	}
	// A ping is allowed before initialize (2025-11-25 lifecycle).
	if r := p.callRaw("ping", nil); r["result"] == nil {
		t.Fatalf("ping: %v", r)
	}
	// Version negotiation: a version the server lacks gets its own.
	checkLegacyInit(t, p.callRaw("initialize", legacyInit("2024-01-01")), legacyVersion)
	checkLegacyInit(t, p.callRaw("initialize", legacyInit(legacyVersion)), legacyVersion)
	p.in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))

	list := p.callRaw("tools/list", map[string]any{})
	var names []string
	for _, x := range list["result"].(map[string]any)["tools"].([]any) {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	if strings.Join(names, " ") != "clank_channels clank_read clank_join clank_post clank_create_channel clank_subscribe clank_digest clank_whoami clank_set_name" {
		t.Fatalf("legacy tools/list: %v", list)
	}
	read := legacyToolText(t, p.callRaw("tools/call", map[string]any{"name": "clank_read", "arguments": map[string]any{"channel": "#old-room"}}))
	if !strings.HasPrefix(read, "<untrusted-channel-content>\n") || !strings.Contains(read, "Z for older clients\n") {
		t.Fatalf("legacy read: %q", read)
	}
	// The 2026-07-28 path still works in the same process.
	if got := strings.Join(toolNames(t, p.call("tools/list", nil)), " "); !strings.HasPrefix(got, "clank_channels clank_read") {
		t.Fatalf("modern tools/list after initialize: %s", got)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.stray) != 0 {
		t.Fatalf("non-response output on stdout: %q", p.stray)
	}
}

// postLegacy posts a 2025-11-25 message: no Mcp-Method/Mcp-Name, and the
// MCP-Protocol-Version header only when pv is set.
func postLegacy(t *testing.T, url string, body []byte, pv string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if pv != "" {
		req.Header.Set("MCP-Protocol-Version", pv)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp, m
}

func TestClankMCPHTTPLegacy(t *testing.T) {
	h := startHub(t)
	a := newAgent(t, h)
	a.ok("init")
	a.ok("create", "old-room", "--title", "Old room")
	a.ok("post", "#old-room", "for older clients")
	url := startHTTPMCP(t, a)

	// initialize carries no MCP-Protocol-Version header; no session is issued.
	resp, m := postLegacy(t, url, rpcBody(1, "initialize", legacyInit(legacyVersion)), "")
	if resp.StatusCode != 200 || resp.Header.Get("Mcp-Session-Id") != "" {
		t.Fatalf("initialize: HTTP %d, session %q", resp.StatusCode, resp.Header.Get("Mcp-Session-Id"))
	}
	checkLegacyInit(t, m, legacyVersion)
	resp, m = postLegacy(t, url, rpcBody(2, "initialize", legacyInit("2024-01-01")), "")
	checkLegacyInit(t, m, legacyVersion)

	// notifications/initialized: 202, no body.
	if resp, _ := postLegacy(t, url, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), legacyVersion); resp.StatusCode != 202 {
		t.Fatalf("notifications/initialized: HTTP %d, want 202", resp.StatusCode)
	}
	// tools/list and tools/call with the negotiated header, and without any
	// header (2025-11-25 transports: a server serves those too).
	for _, pv := range []string{legacyVersion, ""} {
		resp, m = postLegacy(t, url, rpcBody(3, "tools/list", map[string]any{}), pv)
		var names []string
		if res, ok := m["result"].(map[string]any); ok {
			for _, x := range res["tools"].([]any) {
				names = append(names, x.(map[string]any)["name"].(string))
			}
		}
		if resp.StatusCode != 200 || strings.Join(names, " ") != "clank_channels clank_read" {
			t.Fatalf("tools/list (header %q): HTTP %d %v", pv, resp.StatusCode, m)
		}
		resp, m = postLegacy(t, url, rpcBody(4, "tools/call", map[string]any{"name": "clank_read", "arguments": map[string]any{"channel": "#old-room"}}), pv)
		if resp.StatusCode != 200 || !strings.Contains(legacyToolText(t, m), "Z for older clients\n") {
			t.Fatalf("tools/call (header %q): HTTP %d %v", pv, resp.StatusCode, m)
		}
		if resp, m = postLegacy(t, url, rpcBody(5, "ping", nil), pv); resp.StatusCode != 200 || m["result"] == nil {
			t.Fatalf("ping (header %q): HTTP %d %v", pv, resp.StatusCode, m)
		}
	}
	// Write tools stay absent in this era too.
	if _, m = postLegacy(t, url, rpcBody(6, "tools/call", map[string]any{"name": "clank_post", "arguments": map[string]any{"channel": "#old-room", "text": "x"}}), legacyVersion); m["error"] == nil {
		t.Fatalf("legacy clank_post: %v", m)
	}
	// An unsupported header version: 400 (2025-11-25 transports).
	if resp, _ := postLegacy(t, url, rpcBody(7, "tools/list", map[string]any{}), "1900-01-01"); resp.StatusCode != 400 {
		t.Fatalf("unsupported MCP-Protocol-Version: HTTP %d, want 400", resp.StatusCode)
	}
	// Era mix: a 2026-07-28 header without _meta.
	if resp, m := postLegacy(t, url, rpcBody(8, "tools/list", map[string]any{}), mcpVersion); resp.StatusCode != 400 || m["error"].(map[string]any)["code"] != float64(-32602) {
		t.Fatalf("2026-07-28 header without _meta: HTTP %d %v", resp.StatusCode, m)
	}
	// The 2026-07-28 path still works.
	if got := strings.Join(toolNames(t, rpc(t, url, 9, "tools/list", nil)), " "); got != "clank_channels clank_read" {
		t.Fatalf("modern tools/list: %s", got)
	}
}
