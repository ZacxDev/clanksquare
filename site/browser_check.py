"""Headless-browser checks for site/test.sh (Playwright, Chromium).

    python3 site/browser_check.py http://127.0.0.1:PORT

The stub hub behind the site lists a channel whose title, slug and about are
HTML. They must arrive as text: the exact string in the DOM, and no element
created from them. Prints one "ok"/"not ok" line per check; exits 1 on any
failure.
"""

import glob
import os
import sys

from playwright.sync_api import sync_playwright

XSS = "<img src=x onerror=alert(1)>"

base = sys.argv[1].rstrip("/")
failures = 0


def wait_error(page):
    try:
        page.wait_for_function("document.getElementById('channels-status').classList.contains('error')", timeout=10000)
        return True
    except Exception:
        return False


def check(cond, what):
    global failures
    print(("ok" if cond else "not ok") + " - browser: " + what)
    if not cond:
        failures += 1


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


with sync_playwright() as p:
    browser = launch(p)

    # 1. The live list, from the stub, with hostile fields.
    page = browser.new_page(viewport={"width": 390, "height": 844})
    dialogs, console_errors = [], []
    page.on("dialog", lambda d: (dialogs.append(d.message), d.dismiss()))
    page.on("console", lambda m: m.type == "error" and console_errors.append(m.text))
    page.goto(base + "/")
    try:
        page.wait_for_selector("#channels li", timeout=10000)
        loaded = True
    except Exception:
        loaded = False
    check(loaded, "the channel list rendered from the stub")
    page.wait_for_timeout(500)

    titles = page.locator("#channels .ch-title").all_text_contents()
    slugs = page.locator("#channels .ch-slug").all_text_contents()
    abouts = page.locator("#channels .ch-about").all_text_contents()
    check(XSS in titles, "hostile title rendered as its exact text (titles=%r)" % titles)
    check("#" + XSS in slugs, "hostile slug rendered as its exact text")
    check(any(XSS in a for a in abouts), "hostile about rendered as text")
    check(page.locator("img").count() == 0, "no <img> element exists in the page")
    check(page.evaluate("document.querySelectorAll('#channels *:not(li):not(div):not(span):not(p)').length") == 0,
          "the list holds only li/div/span/p elements")
    check(dialogs == [], "no dialog fired (got %r)" % dialogs)
    check(not any("Content Security Policy" in e for e in console_errors),
          "no CSP violation (inline script and style hashes match): %r" % console_errors)
    check("#smoke-hidden" not in slugs, "a channel tagged smoke is not listed")
    metas = page.locator("#channels .ch-meta").all_text_contents()
    meta = metas[0] if metas else None
    check(meta == "3 members · 7 posts in the last 7 days", "members and posts_7d shown (%r)" % meta)
    status = page.locator("#channels-status").text_content()
    check(status == "Most active first, as listed by the hub.", "status line after load (%r)" % status)
    width = page.evaluate("document.documentElement.scrollWidth")
    check(width <= 390, "no horizontal scroll at phone width (scrollWidth=%d)" % width)
    page.close()

    # 2. The fetch fails: the page says so and shows no list.
    page = browser.new_page()
    page.route("**/api/channels*", lambda route: route.fulfill(status=502, body="bad gateway"))
    page.goto(base + "/")
    check(wait_error(page), "a 502 from /api/channels marks the status as an error")
    check(page.locator("#channels-status").text_content() == "Couldn't load channels. Try again in a minute.",
          "a failed fetch shows \"couldn't load channels\"")
    check(page.locator("#channels li").count() == 0, "a failed fetch leaves the list empty")
    page.close()

    # 3. A 200 that is not JSON fails the same way.
    page = browser.new_page()
    page.route("**/api/channels*", lambda route: route.fulfill(status=200, body="{\"items\": [", content_type="application/json"))
    page.goto(base + "/")
    check(wait_error(page), "a truncated JSON body (as the size cap produces) shows the error")
    page.close()

    browser.close()

sys.exit(1 if failures else 0)
