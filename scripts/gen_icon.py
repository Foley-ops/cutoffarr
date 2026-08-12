#!/usr/bin/env python3
"""gen_icon.py — generates icon.png (256x256) for cutoffarr, reproducibly.

This is a pure-Python PNG writer: it builds the pixel buffer itself and
hand-assembles the PNG chunk structure (signature, IHDR, one IDAT, IEND),
using only the standard library (zlib for DEFLATE compression and the CRC32
each chunk needs, struct for the big-endian chunk-length/CRC fields). No
Pillow, no third-party dependency, no network access, no non-stdlib import
at all — anyone with a stock python3 can run this and get a byte-identical
icon.png back out. Committing the generator alongside the generated file is
the point: the icon is reproducible from source, not merely a binary the
repo happens to also carry.

The design is deliberately simple, abstract, and original: a small bar
chart on a dark background, where every bar that reaches a horizontal
"cutoff" line is capped flush at that line and rendered in a distinct
accent color — nothing grows past it. That is the whole visual metaphor
this project is named for (a value reaching a threshold and stopping),
and it uses no wordmark, no *arr-family iconography, and no third-party
or copyrighted artwork of any kind: every pixel below is one of five flat
RGB fills this script computes itself.

Usage:
    python3 scripts/gen_icon.py [output_path]

output_path defaults to icon.png at the repo root (this script's parent
directory's parent — i.e. running it from anywhere still writes to the
right place).
"""

from __future__ import annotations

import pathlib
import struct
import sys
import zlib

WIDTH = 256
HEIGHT = 256

# --- palette -----------------------------------------------------------
# Flat fills only; no gradients, no photographic content, nothing that
# resembles any existing product's branding or icon.
BACKGROUND = (0x14, 0x16, 0x1D)  # near-black slate
THRESHOLD_LINE = (0xE8, 0xEA, 0xED)  # light gray — the "cutoff" itself
BAR_IN_PROGRESS = (0x3A, 0x4A, 0x63)  # muted slate blue — below cutoff
BAR_AT_CUTOFF = (0x2D, 0xD4, 0xBF)  # bright teal — capped AT the cutoff


def new_canvas(width: int, height: int, fill: tuple[int, int, int]) -> bytearray:
    """A row-major RGB pixel buffer, 3 bytes per pixel, pre-filled."""
    row = bytes(fill) * width
    return bytearray(row * height)


def fill_rect(buf: bytearray, width: int, x0: int, y0: int, x1: int, y1: int, color: tuple[int, int, int]) -> None:
    """Fills the half-open rectangle [x0,x1) x [y0,y1), clipped to the canvas."""
    r, g, b = color
    for y in range(max(y0, 0), y1):
        row_start = y * width * 3
        for x in range(max(x0, 0), x1):
            i = row_start + x * 3
            buf[i] = r
            buf[i + 1] = g
            buf[i + 2] = b


def build_pixels() -> bytearray:
    buf = new_canvas(WIDTH, HEIGHT, BACKGROUND)

    baseline_y = 210  # bars grow upward from this row
    threshold_y = 70  # the horizontal cutoff line's row
    line_thickness = 4

    bar_count = 5
    bar_width = 26
    gap = 14
    # Heights are each bar's distance above the baseline. The last two are
    # deliberately >= (baseline_y - threshold_y) = 140, so they get capped
    # at the line below rather than drawn past it — that clipping IS the
    # motif, not an approximation of it.
    heights = [50, 85, 120, 140, 158]

    total_width = bar_count * bar_width + (bar_count - 1) * gap
    start_x = (WIDTH - total_width) // 2

    for i, h in enumerate(heights):
        x0 = start_x + i * (bar_width + gap)
        x1 = x0 + bar_width
        top_y = baseline_y - h
        at_cutoff = top_y <= threshold_y
        # A bar that reaches or passes the line is drawn only down to the
        # line — never above it — and in the "reached cutoff" color.
        drawn_top_y = max(top_y, threshold_y)
        color = BAR_AT_CUTOFF if at_cutoff else BAR_IN_PROGRESS
        fill_rect(buf, WIDTH, x0, drawn_top_y, x1, baseline_y, color)

    # The threshold line itself, spanning a little wider than the bar
    # cluster so it visibly reads as a line the bars are measured against,
    # not just another bar's edge.
    line_x0 = start_x - 18
    line_x1 = start_x + total_width + 18
    fill_rect(buf, WIDTH, line_x0, threshold_y - line_thickness // 2, line_x1, threshold_y - line_thickness // 2 + line_thickness, THRESHOLD_LINE)

    return buf


# --- PNG encoding --------------------------------------------------------

PNG_SIGNATURE = bytes([137, 80, 78, 71, 13, 10, 26, 10])


def _chunk(chunk_type: bytes, data: bytes) -> bytes:
    length = struct.pack(">I", len(data))
    crc = struct.pack(">I", zlib.crc32(chunk_type + data) & 0xFFFFFFFF)
    return length + chunk_type + data + crc


def encode_png(pixels: bytearray, width: int, height: int) -> bytes:
    # IHDR: width, height, bit depth 8, color type 2 (truecolor RGB), then
    # compression/filter/interlace method, all fixed at 0.
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)

    # Raw image data is scanlines, each prefixed with a filter-type byte;
    # filter 0 (None) is used throughout — the pixel data here has no
    # smooth gradients for a predictive filter to meaningfully compress
    # better, so the simplest correct choice is also a fine one.
    raw = bytearray()
    stride = width * 3
    for y in range(height):
        raw.append(0)
        raw.extend(pixels[y * stride:(y + 1) * stride])
    idat = zlib.compress(bytes(raw), level=9)

    return (
        PNG_SIGNATURE
        + _chunk(b"IHDR", ihdr)
        + _chunk(b"IDAT", idat)
        + _chunk(b"IEND", b"")
    )


def main() -> None:
    out_path = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else pathlib.Path(__file__).resolve().parent.parent / "icon.png"
    pixels = build_pixels()
    png_bytes = encode_png(pixels, WIDTH, HEIGHT)
    out_path.write_bytes(png_bytes)
    print(f"wrote {out_path} ({len(png_bytes)} bytes, {WIDTH}x{HEIGHT})")


if __name__ == "__main__":
    main()
