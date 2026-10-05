#!/usr/bin/env python3
"""生成 TanGIS 应用图标（.icns）。

用法：
    python3 deploy/desktop/make-icon.py <输出路径.icns>

设计：深青蓝渐变圆角方块 + 白色"瓦片金字塔"字形（三层错位菱形，呼应瓦片金字塔）。
几何遵循 macOS Big Sur 之后的规范（内容区 824/1024、圆角 185/824），不做出血，
以免在 Dock 里比系统图标显大。

依赖 Pillow（pip install pillow）与系统自带的 iconutil。打包脚本在依赖缺失时
会跳过图标而不是让整个打包失败——图标是锦上添花，不该挡住产物。
"""

import os
import subprocess
import sys
import tempfile

from PIL import Image, ImageDraw

# macOS 图标规范：1024 画布内内容区 824、圆角 185、四周留 100 出血
CANVAS = 1024
INSET = 100
RADIUS = 185
TOP = (13, 148, 136)  # teal-600
BOTTOM = (12, 74, 110)  # 深青蓝


def gradient(size: int) -> Image.Image:
    """对角渐变底色（逐行插值，够平滑且不依赖第三方渐变实现）。"""
    img = Image.new("RGB", (size, size))
    px = img.load()
    for y in range(size):
        for x in range(size):
            # 对角权重：左上偏亮，右下偏深
            t = (x + y) / (2 * (size - 1))
            px[x, y] = tuple(
                round(TOP[i] + (BOTTOM[i] - TOP[i]) * t) for i in range(3)
            )
    return img


def slab(draw: ImageDraw.ImageDraw, cx: float, cy: float, half_w: float,
         half_h: float, alpha: int) -> None:
    """一块等距投影的"瓦片"（菱形）。"""
    draw.polygon(
        [
            (cx, cy - half_h),
            (cx + half_w, cy),
            (cx, cy + half_h),
            (cx - half_w, cy),
        ],
        fill=(255, 255, 255, alpha),
    )


def make_master(size: int = CANVAS) -> Image.Image:
    """按尺寸等比生成主图。几何量必须随 size 缩放——早先用绝对值时，
    缩小尺寸会让圆角半径超过半宽、圆角方块退化成圆（预览时踩到过）。"""
    k = size / CANVAS
    inset, radius = round(INSET * k), round(RADIUS * k)

    base = gradient(size).convert("RGBA")
    mask = Image.new("L", (size, size), 0)
    ImageDraw.Draw(mask).rounded_rectangle(
        [(inset, inset), (size - inset - 1, size - inset - 1)],
        radius=radius,
        fill=255,
    )
    base.putalpha(mask)

    glyph = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    g = ImageDraw.Draw(glyph)
    cx = size / 2
    # 三层自下而上（越靠上越实）：读作"一层层摊开的瓦片"
    for i, alpha in enumerate((96, 168, 245)):
        cy = size / 2 + 148 * k - i * 146 * k
        slab(g, cx, cy, 196 * k, 80 * k, alpha)
    base.alpha_composite(glyph)
    return base


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: make-icon.py <out.icns>", file=sys.stderr)
        return 2
    out = os.path.abspath(sys.argv[1])
    master = make_master()

    with tempfile.TemporaryDirectory() as tmp:
        iconset = os.path.join(tmp, "AppIcon.iconset")
        os.makedirs(iconset)
        # iconutil 要求的固定命名与尺寸组合（1x / 2x）
        for pt in (16, 32, 128, 256, 512):
            for scale in (1, 2):
                px = pt * scale
                name = f"icon_{pt}x{pt}{'@2x' if scale == 2 else ''}.png"
                master.resize((px, px), Image.LANCZOS).save(
                    os.path.join(iconset, name)
                )
        os.makedirs(os.path.dirname(out), exist_ok=True)
        subprocess.run(["iconutil", "-c", "icns", iconset, "-o", out], check=True)
    print(f"icon ok: {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
