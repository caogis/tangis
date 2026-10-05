#!/usr/bin/env python3
"""TanGIS 千 tile 级真实 osgconv 语料（M2-PERF 基准用）。

复用 make_obj_corpus.py 的生成思路（真实地形高度场 OBJ + atlas256 贴图），
把规模放大为 32×32 = 1024 个精细叶 tile（33×33 网格）+ 4×4 = 16 个粗层
父 tile（17×17 网格，各覆盖 8×8 = 64 个叶 tile），共 **1040 tile**：

  python3 make_1k_corpus.py
    ① 生成 objs1k/*.obj + *.mtl（宿主机，无第三方依赖）
    ② docker 内批量 osgconv → osgb-1k/*.osgb（真实 OSG 3.6 二进制，
       现有 kernel reader 可解析；texutre 为 INLINE_DATA 内嵌）
    ③ 写 manifest-1k.json（16 父 lod=0 挂 64 子 + 1024 叶 lod=1）

注意：bench-synth/ 是旧合成格式（现 reader 不再支持），本语料全部经
osgconv 转换，供千 tile 级切片基准使用。

用法：python3 make_1k_corpus.py（需要 docker；镜像缺失时自动构建
tangis-osgconv:bookworm 本地镜像）
"""

import math
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from make_obj_corpus import height, write_png, TEX_DIR, TEX_NAME  # noqa: E402

ROOT = os.path.dirname(os.path.abspath(__file__))
OBJ_DIR = os.path.join(ROOT, "objs1k")
OSGB_DIR = os.path.join(ROOT, "osgb-1k")
IMAGE = "tangis-osgconv:bookworm"

# 布局：叶 tile 100 m 见方，间距 120 m，32×32 网格
LEAF_N = 32
LEAF_SIZE = 100.0
LEAF_STEP = 120.0
LEAF_GRID = 33

# 粗层：4×4，每个覆盖 8×8 叶 tile（960×960 m）
COARSE_N = 4
COARSE_GRID = 17


def emit_obj(path, ox, oy, size, grid, name):
    """写一个 OBJ tile：局部网格 (ox..ox+size, oy..oy+size)，Z 为高度。"""
    verts, uvs, norms, faces = [], [], [], []
    for j in range(grid):
        for i in range(grid):
            x = ox + size * i / (grid - 1)
            y = oy + size * j / (grid - 1)
            z = height(x, y)
            verts.append((x, y, z))
            uvs.append((i / (grid - 1), j / (grid - 1)))
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
        f.write(f"# tangis 1k-corpus tile {name}\n")
        f.write(f"mtllib {mtl_name}\nusemtl atlas\n")
        for v in verts:
            f.write(f"v {v[0]:.4f} {v[1]:.4f} {v[2]:.4f}\n")
        for vt in uvs:
            f.write(f"vt {vt[0]:.6f} {vt[1]:.6f}\n")
        for vn in norms:
            f.write(f"vn {vn[0]:.4f} {vn[1]:.4f} {vn[2]:.4f}\n")
        for a, b, c in faces:
            f.write(f"f {a}/{a}/{a} {b}/{b}/{b} {c}/{c}/{c}\n")


def ensure_image():
    """本地缺失 osgconv 镜像时构建（debian + openscenegraph）。"""
    out = subprocess.run(
        ["docker", "image", "inspect", IMAGE],
        capture_output=True,
    )
    if out.returncode == 0:
        return
    print(f"构建 docker 镜像 {IMAGE}（一次性，约 1~2 分钟）...")
    script = (
        "apt-get update -qq && apt-get install -y -qq openscenegraph"
        " && rm -rf /var/lib/apt/lists/*"
    )
    subprocess.run(
        ["docker", "run", "--name", "tangis-osgconv-build",
         "debian:bookworm-slim", "bash", "-c", script],
        check=True,
    )
    subprocess.run(
        ["docker", "commit", "tangis-osgconv-build", IMAGE],
        check=True,
    )
    subprocess.run(["docker", "rm", "tangis-osgconv-build"], check=True)


def convert():
    """docker 内批量 osgconv：objs1k/*.obj → osgb-1k/*.osgb（8 并行）。"""
    objs = sorted(f for f in os.listdir(OBJ_DIR) if f.endswith(".obj"))
    os.makedirs(OSGB_DIR, exist_ok=True)
    cmd = (
        "ls objs1k/*.obj | xargs -P 8 -I{} sh -c "
        "'osgconv {} osgb-1k/$(basename {} .obj).osgb'"
    )
    subprocess.run(
        ["docker", "run", "--rm", "-v", f"{ROOT}:/work", "-w", "/work",
         IMAGE, "bash", "-c", cmd],
        check=True,
    )
    return len(objs)


def write_manifest(path, n_coarse):
    import json

    def chunk(cid, lod, children):
        return {
            "id": cid, "lod": lod,
            "bounds": {"min": [0, 0, 0], "max": [1, 1, 1]},
            "status": "pending", "attempts": 0,
            **({"children": children} if children else {}),
        }

    chunks = []
    # 粗层父 tile：TileC_+C_+R 覆盖 (32/4)×(32/4) = 8×8 叶
    per = LEAF_N // n_coarse
    for r in range(n_coarse):
        for c in range(n_coarse):
            children = []
            for rr in range(per):
                for cc in range(per):
                    col, row = c * per + cc, r * per + rr
                    children.append(f"Tile_+{col:03d}_+{row:03d}")
            chunks.append(chunk(f"TileC_+{c:03d}_+{r:03d}", 0, children))
    for row in range(LEAF_N):
        for col in range(LEAF_N):
            chunks.append(chunk(f"Tile_+{col:03d}_+{row:03d}", 1, []))
    manifest = {
        "task_id": "osgb-1k-bench",
        "chunks": chunks,
        "source": "osgb-1k",
    }
    with open(path, "w") as f:
        json.dump(manifest, f, indent=2, ensure_ascii=False)
        f.write("\n")
    return len(chunks)


def main():
    os.makedirs(OBJ_DIR, exist_ok=True)
    write_png(os.path.join(TEX_DIR, TEX_NAME), 256, 256)

    count = 0
    for row in range(LEAF_N):
        for col in range(LEAF_N):
            name = f"Tile_+{col:03d}_+{row:03d}"
            emit_obj(
                os.path.join(OBJ_DIR, f"{name}.obj"),
                col * LEAF_STEP, row * LEAF_STEP, LEAF_SIZE, LEAF_GRID, name,
            )
            count += 1
    coarse = 0
    per = LEAF_N // COARSE_N
    for row in range(COARSE_N):
        for col in range(COARSE_N):
            name = f"TileC_+{col:03d}_+{row:03d}"
            emit_obj(
                os.path.join(OBJ_DIR, f"{name}.obj"),
                col * per * LEAF_STEP, row * per * LEAF_STEP,
                per * LEAF_SIZE, COARSE_GRID, name,
            )
            coarse += 1
    print(f"OK: {count} 叶 OBJ + {coarse} 粗层 OBJ → {OBJ_DIR}")

    ensure_image()
    n = convert()
    print(f"OK: {n} OSGB → {OSGB_DIR}")

    total = write_manifest(os.path.join(ROOT, "manifest-1k.json"), COARSE_N)
    print(f"OK: manifest-1k.json（{total} 分块）")


if __name__ == "__main__":
    main()
