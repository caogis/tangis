#!/usr/bin/env python3
"""TanGIS 合成 OSGB 语料生成器。

按 kernel/crates/osgb/src/parser.rs 逆向记录的字节布局构造 OSGB：

  [24B 头][0x30][Group 帧][Geode 帧][Geometry 帧]
    [DrawElementsUShort 帧][Vec3Array 顶点][Vec3Array 法线][Vec2Array UV]
  头 attributes bit0 置位时，24 字节头之后的全部字节为 zlib 压缩流。
  Geode 序言 StateSet 引用非 null 时跟随 [u64 slot][StateSet][Texture2D][Image]。

产物：
  compressed/Tile_+000_+000.osgb   —— 现有未压缩 tile 的 zlib 压缩版（数据逐字节等价）
  textured/Tile_+000_+000.osgb     —— 带 UV + StateSet/Texture2D/Image 的纹理语料
  textured/textures/atlas.png      —— 语料引用的 4x4 RGBA 贴图

用法：python3 make_corpus.py
"""

import os
import struct
import sys
import zlib

ROOT = os.path.dirname(os.path.abspath(__file__))
UNCOMPRESSED_DIR = ROOT
SRC_DIR = os.path.join(ROOT, "src")
COMPRESSED_DIR = os.path.join(ROOT, "compressed")
TEXTURED_DIR = os.path.join(ROOT, "textured")

OSG_MAGIC = 0x6C910EA1
FEATURE_MASK = 0x1AFB4545
SCENE_TYPE = 1
VERSION = 161
ATTR_BRACKETS = 0x4          # bit2：二进制括号（语料约定）
ATTR_COMPRESSED = 0x1        # bit0：zlib 压缩流
RESERVED = 1
FIRST_MARKER = 0x30

ARRAY_VEC2 = 3
ARRAY_VEC3 = 4
TARGET_EBO = 34963           # GL_ELEMENT_ARRAY_BUFFER
TARGET_VBO = 34962           # GL_ARRAY_BUFFER
USAGE_STATIC = 35044         # GL_STATIC_DRAW
NULL_REF = 0xFFFFFFFF


def header(attr):
    return struct.pack("<6I", OSG_MAGIC, FEATURE_MASK, SCENE_TYPE, VERSION, attr, RESERVED)


def frame(name, body):
    """对象帧：[u32 名长][名][u64 bodysize=8+len(body)][body]"""
    nb = name.encode()
    return struct.pack("<I", len(nb)) + nb + struct.pack("<Q", 8 + len(body)) + body


def slot(obj):
    """子槽封装：[u64 slot=8+len(obj)][obj]（slot 从 u64 自身起点计）"""
    return struct.pack("<Q", 8 + len(obj)) + obj


def group_body(class_id, stateset, children):
    """Group/Geode 体：序言 + 可选 StateSet 槽 + 子列表前缀 + 子列表"""
    b = struct.pack("<III", class_id, 0, 2) + struct.pack("<I", 0)
    b += bytes([0, 0, 1])
    b += struct.pack("<I", NULL_REF if stateset is None else 1)
    if stateset is not None:
        b += slot(stateset)
    b += bytes([0, 1, 1, 0, 0, 0])
    for c in children:
        b += slot(c)
    return b


def stateset_body(texture2d):
    """StateSet 体：[cid][0][2][0][1][纹理属性数][slot×纹理属性]"""
    b = struct.pack("<III", 8, 0, 2) + bytes([0, 1])
    b += struct.pack("<I", 1 if texture2d is not None else 0)
    if texture2d is not None:
        b += slot(texture2d)
    return b


def texture2d_body(image):
    """Texture2D 体：[cid][0][2][0][1][slot×Image]"""
    b = struct.pack("<III", 9, 0, 2) + bytes([0, 1])
    b += slot(image)
    return b


def image_body(filename):
    """Image 体：[cid][0][2][0][1][名长][名][wrapS][wrapT]"""
    nb = filename.encode()
    b = struct.pack("<III", 10, 0, 2) + bytes([0, 1])
    b += struct.pack("<I", len(nb)) + nb
    b += struct.pack("<II", 33071, 33071)  # CLAMP_TO_EDGE
    return b


def vbo_body():
    """VBO 体（30 字节 bodysize 版）：[cid][0][2][0][target][usage][0]"""
    return struct.pack("<III", 7, 0, 2) + bytes([0]) + struct.pack("<II", TARGET_VBO, USAGE_STATIC) + bytes([0])


def ebo_body():
    """EBO 体：[cid][0][2][0][target][usage][0]"""
    return struct.pack("<III", 5, 0, 2) + bytes([0]) + struct.pack("<II", TARGET_EBO, USAGE_STATIC) + bytes([0])


def draw_elements_u16(faces):
    """DrawElementsUShort：序言 + EBO + [0][4][count][u16×count]；faces 为三元组列表"""
    flat = [i for f in faces for i in f]
    b = struct.pack("<III", 4, 0, 2) + bytes([0, 1])
    b += frame("osg::ElementBufferObject", ebo_body())
    b += struct.pack("<II", 0, 4)
    b += struct.pack("<I", len(flat))
    b += b"".join(struct.pack("<H", i) for i in flat)
    return b


def array_body(arr_type, values):
    """Vec2/3/4Array：序言 + VBO + [type][u16 0][count][数据]"""
    b = struct.pack("<III", 6, 0, 2) + bytes([0, 1])
    b += frame("osg::VertexBufferObject", vbo_body())
    b += struct.pack("<I", arr_type) + struct.pack("<H", 0)
    b += struct.pack("<I", len(values))
    comps = {ARRAY_VEC2: 2, ARRAY_VEC3: 3}[arr_type]
    for v in values:
        for c in range(comps):
            b += struct.pack("<f", v[c])
    return b


def geometry_body(vertices, normals, uvs, indices):
    """Geometry 体：序言 + primitive set + [0x01][数组]×k + 尾部 0×11"""
    b = struct.pack("<III", 3, 0, 1) + struct.pack("<I", 0)
    b += bytes([0, 0, 1])
    b += struct.pack("<I", NULL_REF)  # Geometry 级 StateSet = null
    b += struct.pack("<I", 0)
    b += bytes([1, 1, 0])
    b += struct.pack("<I", NULL_REF)  # primitive set 前缀引用
    b += bytes([1, 1, 0, 0, 0])
    b += frame("osg::DrawElementsUShort", draw_elements_u16(indices))
    arrays = [frame("osg::Vec3Array", array_body(ARRAY_VEC3, vertices)),
              frame("osg::Vec3Array", array_body(ARRAY_VEC3, normals))]
    if uvs is not None:
        arrays.append(frame("osg::Vec2Array", array_body(ARRAY_VEC2, uvs)))
    for a in arrays:
        b += bytes([1]) + a
    b += bytes(11)
    return b


def build_tile(vertices, normals, uvs, indices, texture=None, compressed=False):
    """组装完整 OSGB 字节流。

    texture: 纹理文件相对路径（None = 无 StateSet）；compressed: zlib 压缩流。
    """
    geometry = frame("osg::Geometry", geometry_body(vertices, normals, uvs, indices))
    geode = frame("osg::Geode", group_body(2, None, [geometry]))
    ss = None
    if texture is not None:
        img = frame("osg::Image", image_body(texture))
        tex = frame("osg::Texture2D", texture2d_body(img))
        ss = frame("osg::StateSet", stateset_body(tex))
    group = frame("osg::Group", group_body(1, ss, [geode]))
    attr = ATTR_BRACKETS | (ATTR_COMPRESSED if compressed else 0)
    head = header(attr) + bytes([FIRST_MARKER])
    payload = group if not compressed else zlib.compress(group, 9)
    return head + payload


def read_obj(path):
    """读取合成 OBJ：返回 (顶点 xyz, 面索引)。"""
    verts, faces = [], []
    with open(path) as f:
        for line in f:
            if line.startswith("v "):
                verts.append([float(x) for x in line.split()[1:4]])
            elif line.startswith("f "):
                faces.append([int(x) - 1 for x in line.split()[1:4]])
    return verts, faces


def make_textured_tile(obj_path, texture_rel):
    """OBJ → OSGB：坐标 (x,y,z)→(x,−z,y)，UV 平面投影 u=x/100, v=y/100。"""
    verts, faces = read_obj(obj_path)
    osVerts = [[v[0], -v[2], v[1]] for v in verts]
    normals = [[0.0, 1.0, 0.0]] * len(osVerts)  # OBJ z-up 平面法线 (0,0,1) → Y-up (0,1,0)
    uvs = [[v[0] / 100.0, v[1] / 100.0] for v in verts]
    return build_tile(osVerts, normals, uvs, faces, texture=texture_rel)


def make_png_atlas():
    """4x4 RGBA 棋盘 PNG（无依赖手工构造：IHDR+IDAT+IEND）。"""
    def chunk(typ, data):
        c = typ + data
        return struct.pack(">I", len(data)) + c + struct.pack(">I", zlib.crc32(c) & 0xFFFFFFFF)

    w = h = 4
    rows = []
    for y in range(h):
        row = bytes([0])  # filter = None
        for x in range(w):
            row += bytes([(255, 0)[(x + y) % 2], (0, 255)[(x + y) % 2], 0, 255])
        rows.append(row)
    ihdr = struct.pack(">IIBBBBB", w, h, 8, 6, 0, 0, 0)  # 8bit RGBA
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr)
            + chunk(b"IDAT", zlib.compress(b"".join(rows))) + chunk(b"IEND", b""))


def main():
    os.makedirs(COMPRESSED_DIR, exist_ok=True)
    os.makedirs(os.path.join(TEXTURED_DIR, "textures"), exist_ok=True)

    # 1) 压缩语料：现有未压缩 tile 的 zlib 版（数据等价，供 Rust 端到端对照）
    for name in ["Tile_+000_+000", "Tile_+000_+001", "Tile_+001_+000", "Tile_+001_+001"]:
        raw = open(os.path.join(UNCOMPRESSED_DIR, f"{name}.osgb"), "rb").read()
        assert not raw[16] & 1, f"{name}: 源文件应为未压缩"
        out = raw[:16] + struct.pack("<I", ATTR_BRACKETS | ATTR_COMPRESSED) + raw[20:]
        out = out[:24] + zlib.compress(out[24:], 9)
        path = os.path.join(COMPRESSED_DIR, f"{name}.osgb")
        open(path, "wb").write(out)
        print(f"compressed/{name}.osgb  {len(raw)} -> {len(out)} bytes")

    # 2) 纹理语料：带 UV + StateSet 引用 textures/atlas.png
    atlas = make_png_atlas()
    open(os.path.join(TEXTURED_DIR, "textures", "atlas.png"), "wb").write(atlas)
    name = "Tile_+000_+000"
    data = make_textured_tile(os.path.join(SRC_DIR, f"{name}.obj"), "textures/atlas.png")
    open(os.path.join(TEXTURED_DIR, f"{name}.osgb"), "wb").write(data)
    print(f"textured/{name}.osgb  {len(data)} bytes; atlas.png {len(atlas)} bytes")


if __name__ == "__main__":
    sys.exit(main())
