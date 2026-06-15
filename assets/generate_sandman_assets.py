#!/usr/bin/env python3
"""Generate Sandman brand SVG assets.

S-mark = rounded circuit-trace monogram: 5 parallel lanes, each lane one
<path> (fill none, round caps). Centerline construction per lane offset t:

    top run (y = Y1+t)  : terminal at right XR, leftward to XA
    left 180-degree bend: center (XA, (Y1+Y2)/2), radius B - t
    middle run (Y2 - t) : rightward XA -> XB
    right 180-degree bend: center (XB, (Y2+Y3)/2), radius C + t
    bottom run (Y3 + t) : leftward to terminal XL

Sand lives inside lanes only: solid stroke through the body, dasharray
dots (zero-length dashes, round caps) at the terminals, plus two extra
on-rail packet dots beyond each terminal. No loose particle cloud.
"""
import math

GOLD = "#D8B26E"
GOLD_HI = "#E9C98A"
GOLD_DARK = "#BE8E45"
INK = "#F2EEE3"
BG_DARK = "#081020"
BG_LIGHT = "#F5EFE2"

# --- lane geometry -----------------------------------------------------
SW = 10.0          # lane stroke width
GAP = 8.0          # negative space between lanes (slightly < SW)
PITCH = SW + GAP   # 18: lane center-to-center
N = 5
Y1, Y2, Y3 = 50.0, 160.0, 270.0   # run centerlines (middle lane) — tall S
B = C = (Y2 - Y1) / 2.0           # bend radii of the centerline = 55
XA, XB = 115.0, 205.0             # bend axis x positions
XR, XL = 245.0, 75.0              # top-right / bottom-left terminals
G = 19.0                          # terminal dot pitch along the rail
VIEW = "0 0 320 320"

OFFSETS = [(i - (N - 1) / 2) * PITCH for i in range(N)]  # -36..36


def lane(t):
    """Path d + exact length for the lane offset t from the centerline."""
    rb, rc = B - t, C + t
    y1, y2, y3 = Y1 + t, Y2 - t, Y3 + t
    m1, m2 = (Y1 + Y2) / 2, (Y2 + Y3) / 2
    d = (f"M {XR:g} {y1:g} L {XA:g} {y1:g} "
         f"A {rb:g} {rb:g} 0 0 0 {XA - rb:g} {m1:g} "
         f"A {rb:g} {rb:g} 0 0 0 {XA:g} {y2:g} "
         f"L {XB:g} {y2:g} "
         f"A {rc:g} {rc:g} 0 0 1 {XB + rc:g} {m2:g} "
         f"A {rc:g} {rc:g} 0 0 1 {XB:g} {y3:g} "
         f"L {XL:g} {y3:g}")
    L = (XR - XA) + math.pi * rb + (XB - XA) + math.pi * rc + (XB - XL)
    return d, L


def lane_dash(L, ndots=3):
    """Evenly spaced terminal dots dissolving into a solid body, both ends."""
    head = []
    for _ in range(ndots):
        head += [0, G]
    head += [9, G]                       # short packet before the body
    tail = [G, 9] + [G, 0] * ndots       # mirrored at the far terminal
    mid = L - sum(head) - sum(tail)
    arr = head + [mid] + tail + [4000]
    return " ".join(f"{v:.2f}" for v in arr)


def mark_group(color, ndots=3, packets=True):
    parts = [f'<g fill="none" stroke="{color}" stroke-width="{SW:g}" '
             f'stroke-linecap="round" stroke-linejoin="round">']
    for t in OFFSETS:
        d, L = lane(t)
        parts.append(f'<path d="{d}" stroke-dasharray="{lane_dash(L, ndots)}"/>')
    parts.append("</g>")
    if packets:
        dots = []
        for t in OFFSETS:
            y1, y3 = Y1 + t, Y3 + t
            dots.append(f'<circle cx="{XR + G:g}" cy="{y1:g}" r="3.2" '
                        f'fill="{color}" opacity="0.65"/>')
            dots.append(f'<circle cx="{XL - G:g}" cy="{y3:g}" r="3.2" '
                        f'fill="{color}" opacity="0.65"/>')
        parts.append("<g>" + "".join(dots) + "</g>")
    return "\n".join(parts)


def svg(viewbox, body, w=None, h=None):
    size = f' width="{w}" height="{h}"' if w else ""
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="{viewbox}"{size}>'
            f"\n{body}\n</svg>\n")


def write(name, content):
    with open(name, "w") as f:
        f.write(content)
    print("wrote", name)


MONO = "'Space Mono','Courier New',monospace"


def wordmark(x, y, size, spacing, fill=INK, anchor="start"):
    return (f'<text x="{x}" y="{y}" font-family="{MONO}" font-weight="700" '
            f'font-size="{size}" letter-spacing="{spacing}" fill="{fill}" '
            f'text-anchor="{anchor}">SANDMAN</text>')


def tagline(x, y, size, fill=GOLD_HI, anchor="start"):
    return (f'<text x="{x}" y="{y}" font-family="{MONO}" font-size="{size}" '
            f'fill="{fill}" text-anchor="{anchor}">Sleep while your agents code.</text>')


GREEN = "#22C55E"
CYAN = "#4CC9F0"
PURPLE = "#A78BFA"
SLATE = "#94A3B8"
PANEL = "#0B1228"
PANEL_2 = "#0D1530"
BORDER = "#222B45"


def elbow(sx, sy, vx, ey, ex, r=12):
    """Rounded circuit elbow: horizontal -> vertical -> horizontal."""
    if abs(ey - sy) < 2:
        return f"M {sx} {sy} L {ex} {ey}"
    r = min(r, abs(ey - sy) / 2)
    s = 1 if ey > sy else -1
    return (f"M {sx} {sy} L {vx - r:g} {sy} Q {vx} {sy} {vx} {sy + s * r:g} "
            f"L {vx} {ey - s * r:g} Q {vx} {ey} {vx + r:g} {ey} L {ex} {ey}")


def wf_icon(kind):
    """24x24 workflow glyphs, drawn around (0,0)."""
    s = 'fill="none" stroke="#C9D2E4" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"'
    if kind == "issues":
        return f'<circle r="7" {s}/><circle r="2" fill="#C9D2E4"/>'
    if kind == "sandbox":
        return (f'<path d="M 0 -8 L 7 -4 L 7 4 L 0 8 L -7 4 L -7 -4 Z" {s}/>'
                f'<path d="M -7 -4 L 0 0 L 7 -4 M 0 0 L 0 8" {s}/>')
    if kind == "agents":
        return (f'<rect x="-7" y="-5" width="14" height="11" rx="3" {s}/>'
                f'<path d="M 0 -5 L 0 -9" {s}/><circle cy="-9.5" r="1.3" fill="#C9D2E4"/>'
                f'<circle cx="-3" cy="0.5" r="1.4" fill="#C9D2E4"/>'
                f'<circle cx="3" cy="0.5" r="1.4" fill="#C9D2E4"/>')
    if kind == "code":
        return (f'<path d="M -4 -5 L -8 0 L -4 5 M 4 -5 L 8 0 L 4 5 M 1.5 -7 L -1.5 7" {s}/>')
    if kind == "review":
        return (f'<path d="M -7 -7 H 7 Q 9 -7 9 -5 V 3 Q 9 5 7 5 H 0 L -4 9 V 5 H -7 '
                f'Q -9 5 -9 3 V -5 Q -9 -7 -7 -7 Z" {s}/>'
                f'<path d="M -4 -2 H 4 M -4 1.5 H 1" {s}/>')
    if kind == "pr":
        return (f'<circle cx="-6" cy="-6" r="2.6" {s}/><circle cx="-6" cy="6" r="2.6" {s}/>'
                f'<circle cx="6" cy="6" r="2.6" {s}/>'
                f'<path d="M -6 -3.4 V 3.4 M -3.4 -6 H 0 Q 6 -6 6 0 V 3.4" {s}/>')
    return ""


def banner():
    import random
    rng = random.Random(11)
    e = []
    # panel
    e.append(f'<rect width="1520" height="420" rx="18" fill="{PANEL}"/>')
    e.append(f'<rect x="1.5" y="1.5" width="1517" height="417" rx="16.5" '
             f'fill="none" stroke="{BORDER}" stroke-width="3"/>')
    # stars (upper right night sky)
    stars = []
    for _ in range(26):
        x = rng.uniform(800, 1480)
        y = rng.uniform(28, 150)
        stars.append(f'<circle cx="{x:.0f}" cy="{y:.0f}" r="{rng.uniform(0.8, 1.7):.1f}" '
                     f'fill="#E9EDF6" opacity="{rng.uniform(0.25, 0.75):.2f}"/>')
    e.append("<g>" + "".join(stars) + "</g>")
    # crescent moon
    e.append(f'<circle cx="1118" cy="76" r="24" fill="{GOLD_HI}"/>'
             f'<circle cx="1130" cy="68" r="22" fill="{PANEL}"/>')
    # mountain silhouettes
    e.append(f'<path d="M 830 290 L 950 195 L 1040 252 L 1140 178 L 1255 262 '
             f'L 1350 215 L 1455 280 L 1518 252 L 1518 330 L 830 330 Z" '
             f'fill="#0E1734" opacity="0.85"/>')
    e.append(f'<path d="M 980 310 L 1090 240 L 1190 300 L 1300 248 L 1410 310 '
             f'L 1518 275 L 1518 345 L 980 345 Z" fill="#0A0F22"/>')
    # S mark, left
    e.append(f'<g transform="translate(20 28) scale(1.09)">{mark_group(GOLD)}</g>')
    # title block
    e.append(wordmark(390, 118, 64, 13))
    e.append(tagline(392, 162, 26))
    e.append(f'<text x="392" y="200" font-family="{MONO}" font-size="19" fill="{SLATE}">'
             f'AFK coding agents orchestration</text>')
    e.append(f'<text x="392" y="226" font-family="{MONO}" font-size="19" fill="{SLATE}">'
             f'in isolated sandboxes.</text>')
    # terminal command box
    e.append(f'<rect x="390" y="252" width="330" height="50" rx="9" fill="{PANEL_2}" '
             f'stroke="{BORDER}" stroke-width="1.5"/>')
    e.append(f'<text x="410" y="284" font-family="{MONO}" font-size="21" fill="{GREEN}">'
             f'$ sandman run 42 43</text>')
    e.append(f'<text x="700" y="283" font-family="{MONO}" font-size="16" fill="{SLATE}">&#8942;</text>')
    # status cards + connectors
    cards = [("#42", "completed", GREEN), ("#43", "completed", GREEN),
             ("#44", "running", CYAN), ("#45", "queued", PURPLE)]
    line_cols = [GOLD_HI, GOLD_HI, CYAN, PURPLE]
    for i, (num, status, col) in enumerate(cards):
        cy = 168 + i * 46
        sy = 250 + i * 12
        vx = 1090 + i * 24
        e.append(f'<path d="{elbow(1020, sy, vx, cy, 1212)}" fill="none" '
                 f'stroke="{line_cols[i]}" stroke-width="2.5" opacity="0.85"/>')
        e.append(f'<circle cx="1218" cy="{cy}" r="4.5" fill="{line_cols[i]}"/>')
        e.append(f'<path d="M 1228 {cy - 5} L 1237 {cy} L 1228 {cy + 5} Z" fill="{line_cols[i]}"/>')
        e.append(f'<rect x="1248" y="{cy - 19}" width="228" height="38" rx="8" '
                 f'fill="{PANEL_2}" stroke="{BORDER}" stroke-width="1.5"/>')
        e.append(f'<text x="1266" y="{cy + 6}" font-family="{MONO}" font-size="18" '
                 f'fill="{SLATE}">{num}</text>')
        e.append(f'<text x="1320" y="{cy + 6}" font-family="{MONO}" font-size="18" '
                 f'fill="{col}">+ {status}</text>')
    # workflow row
    steps = ["Issues", "Sandbox", "Agents", "Code", "Review", "PR"]
    kinds = ["issues", "sandbox", "agents", "code", "review", "pr"]
    label_w = {s: 8.6 * len(s) for s in steps}
    x = 400
    y = 352
    centers = []
    for step, kind in zip(steps, kinds):
        e.append(f'<circle cx="{x}" cy="{y}" r="15" fill="none" stroke="#2B3554" stroke-width="1.5"/>')
        e.append(f'<g transform="translate({x} {y}) scale(0.9)">{wf_icon(kind)}</g>')
        lx = x + 24
        e.append(f'<text x="{lx}" y="{y + 5}" font-family="{MONO}" font-size="15" '
                 f'fill="#C9D2E4">{step}</text>')
        centers.append(x)
        ax = lx + label_w[step] + 14
        if step != "PR":
            e.append(f'<path d="M {ax} {y} h 16 m -5 -4 l 5 4 l -5 4" fill="none" '
                     f'stroke="{SLATE}" stroke-width="1.6" stroke-linecap="round"/>')
        x = ax + 46
    row_end = x - 46 + label_w["PR"] + 26
    # dashed "Complete Workflow" bracket
    by = 392
    mid = (400 + row_end) / 2
    e.append(f'<path d="M 400 {by} H {mid - 105}" stroke="#3A4663" stroke-width="1.5" '
             f'stroke-dasharray="6 6" fill="none"/>')
    e.append(f'<path d="M {mid + 105} {by} H {row_end}" stroke="#3A4663" stroke-width="1.5" '
             f'stroke-dasharray="6 6" fill="none"/>')
    e.append(f'<text x="{mid}" y="{by + 5}" font-family="{MONO}" font-size="15" '
             f'fill="{SLATE}" text-anchor="middle">Complete Workflow</text>')
    return svg("0 0 1520 420", "\n".join(e))


def main():
    # 1. primary mark (transparent bg, for dark surfaces)
    write("sandman-mark.svg", svg(VIEW, mark_group(GOLD)))

    # 2. monochrome mark
    write("sandman-mark-mono.svg", svg(VIEW, mark_group("#FFFFFF")))

    # 3. horizontal lockup
    body = (f'<g transform="translate(10 5) scale(0.97)">{mark_group(GOLD)}</g>\n'
            + wordmark(350, 150, 78, 16)
            + tagline(352, 200, 26))
    write("sandman-logo-horizontal.svg", svg("0 0 1000 320", body))

    # 4. stacked lockup
    body = (f'<g transform="translate(90 0)">{mark_group(GOLD)}</g>\n'
            + wordmark(250, 400, 56, 12, anchor="middle")
            + tagline(250, 440, 20, anchor="middle"))
    write("sandman-logo-stacked.svg", svg("0 0 500 480", body))

    # 5. app icon (dark)
    body = (f'<rect width="512" height="512" rx="116" fill="{BG_DARK}"/>'
            f'<rect x="3" y="3" width="506" height="506" rx="113" fill="none" '
            f'stroke="#222B45" stroke-width="2"/>'
            f'<g transform="translate(56 56) scale(1.25)">{mark_group(GOLD)}</g>')
    write("sandman-app-icon.svg", svg("0 0 512 512", body, 512, 512))

    # 6. app icon (light)
    body = (f'<rect width="512" height="512" rx="116" fill="{BG_LIGHT}"/>'
            f'<g transform="translate(56 56) scale(1.25)">{mark_group(GOLD_DARK)}</g>')
    write("sandman-app-icon-light.svg", svg("0 0 512 512", body, 512, 512))

    # 7-8. favicons: one terminal dot, no packets -> reads at 32px
    fav = mark_group(GOLD, ndots=1, packets=False)
    body = (f'<rect width="512" height="512" rx="96" fill="{BG_DARK}"/>'
            f'<g transform="translate(56 56) scale(1.25)">{fav}</g>')
    write("sandman-favicon.svg", svg("0 0 512 512", body))

    fav_l = mark_group(GOLD_DARK, ndots=1, packets=False)
    body = (f'<rect width="512" height="512" rx="96" fill="{BG_LIGHT}"/>'
            f'<g transform="translate(56 56) scale(1.25)">{fav_l}</g>')
    write("sandman-favicon-light.svg", svg("0 0 512 512", body))

    # 9. README banner
    write("sandman-banner.svg", banner())


if __name__ == "__main__":
    main()
