#!/usr/bin/env python3
"""Render captured terminal output to a standalone SVG.

Why this exists rather than a GIF: the images in the README are generated from
real command output by scripts/screenshots.sh, so they cannot drift from what
the tool actually prints. A recording made by hand rots the first time an
output line changes and nobody notices. This is the same bargain as the demo
script being the integration test.

SVG rather than PNG: it stays crisp at any zoom, it diffs as text, and it
needs no image toolchain. Input is plain text with a leading "$ " marking a
command line; ANSI escapes are stripped.
"""
import html
import re
import sys

ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")

BG, FG = "#11131a", "#c9d1d9"
PROMPT, CMD, DIM = "#7ee787", "#d2a8ff", "#8b949e"
WARN, OK = "#f0883e", "#7ee787"
CH_W, LINE_H, PAD = 8.4, 20, 18
TITLE_H = 34


def colour(line: str) -> str:
    if line.startswith("$ "):
        return CMD
    low = line.lower()
    if low.startswith(("warning", "error", "gnopm: ")) or "⚠" in line:
        return WARN
    if line.startswith("  ok:") or " ok," in line or line.startswith("ok "):
        return OK
    if line.startswith("#") or line.startswith("  "):
        return DIM
    return FG


def render(text: str, title: str) -> str:
    lines = [ANSI.sub("", l).rstrip() for l in text.rstrip("\n").split("\n")]
    width = max([len(l) for l in lines] + [len(title) + 8, 40])
    w = int(width * CH_W + PAD * 2)
    h = int(len(lines) * LINE_H + PAD * 2 + TITLE_H)

    out = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" '
        f'viewBox="0 0 {w} {h}" font-family="ui-monospace,SFMono-Regular,Menlo,monospace" font-size="13">',
        f'<rect width="{w}" height="{h}" rx="10" fill="{BG}"/>',
        f'<rect width="{w}" height="{TITLE_H}" rx="10" fill="#0d1117"/>',
        f'<rect y="{TITLE_H-10}" width="{w}" height="10" fill="#0d1117"/>',
    ]
    for i, c in enumerate(("#ff5f56", "#ffbd2e", "#27c93f")):
        out.append(f'<circle cx="{18+i*18}" cy="{TITLE_H/2}" r="5.5" fill="{c}"/>')
    out.append(
        f'<text x="{w/2}" y="{TITLE_H/2+4.5}" fill="{DIM}" text-anchor="middle" font-size="12">'
        f"{html.escape(title)}</text>"
    )
    y = TITLE_H + PAD + 4
    for line in lines:
        if line.startswith("$ "):
            out.append(f'<text x="{PAD}" y="{y}" fill="{PROMPT}">$</text>')
            out.append(
                f'<text x="{PAD + CH_W*2}" y="{y}" fill="{CMD}">{html.escape(line[2:])}</text>'
            )
        elif line:
            out.append(
                f'<text x="{PAD}" y="{y}" fill="{colour(line)}" xml:space="preserve">'
                f"{html.escape(line)}</text>"
            )
        y += LINE_H
    out.append("</svg>")
    return "\n".join(out) + "\n"


if __name__ == "__main__":
    title = sys.argv[1] if len(sys.argv) > 1 else "gnopm"
    sys.stdout.write(render(sys.stdin.read(), title))
