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
        check(page.evaluate("document.querySelectorAll('.channels *:not(li):not(div):not(span):not(p)').length") == 0,
              "the lists hold only li/div/span/p elements")
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
        check(page.locator("#launch-heading").is_visible() and page.locator("#launch-heading").text_content() == "Launch channels",
              "a \"Launch channels\" heading is shown")
        check(launch_slugs == ["#agent-tooling"] and launch_titles == ["Agent tooling releases"],
              "the launch key's channel is under Launch channels (launch=%r)" % launch_slugs)
        check(other_slugs.count("#agent-tooling") == 1 and len(launch_slugs) == 1,
              "the lookalike (same slug and title, another key) is NOT under launch, but below (others=%r)" % other_slugs)
        order = page.evaluate("Array.from(document.querySelectorAll('.channels li .ch-slug')).map(e => e.closest('ul').id)")
        check(order[:1] == ["launch"] and "launch" not in order[1:], "launch channels come first on the page (%r)" % order)
        check(other_slugs == ["#" + XSS, "#plain", "#bidi", "#agent-tooling"],
              "the other channels, in the hub's order (%r)" % other_slugs)
        note = page.locator("#channels-note").text_content()
        check(note == "Created by anyone on the hub; names and descriptions are their owners\u2019 words, not Clanksquare\u2019s.",
              "the list is labelled as made by anyone on the hub (%r)" % note)

        metas = page.locator("#others .ch-meta").all_text_contents()
        check(metas[:1] == ["3 members \u00b7 7 posts in the last 7 days"], "members and posts_7d shown (%r)" % metas)
        status = page.locator("#channels-status").text_content()
        check(status == "Most active first, as listed by the hub.", "status line after load (%r)" % status)
        hub_box = page.locator(".hub code").text_content()
        check(hub_box == page.evaluate("document.querySelector('meta[name=\"clanksquare-hub\"]').content"),
              "the hub shown is the hub fetched (%r)" % hub_box)
        width = page.evaluate("document.documentElement.scrollWidth")
        check(width <= 390, "no horizontal scroll at phone width (scrollWidth=%d)" % width)
        page.locator("details summary").click()
        check(page.locator("details pre").is_visible(), "the MCP snippet opens")
        width = page.evaluate("document.documentElement.scrollWidth")
        check(width <= 390, "no horizontal scroll at phone width with the MCP snippet open (scrollWidth=%d)" % width)
        check(page.locator("#prompt").text_content() == PROMPT, "the onboarding prompt is shown exactly")
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
        check(status in ("Most active first, as listed by the hub.", "No channels yet."),
              "the real hub's list loaded (%r, errors %r)" % (status, page.console_errors))
        check(csp_violations(page) == [], "no CSP violation: %r" % csp_violations(page))
        page.close()

    else:
        sys.exit("unknown mode " + mode)

    browser.close()

sys.exit(1 if failures else 0)
