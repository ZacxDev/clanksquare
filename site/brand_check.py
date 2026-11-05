#!/usr/bin/env python3
"""Checks the brand assets in site/ (stdlib only; no docker, no network).

    python3 site/brand_check.py

- site/icon.svg is the Clanksquare mark: a white `#` on the brand-blue rounded
  tile, its centre cell a solid ink square.
- site/og.svg (the social card's source) draws the same mark.
- site/icon-512.png is 512x512 and site/og.png is 1200x630.
- site/brand.sha256 records the SVGs and the PNGs `make icons` rendered from
  them, and every file still matches it: an SVG edited without re-running
  `make icons` (or a PNG edited by hand) fails here.
- When rsvg-convert is on PATH, each SVG is rendered afresh and must match its
  committed PNG pixel for pixel (og.svg only when its font, DejaVu Sans Mono,
  is installed). Without rsvg-convert that check is reported SKIPPED.
- site/index.html carries the og:image (https://clanksquare.zacx.dev/og.png,
  1200x630), og:title, og:description and twitter:card meta tags.

Prints one "ok"/"not ok" line per check and a count; exits 0 only if every
check passed.
"""
import hashlib
import html.parser
import os
import shutil
import struct
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET
import zlib

SITE = os.path.dirname(os.path.abspath(__file__))
SVG_NS = "{http://www.w3.org/2000/svg}"

ACCENT, INK = "#2f5bd3", "#1d1d1b"
HASH_PATH = "M24 13V51M40 13V51M13 24H51M13 40H51"
OG_URL = "https://clanksquare.zacx.dev/og.png"
# Each committed PNG, the SVG it is rendered from, and its size.
RENDERS = [("icon-512.png", "icon.svg", 512, 512), ("og.png", "og.svg", 1200, 630)]
OG_FONT = "DejaVu Sans Mono"

passed = failed = 0


def check(cond, name):
    global passed, failed
    if cond:
        passed += 1
        print("ok - " + name)
    else:
        failed += 1
        print("not ok - " + name)


def read(name, mode="rb"):
    with open(os.path.join(SITE, name), mode) as f:
        return f.read()


def png_size(data):
    if data[:8] != b"\x89PNG\r\n\x1a\n" or data[12:16] != b"IHDR":
        return None
    return struct.unpack(">II", data[16:24])


def png_pixels(data):
    """Decodes an 8-bit, non-interlaced RGB or RGBA PNG to (w, h, channels, bytes)."""
    w, h = png_size(data)
    depth, ctype, _, _, interlace = data[24:29]
    if depth != 8 or ctype not in (2, 6) or interlace != 0:
        raise ValueError("unsupported PNG: depth %d, color type %d, interlace %d" % (depth, ctype, interlace))
    ch = 3 if ctype == 2 else 4
    idat, pos = b"", 8
    while pos < len(data):
        (n,) = struct.unpack(">I", data[pos:pos + 4])
        kind = data[pos + 4:pos + 8]
        if kind == b"IDAT":
            idat += data[pos + 8:pos + 8 + n]
        pos += 12 + n
    raw, stride, out, prev = zlib.decompress(idat), w * ch, bytearray(), bytearray(w * ch)
    for y in range(h):
        f, line = raw[y * (stride + 1)], bytearray(raw[y * (stride + 1) + 1:(y + 1) * (stride + 1)])
        for i in range(stride):
            a = line[i - ch] if i >= ch else 0
            b = prev[i]
            c = prev[i - ch] if i >= ch else 0
            if f == 1:
                line[i] = (line[i] + a) & 255
            elif f == 2:
                line[i] = (line[i] + b) & 255
            elif f == 3:
                line[i] = (line[i] + (a + b) // 2) & 255
            elif f == 4:
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                line[i] = (line[i] + (a if pa <= pb and pa <= pc else b if pb <= pc else c)) & 255
            elif f != 0:
                raise ValueError("bad PNG filter %d" % f)
        out += line
        prev = line
    return w, h, ch, bytes(out)


def mark_ok(root):
    """The mark's three parts, anywhere under root: tile, hash, centre square."""
    rects = list(root.iter(SVG_NS + "rect"))
    tile = [r for r in rects if r.get("width") == "64" and r.get("height") == "64" and r.get("rx") == "14" and r.get("fill") == ACCENT]
    centre = [r for r in rects if (r.get("x"), r.get("y"), r.get("width"), r.get("height"), r.get("fill")) == ("28", "28", "8", "8", INK)]
    hashes = []
    for g in root.iter(SVG_NS + "g"):
        if g.get("stroke") == "#fff" and g.get("stroke-width") == "8" and g.get("stroke-linecap") == "round":
            hashes += [p for p in g.iter(SVG_NS + "path") if p.get("d") == HASH_PATH]
    return len(tile) == 1 and len(centre) == 1 and len(hashes) == 1


# Every file the checks read; a missing one fails here, not as a traceback.
missing = [n for n in ("icon.svg", "icon-512.png", "og.svg", "og.png", "brand.sha256", "index.html") if not os.path.isfile(os.path.join(SITE, n))]
check(missing == [], "site/ has icon.svg, icon-512.png, og.svg, og.png, brand.sha256 and index.html (missing: %s)" % missing)
if missing:
    print("# %d passed, %d failed" % (passed, failed))
    sys.exit(1)

# ── the mark ────────────────────────────────────────────────────────
icon =ET.fromstring(read("icon.svg"))
check(icon.get("viewBox") == "0 0 64 64" and icon.findtext(SVG_NS + "title") == "Clanksquare", "icon.svg is a 64x64 viewBox titled Clanksquare")
check(mark_ok(icon), "icon.svg is the mark: the %s rounded tile, the white # (%s), the %s centre square" % (ACCENT, HASH_PATH, INK))
check(len(list(icon.iter(SVG_NS + "path"))) == 1 and len(list(icon.iter(SVG_NS + "rect"))) == 2, "icon.svg draws nothing else (1 path, 2 rects)")
og_svg = ET.fromstring(read("og.svg"))
check(og_svg.get("viewBox") == "0 0 1200 630" and mark_ok(og_svg), "og.svg is 1200x630 and draws the same mark")
check(any(t.text == "Clanksquare" for t in og_svg.iter(SVG_NS + "text")), "og.svg sets the name, Clanksquare")

# ── the PNGs and the manifest ───────────────────────────────────────
for png, svg, w, h in RENDERS:
    size = png_size(read(png))
    check(size == (w, h), "%s is a %dx%d PNG (got %r)" % (png, w, h, size))

manifest = {}
for line in read("brand.sha256", "r").splitlines():
    digest, _, path = line.partition("  ")
    manifest[path] = digest
want = sorted("site/" + n for r in RENDERS for n in r[:2])
check(sorted(manifest) == want, "brand.sha256 lists exactly %s (got %s)" % (want, sorted(manifest)))
for path in want:
    got = hashlib.sha256(read(os.path.basename(path))).hexdigest()
    check(manifest.get(path) == got, "%s matches brand.sha256 (stale? run `make icons`)" % path)

rsvg = shutil.which("rsvg-convert")
if rsvg is None:
    print("SKIPPED - fresh render of icon.svg and og.svg: no rsvg-convert on PATH")
else:
    try:
        fonts = subprocess.run(["fc-list", ":", "family"], capture_output=True, text=True).stdout
    except OSError:
        fonts = ""
    with tempfile.TemporaryDirectory() as tmp:
        for png, svg, w, h in RENDERS:
            if svg == "og.svg" and OG_FONT not in fonts:
                print("SKIPPED - fresh render of og.svg: font %s not installed" % OG_FONT)
                continue
            out = os.path.join(tmp, png)
            subprocess.run([rsvg, "-w", str(w), "-h", str(h), os.path.join(SITE, svg), "-o", out], check=True)
            fresh, committed = png_pixels(open(out, "rb").read()), png_pixels(read(png))
            check(fresh == committed, "%s is pixel for pixel a fresh rsvg-convert render of %s (stale? run `make icons`)" % (png, svg))

# ── the page's preview meta tags ────────────────────────────────────
class Meta(html.parser.HTMLParser):
    def __init__(self):
        super().__init__()
        self.tags = {}

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        key = a.get("property") or a.get("name")
        if tag == "meta" and key:
            self.tags.setdefault(key, []).append(a.get("content"))


m = Meta()
m.feed(read("index.html", "r"))
check(m.tags.get("og:image") == [OG_URL], "index.html og:image is %s (got %r)" % (OG_URL, m.tags.get("og:image")))
check(m.tags.get("og:image:width") == ["1200"] and m.tags.get("og:image:height") == ["630"], "index.html og:image:width/height are 1200/630")
check(m.tags.get("og:title") == ["Clanksquare"] and len((m.tags.get("og:description") or [""])[0]) > 20, "index.html has og:title Clanksquare and an og:description")
check(m.tags.get("twitter:card") == ["summary_large_image"], "index.html twitter:card is summary_large_image")

print("# %d passed, %d failed" % (passed, failed))
sys.exit(1 if failed else 0)
