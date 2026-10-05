#!/usr/bin/env python3
"""逆向 2：追踪 Group 子列表的 slot 机制与末尾对齐。"""
import struct, sys, os

path = '/Users/yangtanfang/project/2026/AI/tangis/testdata/osgb-real/osgb/Tile_+000_+000.osgb'
data = open(path, 'rb').read()
body = data[24:]
n = len(body)
print(f'body len = {n}')

def u32(p): return struct.unpack_from('<I', body, p)[0], p+4
def u64(p): return struct.unpack_from('<Q', body, p)[0], p+8
def rstr(p):
    nl, p = u32(p)
    return body[p:p+nl].decode('utf8','replace'), p+nl

# Group 帧 @23
name, p = rstr(1)   # 跳过 0x30
bsize, p = u64(p)
print(f'顶层: {name} bsize={bsize} body数据起={p} 数据长={bsize-8} 数据止={p+bsize-8} (buffer {n})')

gstart = p
gend = p + bsize - 8
# 序言
classId, p = u32(p); z1, p = u32(p); flag, p = u32(p); z2, p = u32(p)
b3 = body[p:p+3]; p += 3
ssref, p = u32(p)
print(f'Group 序言: classId={classId} z1={z1} flag={flag} z2={z2} b3={b3.hex()} ssref=0x{ssref:08x} pos={p:#x}')
tail = body[p:p+6]; p += 6
print(f'尾6字节: {tail.hex()} pos={p:#x}')

# 子列表
ci = 0
while p + 12 < gend:
    slot_pos = p
    slot, p = u64(p)
    cname, cp = rstr(p)
    cbsize, cp = u64(cp)
    print(f'child[{ci}] @slot{slot_pos:#x}: slot={slot} (0x{slot:x}) 下一帧 {cname} bsize={cbsize}')
    ci += 1
    if ci > 8:
        break
    # 跳过该 child（假设 slot 覆盖 child 对象帧全部）
    p = slot_pos + 8 + slot
    if slot == 0 or p > gend:
        print('!! slot 异常，停止')
        break

print(f'循环结束后 pos={p:#x} gend={gend:#x} 剩余 {gend-p}')
dump_at = gend - 96 if gend-p >= 0 else p
for i in range(max(0,gend-112), n, 16):
    chunk = body[i:i+16]
    hexs = ' '.join(f'{b:02x}' for b in chunk)
    asc = ''.join(chr(b) if 32<=b<127 else '.' for b in chunk)
    mark = ' <-- gend' if i <= gend < i+16 else ''
    print(f'{i:6x}  {hexs:<48}  {asc}{mark}')
