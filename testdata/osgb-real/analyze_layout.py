#!/usr/bin/env python3
"""逆向分析真实 osgconv 3.6.5 输出的 .osgb 字节流（只读）。

骨架：24B 头 + 0x30 标记 + 对象帧树 [u32 名长][名][u64 bodysize(含自身)][body]。
Group/Geode 序言按合成 reader 布局试探；Geometry/StateSet/Image 等完整 hexdump。
"""
import struct, sys, os
from collections import Counter

path = sys.argv[1] if len(sys.argv) > 1 else os.path.join(
    os.path.dirname(__file__), 'osgb', 'Tile_+000_+000.osgb')
VERBOSE_CLASSES = set(sys.argv[2:]) or {'osg::Geometry', 'osg::Image'}
data = open(path, 'rb').read()
body = data[24:]

def u8(p):  return body[p], p+1
def u32(p): return struct.unpack_from('<I', body, p)[0], p+4
def u64(p): return struct.unpack_from('<Q', body, p)[0], p+8
def f32(p): return struct.unpack_from('<f', body, p)[0], p+4
def rstr(p):
    n, p = u32(p)
    return body[p:p+n].decode('utf8', 'replace'), p+n

objects = []

def dump(p, n, depth):
    ind = '  ' * depth
    for i in range(0, n, 16):
        chunk = body[p+i:p+i+16]
        hexs = ' '.join(f'{b:02x}' for b in chunk)
        asc = ''.join(chr(b) if 32 <= b < 127 else '.' for b in chunk)
        print(f"{ind}  {p+i:6x}  {hexs:<48}  {asc}")

def walk(p, end, depth, ctx):
    meshes = []
    while p < end:
        name, p = rstr(p)
        bsize, p = u64(p)
        start = p
        fend = p + bsize - 8
        objects.append((start, name, bsize, depth))
        verbose = name in VERBOSE_CLASSES
        if verbose:
            print(f"{'  '*depth}@{start:<7} {name} body={bsize}")
        if name in ('osg::Group', 'osg::Geode'):
            # 序言：classId z1 flag=2 z2 [00 00 01] stateset-ref [00 01 01 00 00 00]
            classId, p = u32(p); z1, p = u32(p); flag, p = u32(p); z2, p = u32(p)
            assert flag == 2, f'{name} flag={flag}'
            b3 = body[p:p+3]; p += 3
            ssref, p = u32(p)
            if ssref != 0xFFFFFFFF:
                print(f"{'  '*depth}  {name} 带内联 StateSet（@{p}）")
                dump(p, min(96, fend-p), depth)
                # StateSet 内联：[u64 slot][StateSet 对象]（或引用 id？）
                slot, p = u64(p)
                # 内联 StateSet 对象帧
                p = walk_stateset(p, fend, depth+1)
                if p - (start) != slot and False: pass
            tail6 = body[p:p+6]; p += 6
            if verbose:
                print(f"{'  '*depth}  序言: classId={classId} z1={z1} flag={flag} z2={z2} b3={b3.hex()} ss={ssref:08x} tail={tail6.hex()}")
            kid_meshes = walk(p, fend, depth+1, name)
            # Geode 级纹理绑定给全部子 mesh
            meshes.extend(kid_meshes)
        elif name == 'osg::Geometry':
            meshes.append(walk_geometry(start, fend, depth))
        elif name in ('osg::DrawElementsUByte', 'osg::DrawElementsUShort', 'osg::DrawElementsUInt'):
            walk_draw_elements(name, start, fend, depth)
        elif name in ('osg::Vec3Array', 'osg::Vec2Array', 'osg::Vec4Array'):
            walk_array(name, start, fend, depth)
        elif name == 'osg::Image':
            walk_image(start, fend, depth)
        else:
            if verbose or name.startswith('osg::StateSet') or 'Texture' in name or 'LOD' in name or 'Material' in name:
                print(f"{'  '*depth}@{start:<7} {name} body={bsize} [skip] hexdump:")
                dump(start, min(160, fend-start), depth)
        assert p <= fend, f'{name}: 越界 {p} > {fend}'
        if p != fend:
            if verbose:
                print(f"{'  '*depth}  [对齐差异] 读到 {p}，帧应结束于 {fend}（差 {fend-p}）")
            p = fend
    return meshes

def walk_stateset(p, end, depth):
    name, p = rstr(p)
    bsize, p = u64(p)
    start, fend = p, p + bsize - 8
    print(f"{'  '*depth}@{start:<7} {name} body={bsize} (内联 StateSet)")
    dump(start, min(200, fend-start), depth)
    return fend

def walk_geometry(start, end, depth):
    print(f"{'  '*depth}@{start:<7} osg::Geometry body={end-start+8} ---- hexdump ----")
    dump(start, min(end-start, 400), depth)
    return None

def walk_draw_elements(name, start, end, depth):
    print(f"{'  '*depth}@{start:<7} {name} 前 80 字节:")
    dump(start, min(80, end-start), depth)

def walk_array(name, start, end, depth):
    print(f"{'  '*depth}@{start:<7} {name} 前 80 字节:")
    dump(start, min(80, end-start), depth)

def walk_image(start, end, depth):
    print(f"{'  '*depth}@{start:<7} osg::Image 前 288 字节:")
    dump(start, min(288, end-start), depth)

print(f'首字节 0x{body[0]:02x}')
walk(1, len(body), 0, None)
print(f'\n对象总数: {len(objects)}')
for cls, n in Counter(o[1] for o in objects).most_common():
    print(f'  {cls:<40} x{n}')
