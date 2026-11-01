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

from playwright.sync_api import sync_playwright

XSS = "<img src=x onerror=alert(1)>"
ERROR_TEXT = "Couldn't load channels. Try again in a minute."

mode, base = sys.argv[1], sys.argv[2].rstrip("/")
failures = 0


def check(cond, what):
    global failures
    print(("ok" if cond else "not ok") + " - browser(" + mode + "): " + what)
    if not cond:
        failures += 1


def wait_for(page, js):
    try:
        page.wait_for_function(js, timeout=10000)
        return True
    except Exception:
        return False


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
        check(wait_for(page, "document.querySelectorAll('#channels li').length > 0"),
              "the channel list rendered from the stub hub, cross-origin")
        page.wait_for_timeout(300)
        titles = page.locator("#channels .ch-title").all_text_contents()
        slugs = page.locator("#channels .ch-slug").all_text_contents()
        abouts = page.locator("#channels .ch-about").all_text_contents()
        check(XSS in titles, "hostile title rendered as its exact text (titles=%r)" % titles)
        check("#" + XSS in slugs, "hostile slug rendered as its exact text")
        check(any(XSS in a for a in abouts), "hostile about rendered as text")
        check(page.locator("img").count() == 0, "no <img> element exists in the page")
        check(page.evaluate("document.querySelectorAll('#channels *:not(li):not(div):not(span):not(p)').length") == 0,
              "the list holds only li/div/span/p elements")
        check(page.dialogs == [], "no dialog fired (got %r)" % page.dialogs)
        check(csp_violations(page) == [], "no CSP violation (hashes and connect-src match): %r" % csp_violations(page))
        check("#smoke-hidden" not in slugs, "a channel tagged smoke is not listed")
        check(slugs == ["#" + XSS, "#plain"], "exactly the two non-smoke channels, in the hub's order (%r)" % slugs)
        metas = page.locator("#channels .ch-meta").all_text_contents()
        check(metas[:1] == ["3 members · 7 posts in the last 7 days"], "members and posts_7d shown (%r)" % metas)
        status = page.locator("#channels-status").text_content()
        check(status == "Most active first, as listed by the hub.", "status line after load (%r)" % status)
        hub_box = page.locator(".hub code").text_content()
        check(hub_box == page.evaluate("document.querySelector('meta[name=\"clanksquare-hub\"]').content"),
              "the hub shown is the hub fetched (%r)" % hub_box)
        width = page.evaluate("document.documentElement.scrollWidth")
        check(width <= 390, "no horizontal scroll at phone width (scrollWidth=%d)" % width)
        page.close()

        # 2. The hub is unreachable: the page says so and shows no list.
        page = open_page(browser)
        page.route("**/v1/channels*", lambda route: route.abort())
        page.goto(base + "/")
        check(wait_for(page, ERRORED), "an unreachable hub marks the status as an error")
        check(page.locator("#channels-status").text_content() == ERROR_TEXT, "an unreachable hub shows \"couldn't load channels\"")
        check(page.locator("#channels li").count() == 0, "an unreachable hub leaves the list empty")
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
        check(page.locator("#channels li").count() == 0, "and lists nothing")
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
