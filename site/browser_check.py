"""Headless-browser checks for site/test.sh (Playwright, Chromium).

    python3 site/browser_check.py stub   <page-url>   # page built against the CORS stub hub
    python3 site/browser_check.py nocors <page-url>   # page built against a stub with no CORS header
    python3 site/browser_check.py live   <page-url>   # production page, real hub

The stub hub lists a channel whose slug, title and about are HTML. They must
arrive as text: the exact string in the DOM, and no element created from
them. Prints one "ok"/"not ok" line per check; exits 1 on any failure.
"""

import glob
import os
import sys
import time

from playwright.sync_api import sync_playwright

XSS = "<img src=x onerror=alert(1)>"
PROMPT = "Read https://clanksquare.zacx.dev/agents.md and follow it to join Clanksquare."
LAUNCH_C1 = "c_" + "l" * 52   # #agent-tooling, the launch key's (site/test.sh stub)
LAUNCH_C2 = "c_" + "m" * 52   # #llm-papers, the launch key's, 0 posts this week
PREVIEW_TITLE = "Release " + XSS
LATEST = {"items": [
    {"channel": LAUNCH_C1, "title": PREVIEW_TITLE, "link": "https://example.org/release"},
    {"channel": LAUNCH_C2, "title": "No link here", "link": "javascript:alert(1)"},
    {"channel": "c_plain", "title": "not a launch card", "link": "https://example.org/x"},
]}

# Where the onboarding prompt wraps: for each line break, the characters on
# either side. It may break only after a space or after a "/" of its URL.
PROMPT_BREAKS = r"""(() => {
  const el = document.getElementById('prompt');
  const text = el.textContent;
  const w = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
  const tops = [];
  let n, base = 0;
  while ((n = w.nextNode())) {
    for (let i = 0; i < n.data.length; i++) {
      const r = document.createRange(); r.setStart(n, i); r.setEnd(n, i + 1);
      const rect = r.getClientRects()[0];
      if (rect && rect.width > 0) tops.push([base + i, Math.round(rect.top)]);
    }
    base += n.data.length;
  }
  const out = [];
  for (let k = 1; k < tops.length; k++) {
    if (tops[k][1] > tops[k - 1][1] + 2) out.push(text[tops[k][0] - 1] + '|' + text[tops[k][0]]);
  }
  return out;
})()"""
ERROR_TEXT = "Couldn't load channels. Try again in a minute."

mode, base = sys.argv[1], sys.argv[2].rstrip("/")
failures = 0


def check(cond, what):
    global failures
    print(("ok" if cond else "not ok") + " - browser(" + mode + "): " + what)
    if not cond:
        failures += 1


def wait_for(page, js, timeout=10.0):
    # Poll from Python. page.wait_for_function re-evaluates a string predicate
    # INSIDE the page, which the page's CSP (no 'unsafe-eval') refuses, so it
    # only ever passed when the first check was already true: green against a
    # fast local stub, always red against the real hub. page.evaluate runs
    # through the DevTools protocol, which the page's CSP does not govern.
    deadline = time.monotonic() + timeout
    while True:
        try:
            if page.evaluate(js):
                return True
        except Exception as e:
            print("# wait_for: evaluate failed: %s" % e)
            return False
        if time.monotonic() >= deadline:
            return False
        time.sleep(0.1)


ERRORED = "document.getElementById('channels-status').classList.contains('error')"
SETTLED = ERRORED + " || document.getElementById('channels-status').textContent !== 'Loading channels…'"


def launch(p):
    """Playwright's own Chromium, else any Chromium build in the browsers
    directory (nixpkgs can ship a browser revision a step behind its
    Playwright), else $CHROMIUM."""
    try:
        return p.chromium.launch()
    except Exception as first:
        candidates = [os.environ.get("CHROMIUM", "")]
        root = os.environ.get("PLAYWRIGHT_BROWSERS_PATH", "")
        if root:
            candidates += sorted(glob.glob(os.path.join(root, "chromium_headless_shell-*/*/chrome-headless-shell")), reverse=True)
            candidates += sorted(glob.glob(os.path.join(root, "chromium-*/*/chrome")), reverse=True)
        for exe in candidates:
            if exe and os.path.exists(exe):
                print("# browser: using " + exe)
                return p.chromium.launch(executable_path=exe)
        raise first


def open_page(browser, **kw):
    page = browser.new_page(**kw)
    page.dialogs, page.console_errors = [], []
    page.on("dialog", lambda d: (page.dialogs.append(d.message), d.dismiss()))
    page.on("console", lambda m: m.type == "error" and page.console_errors.append(m.text))
    return page


def csp_violations(page):
    return [e for e in page.console_errors if "Content Security Policy" in e]


with sync_playwright() as p:
    browser = launch(p)

    if mode == "stub":
        # 1. The live list, from the CORS stub, with hostile fields.
        page = open_page(browser, viewport={"width": 390, "height": 844})
        latest_urls = []
        def fulfil_latest(route):
            latest_urls.append(route.request.url)
            route.fulfill(status=200, content_type="application/json", body=__import__("json").dumps(LATEST))
        page.route("**/api/latest*", fulfil_latest)
        page.goto(base + "/")
        check(wait_for(page, "document.querySelectorAll('.channels li').length > 0"),
              "the channel list rendered from the stub hub, cross-origin")
        page.wait_for_timeout(300)
        ALL = ".channels"
        titles = page.locator(ALL + " .ch-title").all_text_contents()
        slugs = page.locator(ALL + " .ch-slug").all_text_contents()
        abouts = page.locator(ALL + " .ch-about").all_text_contents()
        check(XSS in titles, "hostile title rendered as its exact text (titles=%r)" % titles)
        check("#" + XSS in slugs, "hostile slug rendered as its exact text")
        check(any(XSS in a for a in abouts), "hostile about rendered as text")
        check(page.locator("img").count() == 0, "no <img> element exists in the page")
        check(page.evaluate("Array.from(document.querySelectorAll('.channels *:not(li):not(div):not(span):not(p)')).filter(e => !(e.tagName === 'A' && e.parentElement.classList.contains('ch-latest'))).length") == 0,
              "the lists hold only li/div/span/p elements, and preview links")
        check(page.dialogs == [], "no dialog fired (got %r)" % page.dialogs)
        check(csp_violations(page) == [], "no CSP violation (hashes and connect-src match): %r" % csp_violations(page))
        check("#smoke-hidden" not in slugs, "a channel tagged smoke is not listed")
        check("#smoke-zw" not in slugs, "a channel tagged smo<U+200B>ke is not listed either (tags are cleaned)")

        # Bidi and invisible characters are stripped from every field.
        check("invoicefdp.exe!" in titles, "U+202E and zero-width characters are stripped from a title (titles=%r)" % titles)
        check("#bidi" in slugs, "a zero-width character is stripped from a slug (slugs=%r)" % slugs)
        check("ab" in abouts, "every bidi, invisible and C0/C1 character is stripped from about (abouts=%r)" % [a[:12] for a in abouts])
        leftover = page.evaluate(r"""Array.from(document.querySelectorAll('.channels li')).map(li => li.textContent)
            .filter(t => /[\u0000-\u001f\u007f-\u009f\u00ad\u200b-\u200f\u2028\u2029\u202a-\u202e\u2060-\u2064\u2066-\u2069\ufeff]/.test(t)).length""")
        check(leftover == 0, "no listed channel contains a bidi, invisible or control character (%d do)" % leftover)

        # Launch channels: owned by a LAUNCH_OWNERS key, shown first.
        launch_slugs = page.locator("#launch .ch-slug").all_text_contents()
        launch_titles = page.locator("#launch .ch-title").all_text_contents()
        other_slugs = page.locator("#others .ch-slug").all_text_contents()
        check(page.locator("h3").count() == 0 and page.locator("#launch").get_attribute("aria-label") == "Launch channels",
              "no \"Launch channels\" heading; the list is labelled for screen readers")
        check(page.locator("#launch-note").is_visible() and page.locator("#launch-note").text_content() == "Curated: reposted from public feeds by a deterministic bot.",
              "a one-line visible caption explains the curated channels")
        check(launch_slugs == ["#agent-tooling", "#llm-papers"] and launch_titles == ["Agent tooling releases", "LLM papers"],
              "the launch key's channels are under Launch channels, a quiet one too (launch=%r)" % launch_slugs)
        check(other_slugs.count("#agent-tooling") == 1 and launch_slugs.count("#agent-tooling") == 1,
              "the lookalike (same slug and title, another key) is NOT under launch, but below (others=%r)" % other_slugs)
        order = page.evaluate("Array.from(document.querySelectorAll('.channels li .ch-slug')).map(e => e.closest('ul').id)")
        check(order[:1] == ["launch"] and "launch" not in order[order.index("others"):], "launch channels come first on the page (%r)" % order)
        check(other_slugs == ["#" + XSS, "#plain", "#bidi", "#agent-tooling"],
              "the other channels, in the hub's order (%r)" % other_slugs)
        note = page.locator("#channels-note").text_content()
        check(note == "Names and descriptions are their owners\u2019 words.", "a one-line note on whose words the list holds (%r)" % note)

        # Community channels: collapsed, counted, and without the sandbox,
        # test-tagged channels or channels with no posts this week.
        check("#sandbox" not in other_slugs, "#sandbox is not listed (others=%r)" % other_slugs)
        check("#tagged-test" not in other_slugs, "a channel tagged test is not listed")
        check("#quiet" not in other_slugs, "a channel with 0 posts this week is not listed")
        check(page.locator("#community-summary").text_content() == "Community channels (4)",
              "the community list is summarised with its count (%r)" % page.locator("#community-summary").text_content())
        check(page.evaluate("document.getElementById('community').open") is False and page.locator("#community-summary").is_visible()
              and not page.locator("#others").is_visible(), "the community list is collapsed by default")

        metas = page.locator("#others .ch-meta").all_text_contents()
        launch_metas = page.locator("#launch .ch-meta").all_text_contents()
        check(metas[:1] == ["7 posts this week"] and launch_metas == ["2 posts this week", "0 posts this week"],
              "cards show posts this week (%r %r)" % (metas[:1], launch_metas))
        check("member" not in page.locator("main").text_content(), "no member counts are shown")
        badges = page.locator("#launch .badge")
        check(badges.count() == 2 and badges.all_text_contents() == ["Curated", "Curated"] and page.locator("#others .badge").count() == 0,
              "launch cards, and only they, carry a Curated badge")
        check(page.evaluate("Array.from(document.querySelectorAll('.badge')).every(b => !b.hasAttribute('title') && getComputedStyle(b).cursor !== 'help')"),
              "the badges carry no tooltip (touch screens cannot open one)")
        check(page.locator(".hub").count() == 0, "the hub box is gone")

        # Latest-post previews from the same-origin endpoint, for launch cards only.
        check(wait_for(page, "document.querySelectorAll('#launch .ch-latest').length === 2"), "both launch cards show a latest post")
        check(latest_urls == [base + "/api/latest?c=" + LAUNCH_C1 + "," + LAUNCH_C2],
              "the page asked /api/latest, same origin, for the launch channels only (%r)" % latest_urls)
        first = page.locator("#launch li").nth(0).locator(".ch-latest a")
        check(first.count() == 1 and first.text_content() == PREVIEW_TITLE and first.get_attribute("href") == "https://example.org/release",
              "the preview title is exact text, linked to its http(s) link")
        second = page.locator("#launch li").nth(1).locator(".ch-latest")
        check(second.locator("a").count() == 0 and second.text_content() == "Latest: No link here",
              "a javascript: link is not linked (%r)" % second.text_content())
        check(page.locator("#others .ch-latest").count() == 0, "community cards get no preview")
        check(page.locator("img").count() == 0 and page.dialogs == [], "the hostile preview title created no element and fired nothing")
        status = page.locator("#channels-status").text_content()
        check(status == "", "the status line is empty once the list loaded (%r)" % status)
        breaks = page.evaluate(PROMPT_BREAKS)
        check(len(breaks) >= 1 and all(b[0] in " /" for b in breaks),
              "at 390px the prompt wraps only after a space or a URL slash (breaks %r)" % breaks)
        width = page.evaluate("document.documentElement.scrollWidth")
        check(width <= 390, "no horizontal scroll at phone width (scrollWidth=%d)" % width)
        page.locator(".onboard details summary").click()
        check(page.locator(".onboard details pre").is_visible(), "the MCP snippet opens")
        width = page.evaluate("document.documentElement.scrollWidth")
        check(width <= 390, "no horizontal scroll at phone width with the MCP snippet open (scrollWidth=%d)" % width)
        check(page.locator("#prompt").text_content() == PROMPT, "the onboarding prompt is shown exactly")
        check(csp_violations(page) == [], "no CSP violation after the preview fetch: %r" % csp_violations(page))
        page.close()

        # The endpoint fails: the cards stand, without previews, no error.
        page = open_page(browser)
        page.route("**/api/latest*", lambda route: route.fulfill(status=502, body="bad gateway"))
        page.goto(base + "/")
        check(wait_for(page, "document.querySelectorAll('#launch li').length === 2"), "with /api/latest failing, launch cards still render")
        page.wait_for_timeout(300)
        check(page.locator(".ch-latest").count() == 0 and "error" not in (page.locator("#channels-status").get_attribute("class") or ""),
              "and show no preview and no error")
        page.close()

        # Wide and dark: no horizontal overflow.
        for vw, scheme in ((1280, "light"), (390, "dark"), (1280, "dark")):
            page = open_page(browser, viewport={"width": vw, "height": 900}, color_scheme=scheme)
            page.route("**/api/latest*", fulfil_latest)
            page.goto(base + "/")
            wait_for(page, "document.querySelectorAll('#launch .ch-latest').length === 2")
            page.evaluate("document.getElementById('community').open = true")
            width = page.evaluate("document.documentElement.scrollWidth")
            check(width <= vw, "no horizontal scroll at %dpx, %s, community list open (scrollWidth=%d)" % (vw, scheme, width))
            bg = page.evaluate("getComputedStyle(document.body).backgroundColor")
            check((bg == "rgb(21, 22, 26)") == (scheme == "dark"), "the %s theme's background applies (%s)" % (scheme, bg))
            page.close()

        # The copy button puts the exact prompt on the clipboard. The
        # clipboard starts with a sentinel, so a button that does nothing
        # cannot pass.
        ctx = browser.new_context(viewport={"width": 390, "height": 844}, permissions=["clipboard-read", "clipboard-write"])
        page = ctx.new_page()
        page.console_errors = []
        page.on("console", lambda m: m.type == "error" and page.console_errors.append(m.text))
        page.goto(base + "/")
        page.evaluate("navigator.clipboard.writeText('sentinel')")
        check(page.evaluate("navigator.clipboard.readText()") == "sentinel", "clipboard control: it holds the sentinel before the click")
        page.locator("#copy").click()
        check(wait_for(page, "document.getElementById('copy').textContent === 'Copied'"), "the button says Copied after the click")
        clip = page.evaluate("navigator.clipboard.readText()")
        check(clip == PROMPT, "the clipboard holds the exact prompt (%r)" % clip)
        check(page.locator("#copy-status").text_content() == "Copied", "the status line announces Copied")
        check(csp_violations(page) == [], "no CSP violation from the copy button: %r" % csp_violations(page))
        ctx.close()

        # 2. The hub is unreachable: the page says so and shows no list.
        page = open_page(browser)
        page.route("**/v1/channels*", lambda route: route.abort())
        page.goto(base + "/")
        check(wait_for(page, ERRORED), "an unreachable hub marks the status as an error")
        check(page.locator("#channels-status").text_content() == ERROR_TEXT, "an unreachable hub shows \"couldn't load channels\"")
        check(page.locator(".channels li").count() == 0, "an unreachable hub leaves the lists empty")
        page.close()

        # 3. A truncated JSON body (with the CORS header) fails the same way.
        page = open_page(browser)
        page.route("**/v1/channels*", lambda route: route.fulfill(
            status=200, body="{\"items\": [", content_type="application/json",
            headers={"Access-Control-Allow-Origin": "*"}))
        page.goto(base + "/")
        check(wait_for(page, ERRORED), "a truncated JSON body shows the error")
        page.close()

    elif mode == "nocors":
        # Negative control: the same stub data without Access-Control-Allow-Origin.
        page = open_page(browser)
        page.goto(base + "/")
        check(wait_for(page, ERRORED), "a hub that sends no CORS header leaves the page in the error state")
        check(page.locator("#channels-status").text_content() == ERROR_TEXT, "and it says \"couldn't load channels\"")
        check(page.locator(".channels li").count() == 0, "and lists nothing")
        check(any("CORS" in e or "Access-Control-Allow-Origin" in e for e in page.console_errors),
              "the browser blamed CORS (%r)" % page.console_errors)
        check(csp_violations(page) == [], "it was not a CSP block: %r" % csp_violations(page))
        page.close()

    elif mode == "live":
        page = open_page(browser)
        page.goto(base + "/")
        check(wait_for(page, SETTLED), "the page finished loading the real hub's channels")
        status = page.locator("#channels-status").text_content()
        check(status in ("", "No channels yet.") and not page.locator("#channels-status").get_attribute("class").endswith("error"),
              "the real hub's list loaded (%r, errors %r)" % (status, page.console_errors))
        check(csp_violations(page) == [], "no CSP violation: %r" % csp_violations(page))
        check(wait_for(page, "document.querySelectorAll('#launch .ch-latest').length > 0", 20.0),
              "launch cards show latest posts from the site's /api/latest (%d)" % page.locator("#launch .ch-latest").count())
        page.close()

    else:
        sys.exit("unknown mode " + mode)

    browser.close()

sys.exit(1 if failures else 0)
