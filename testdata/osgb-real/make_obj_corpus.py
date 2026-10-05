#!/usr/bin/env python3
"""TanGIS 真实 osgconv 语料 —— OBJ 侧生成器。

生成 8×8 = 64 个精细叶 tile（33×33 顶点地形网格）+ 4×4 = 16 个粗层父 tile
（17×17 顶点、覆盖 2×2 叶 tile 的 200×200 m 区域），每个 OBJ 带：
  - mtllib 引用 tile.mtl，usemtl atlas，map_Kd → textures/atlas256.png；
  - vt UV（把共享贴图铺满 tile）+ vn 顶点法线（解析高度场）。

后续用 docker 内 openscenegraph 的 osgconv 批量转换为真实 OSGB：
  osgconv objs/Tile_+000_+000.obj osgb/Tile_+000_+000.osgb
（命令见 README.md）

用法：python3 make_obj_corpus.py
"""

import math
import os
import struct
import zlib

ROOT = os.path.dirname(os.path.abspath(__file__))
OBJ_DIR = os.path.join(ROOT, "objs")
TEX_DIR = os.path.join(ROOT, "textures")
TEX_NAME = "atlas256.png"

# 布局：叶 tile 100 m 见方，间距 120 m（留 20 m 街道），8×8 网格
LEAF_N = 8
LEAF_SIZE = 100.0
LEAF_STEP = 120.0
LEAF_GRID = 33  # 每边顶点数（32×32×2 = 2048 三角形）

# 粗层：4×4，每个覆盖 2×2 叶 tile（200×200 m），步距 = 2×LEAF_STEP
COARSE_N = 4
COARSE_GRID = 17


def height(x: float, y: float) -> float:
    """确定性高度场（米）：两轴正弦丘陵 + 大尺度缓坡。"""
    return (
        2.5 * math.sin(x / 15.0) * math.cos(y / 12.0)
        + 1.2 * math.sin(x / 47.0 + 1.3) * math.sin(y / 53.0)
        + 0.05 * x * 0.1
    )


def emit_obj(path: str, ox: float, oy: float, size: float, grid: int, name: str):
    """写一个 OBJ tile：局部网格 (ox..ox+size, oy..oy+size)，Z 为高度。"""
    verts, uvs, norms, faces = [], [], [], []
    for j in range(grid):
        for i in range(grid):
            x = ox + size * i / (grid - 1)
            y = oy + size * j / (grid - 1)
            z = height(x, y)
            verts.append((x, y, z))
            uvs.append((i / (grid - 1), j / (grid - 1)))
            # 中心差分法线（对高度场求梯度）
            d = size / (grid - 1)
            hx = height(x + d, y) - height(x - d, y)
            hy = height(x, y + d) - height(x, y - d)
            n = (-hx, -hy, 2.0 * d)
            l = math.sqrt(n[0] ** 2 + n[1] ** 2 + n[2] ** 2)
            norms.append((n[0] / l, n[1] / l, n[2] / l))
    for j in range(grid - 1):
        for i in range(grid - 1):
            a = j * grid + i + 1
            b = a + 1
            c = (j + 1) * grid + i + 1
            d2 = c + 1
            faces.append((a, c, b))
            faces.append((b, c, d2))

    mtl_name = f"{name}.mtl"
    with open(os.path.join(OBJ_DIR, mtl_name), "w") as f:
        f.write(f"newmtl atlas\nKd 1.0 1.0 1.0\nmap_Kd ../textures/{TEX_NAME}\n")

    with open(path, "w") as f:
        f.write(f"# tangis real-corpus tile {name}\n")
        f.write(f"mtllib {mtl_name}\nusemtl atlas\n")
        for v in verts:
            f.write(f"v {v[0]:.4f} {v[1]:.4f} {v[2]:.4f}\n")
        for vt in uvs:
            f.write(f"vt {vt[0]:.6f} {vt[1]:.6f}\n")
        for vn in norms:
            f.write(f"vn {vn[0]:.4f} {vn[1]:.4f} {vn[2]:.4f}\n")
        for a, b, c in faces:
            f.write(f"f {a}/{a}/{a} {b}/{b}/{b} {c}/{c}/{c}\n")


def write_png(path: str, w: int, h: int):
    """手写 PNG（无 PIL 依赖）：RGB 棋盘 + 双向渐变，模拟摄影纹理。"""
    rows = []
    for y in range(h):
        row = bytearray([0])  # filter type 0（每行一个）
        for x in range(w):
            checker = ((x // 32) + (y // 32)) % 2
            r = (x * 255) // (w - 1)
            g = (y * 255) // (h - 1)
            b = 40 + checker * 90
            row += bytes((r, g, b))
        rows.append(bytes(row))
    raw = b"".join(rows)
    comp = zlib.compress(raw, 9)

    def chunk(tag: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data)) + tag + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    ihdr = struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)  # 8bit RGB
    with open(path, "wb") as f:
        f.write(b"\x89PNG\r\n\x1a\n")
        f.write(chunk(b"IHDR", ihdr))
        f.write(chunk(b"IDAT", comp))
        f.write(chunk(b"IEND", b""))


def main():
    os.makedirs(OBJ_DIR, exist_ok=True)
    os.makedirs(TEX_DIR, exist_ok=True)
    write_png(os.path.join(TEX_DIR, TEX_NAME), 256, 256)

    count = 0
    # 叶层：Tile_+COL_+ROW，COL/ROW ∈ 0..8（与合成语料 Tile_+xxx_+yyy 命名一致）
    for row in range(LEAF_N):
        for col in range(LEAF_N):
            name = f"Tile_+{col:03d}_+{row:03d}"
            emit_obj(
                os.path.join(OBJ_DIR, f"{name}.obj"),
                col * LEAF_STEP, row * LEAF_STEP, LEAF_SIZE, LEAF_GRID, name,
            )
            count += 1
    # 粗层：TileC_+COL_+ROW，覆盖 2×2 叶（200×200 m）
    for row in range(COARSE_N):
        for col in range(COARSE_N):
            name = f"TileC_+{col:03d}_+{row:03d}"
            emit_obj(
                os.path.join(OBJ_DIR, f"{name}.obj"),
                col * LEAF_STEP * 2, row * LEAF_STEP * 2,
                LEAF_SIZE * 2, COARSE_GRID, name,
            )
            count += 1
    print(f"OK: {count} OBJ + {TEX_NAME} → {OBJ_DIR}")


if __name__ == "__main__":
    main()
