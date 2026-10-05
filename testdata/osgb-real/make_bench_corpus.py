#!/usr/bin/env python3
"""TanGIS 吞吐基准语料（合成 OSGB 格式 + osgb-real 真实几何）。

背景：kernel 的 OSGB reader 目前只支持合成布局（真实 osgconv 3.6.5 输出在
osg::Geometry 序言处不兼容，详见 README.md 解析验证结论）。为给吞吐基准提供
可解析的大规模语料，本脚本把 osgb-real/objs 的 80 个 OBJ（真实地形几何，
33×33 / 17×17 网格）按 make_corpus.py 的合成字节布局放大复制为 640 个
带纹理 zlib 压缩 OSGB tile。

**注意：这是「合成格式、真实几何」语料，不是 osgconv 产物**，
基准文档中需如实标注。

用法：python3 make_bench_corpus.py
产出：bench-synth/bench_0000.osgb … bench_0639.osgb + manifest-bench.json
"""

import json
import math
import os
import struct
import sys
import zlib

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "osgb"))
from make_corpus import ATTR_BRACKETS, ATTR_COMPRESSED, build_tile  # noqa: E402

OBJ_DIR = os.path.join(HERE, "objs")
OUT_DIR = os.path.join(HERE, "bench-synth")
COPIES = 8        # 80 个 OBJ 模板 × 8 份平移副本 = 640 tile
SPACING = 1000.0  # 副本间平移间距（米），保证各 tile 包围盒不重叠
TEXTURE_REL = "textures/atlas256.png"


def read_obj_full(path):
    """读取本生成器产出的 OBJ：返回 (顶点, 法线, UV, 面)。"""
    verts, norms, uvs, faces = [], [], [], []
    with open(path) as f:
        for line in f:
            p = line.split()
            if not p:
                continue
            if p[0] == "v":
                verts.append([float(x) for x in p[1:4]])
            elif p[0] == "vn":
                norms.append([float(x) for x in p[1:4]])
            elif p[0] == "vt":
                uvs.append([float(x) for x in p[1:3]])
            elif p[0] == "f":
                faces.append([int(x.split("/")[0]) - 1 for x in p[1:4]])
    return verts, norms, uvs, faces


def main():
    os.makedirs(OUT_DIR, exist_ok=True)
    templates = sorted(f for f in os.listdir(OBJ_DIR) if f.endswith(".obj"))
    assert len(templates) == 80, f"期望 80 个模板 OBJ，实际 {len(templates)}"

    total_v = total_f = 0
    chunks = []
    n = 0
    for copy in range(COPIES):
        dx, dy = copy * SPACING, 0.0
        for tmpl in templates:
            verts, norms, uvs, faces = read_obj_full(os.path.join(OBJ_DIR, tmpl))
            verts = [[v[0] + dx, v[1] + dy, v[2]] for v in verts]
            total_v += len(verts)
            total_f += len(faces)
            # 压缩约定与 make_corpus.py 的 compressed/ 语料一致：
            # attributes bit0 置位，24B 头之后的全部字节（含首对象标记 0x30）
            # 为 zlib 流。build_tile 不便组合两种需求，按其字节布局自行封装。
            raw = build_tile(verts, norms, uvs, faces, texture=TEXTURE_REL,
                             compressed=False)
            data = (raw[:16]
                    + struct.pack("<I", ATTR_BRACKETS | ATTR_COMPRESSED)
                    + raw[20:24]
                    + zlib.compress(raw[24:], 6))
            name = f"bench_{n:04d}"
            with open(os.path.join(OUT_DIR, f"{name}.osgb"), "wb") as f:
                f.write(data)
            chunks.append({
                "id": name, "lod": 0,
                "bounds": {"min": [0, 0, 0], "max": [1, 1, 1]},
                "status": "pending", "attempts": 0,
            })
            n += 1

    manifest = {
        "task_id": "bench-synth",
        "chunks": chunks,
        "source": ".",
    }
    with open(os.path.join(OUT_DIR, "manifest-bench.json"), "w") as f:
        json.dump(manifest, f, indent=2)
        f.write("\n")

    print(f"OK: {n} tiles（{COPIES} 副本 × {len(templates)} 模板），"
          f"顶点 {total_v}，面 {total_f}，输出 {OUT_DIR}")


if __name__ == "__main__":
    main()
