#!/usr/bin/env python3
"""逆向 3：完整对象树 walk（slot=u64自身+子对象帧）。"""
import struct, sys, os
from collections import Counter

path = sys.argv[1] if len(sys.argv) > 1 else '/Users/yangtanfang/project/2026/AI/tangis/testdata/osgb-real/osgb/Tile_+000_+000.osgb'
VERBOSE = set(sys.argv[2:]) or {'osg::Geometry'}
data = open(path, 'rb').read()
body = data[24:]
n = len(body)

def u8(p):  return body[p], p+1
def u32(p): return struct.unpack_from('<I', body, p)[0], p+4
def u64(p): return struct.unpack_from('<Q', body, p)[0], p+8
def f32(p): return struct.unpack_from('<f', body, p)[0], p+4
def rstr(p):
    nl, p = u32(p)
    return body[p:p+nl].decode('utf8','replace'), p+nl

counts = Counter()

def dump(p, cnt, depth, label=''):
    ind = '  ' * depth
    if label: print(f"{ind}{label}")
    for i in range(0, cnt, 16):
        chunk = body[p+i:p+i+16]
        hexs = ' '.join(f'{b:02x}' for b in chunk)
        asc = ''.join(chr(b) if 32<=b<127 else '.' for b in chunk)
        print(f"{ind}  {p+i:6x}  {hexs:<48}  {asc}")

def frame(p, depth, force_verbose=False):
    name, p = rstr(p)
    bsize, p = u64(p)
    start, fend = p, p + bsize - 8
    counts[name] += 1
    return name, start, fend

def walk(p, end, depth, owner, child_list=False):
    """返回 mesh 数。child_list=True 时每项前有 [u64 slot][帧]。"""
    meshes = []
    while p < end:
        if child_list:
            slot_pos = p
            slot, p = u64(p)
        cname, cp = rstr(p)
        cbsize, cp = u64(cp)
        cstart, cfend = cp, cp + cbsize - 8
        counts[cname] += 1
        verbose = cname in VERBOSE
        if verbose:
            print(f"{'  '*depth}@{cstart:<7} {cname} body={cbsize}")
        if cname in ('osg::Group', 'osg::Geode'):
            classId, p = u32(cp); z1, p = u32(p); flag, p = u32(p); z2, p = u32(p)
            b3 = body[p:p+3]; p += 3
            ssref, p = u32(p)
            tail = body[p:p+6]; p += 6
            if verbose or ssref != 0xFFFFFFFF:
                print(f"{'  '*depth}@{cstart:<7} {cname} classId={classId} flag={flag} b3={b3.hex()} ssref=0x{ssref:08x} tail={tail.hex()}")
            if ssref != 0xFFFFFFFF:
                # 内联 StateSet：[u64 slot][StateSet 对象]
                slot, p2 = u64(p)
                ss_name, p3 = rstr(p2)
                print(f"{'  '*depth}  内联对象: slot={slot} -> {ss_name}")
                ssb, p4 = u64(p3)
                dump(p2, min(72, cfend-p2), depth+1, '')
                p = cfend  # StateSet 细节后面专门分析
                continue
            kid = walk(p, cfend, depth+1, cname, child_list=True)
            meshes.extend(kid)
            p = cfend
        elif cname == 'osg::Geometry':
            p = walk_geometry(cstart, cfend, depth, owner)
        elif cname in ('osg::DrawElementsUByte','osg::DrawElementsUShort','osg::DrawElementsUInt'):
            p = walk_draw_elements(cname, cstart, cfend, depth)
        elif cname in ('osg::Vec3Array','osg::Vec2Array','osg::Vec4Array'):
            p = walk_array(cname, cstart, cfend, depth)
        elif cname == 'osg::Image':
            p = walk_image(cstart, cfend, depth)
        else:
            p = cfend
        if p != cfend:
            print(f"{'  '*depth}!! {cname} 对齐差异: pos={p:#x} fend={cfend:#x} diff={cfend-p}")
            p = cfend
        if child_list:
            actual = p - slot_pos
            if actual != slot:
                print(f"{'  '*depth}!! slot 声明 {slot} 实际 {actual} (child {cname} @ {slot_pos:#x})")
    return meshes

def walk_geometry(start, end, depth, owner):
    # 前言 hexdump 64 字节
    dump(start, 96, depth, '--- Geometry 开头 96 字节 ---')
    return end

def walk_draw_elements(name, start, end, depth):
    dump(start, 80, depth, f'--- {name} 前 80 字节 ---')
    return end

def walk_array(name, start, end, depth):
    dump(start, 80, depth, f'--- {name} 前 80 字节 ---')
    return end

def walk_image(start, end, depth):
    dump(start, 288, depth, '--- osg::Image 前 288 字节 ---')
    return end

# 顶层
assert body[0] == 0x30
walk(1, n, 0, None)
print('\n=== 对象统计 ===')
for cls, cnt in counts.most_common():
    print(f'  {cls:<40} x{cnt}')
