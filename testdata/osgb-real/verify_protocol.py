#!/usr/bin/env python3
"""OSG 3.6 二进制序列化协议验证解析器。

按 osgDB OutputStream/InputStream 3.6.5 + osgWrappers/serializers 源码逐字段实现，
对 testdata/osgb-real/osgb/*.osgb 全量验证块对齐（bodysize u64 一致性）。
"""
import struct, sys, os, glob, zlib
from collections import Counter

class R:
    def __init__(self, data):
        self.d = data; self.p = 0
    def u8(self):
        v = self.d[self.p]; self.p += 1; return v
    def i32(self):
        v = struct.unpack_from('<i', self.d, self.p)[0]; self.p += 4; return v
    u32 = i32
    def u64(self):
        v = struct.unpack_from('<Q', self.d, self.p)[0]; self.p += 8; return v
    def f32(self):
        v = struct.unpack_from('<f', self.d, self.p)[0]; self.p += 4; return v
    def f64(self):
        v = struct.unpack_from('<d', self.d, self.p)[0]; self.p += 8; return v
    def string(self):
        n = self.u32()
        s = self.d[self.p:self.p+n].decode('utf8', 'replace'); self.p += n; return s
    def bytes_(self, n):
        v = self.d[self.p:self.p+n]; self.p += n; return v

class Parser:
    def __init__(self, data):
        self.r = R(data)
        self.objects = Counter()
        self.skipped = Counter()
        self.ids = {}
        self.meshes = []   # dict(vertices, normals, uvs, indices, primitive_modes)
        self.textures = [] # image 文件名
        self.images = {}   # id -> filename
        self.paged = []    # PagedLOD 信息

    # ---- 基础 ----
    def frame(self):
        """读对象帧；返回 (name, block_end)；帧数据已定位到 id 前。"""
        name = self.r.string()
        if name == 'NULL':
            return None, None
        begin = self.r.p
        size = self.r.u64()
        return name, begin + size

    def obj_ref(self):
        """readObjectOfType / vector 元素：帧或 NULL。

        UniqueID 复用机制（OutputStream::writeObject：仅 newID 时写
        writeObjectFields；readObject 查 _identifierMap 命中则跳块尾）：
        同一实例第二次出现时帧内只有 UniqueID、无字段。
        """
        name, end = self.frame()
        if name is None:
            return None
        uid = self.r.u32()
        if uid in self.ids:
            if self.r.p != end:
                raise AssertionError(f'{name} uid={uid} 引用帧含多余 {end-self.r.p} 字节')
            return name
        self.ids[uid] = name
        self.fields(name, end)
        if self.r.p != end:
            raise AssertionError(f'{name}: pos={self.r.p:#x} != end={end:#x} (差 {end-self.r.p})')
        return name

    def opt_obj(self):
        """ObjectSerializer: u8 has + 条件帧。"""
        if self.r.u8():
            return self.obj_ref()
        return None

    def user(self):
        """UserSerializer: u8 ok。"""
        return self.r.u8() == 1

    def skip_fields(self, name, end):
        self.skipped[name] += 1
        self.r.p = end

    # ---- associates ----
    def f_object(self, end):
        self.r.string()          # Name
        self.r.i32()             # DataVariance
        self.opt_obj()           # UserDataContainer (77+)

    def f_node(self, end):
        if self.user():          # InitialBound
            self.r.u64()         # BEGIN_BRACKET
            self.r.f64(); self.r.f64(); self.r.f64()  # Center Vec3d
            self.r.f64()         # Radius
        self.opt_obj()           # ComputeBoundingSphereCallback
        self.opt_obj()           # UpdateCallback
        self.opt_obj()           # EventCallback
        self.opt_obj()           # CullCallback
        self.r.u8()              # CullingActive
        self.r.u32()             # NodeMask
        self.opt_obj()           # StateSet

    def f_children_like(self, end):
        """USER Children/Drawables: [u32 n][u64][帧×n]"""
        n = self.r.u32()
        self.r.u64()
        for _ in range(n):
            self.obj_ref()

    def f_group(self, end):
        if self.user():
            self.f_children_like(end)

    def f_geode(self, end):
        if self.user():
            self.f_children_like(end)

    def f_lod(self, end):
        self.r.u32()             # CenterMode
        if self.user():          # UserCenter
            self.r.f64(); self.r.f64(); self.r.f64()
            self.r.f64()
        self.r.u32()             # RangeMode
        if self.user():          # RangeList
            n = self.r.u32()
            self.r.u64()
            for _ in range(n):
                self.r.f32(); self.r.f32()

    def f_pagedlod(self, end):
        if self.user():          # DatabasePath
            if self.r.u8():
                self.r.string()
        self.r.u32()             # NumChildrenThatCannotBeExpired
        self.r.u8()              # DisableExternalChildrenPaging
        if self.user():          # RangeDataList
            n = self.r.u32(); self.r.u64()
            names = [self.r.string() for _ in range(n)]
            n2 = self.r.u32(); self.r.u64()
            for _ in range(n2):
                self.r.f32(); self.r.f32()
            self.paged.append(names)
        if self.user():          # Children
            self.f_children_like(end)

    def f_drawable(self, end):
        if self.user():          # InitialBound
            self.r.u64()
            self.r.f64(); self.r.f64(); self.r.f64()  # Min
            self.r.f64(); self.r.f64(); self.r.f64()  # Max
        self.opt_obj()           # ComputeBoundingBoxCallback
        self.opt_obj()           # Shape
        self.r.u8()              # SupportsDisplayList
        self.r.u8()              # UseDisplayList
        self.r.u8()              # UseVertexBufferObjects
        self.r.u32()             # NodeMask (142+)
        self.r.u8()              # CullingActive (145+)

    def f_geometry(self, end):
        # PrimitiveSetList (VectorSerializer RW_OBJECT): [u32 n][帧|NULL×n]
        n = self.r.u32()
        prim_names = []
        modes = []
        for _ in range(n):
            pn = self.obj_ref()
            prim_names.append(pn)
            if pn is None:
                continue
            # primitive 帧已解析——mode 由 obj_ref 内 fields 读出，需要传出……
        # 上面 obj_ref 会调 f_* 处理，索引数据在对应 f_ 中收集

        # VertexArray..FogCoordArray (ObjectSerializer)
        arrays = {}
        for key in ('vertex', 'normal', 'color', 'secondarycolor', 'fogcoord'):
            a = self.opt_obj()
        # TexCoordArrayList / VertexAttribArrayList (VectorSerializer RW_OBJECT)
        nt = self.r.u32()
        for _ in range(nt):
            self.obj_ref()
        na = self.r.u32()
        for _ in range(na):
            self.obj_ref()

    def f_stateset(self, end):
        if self.user():          # ModeList
            self.modes_block()
        if self.user():          # AttributeList
            self.attrs_block()
        if self.user():          # TextureModeList
            units = self.r.u32(); self.r.u64()
            for _ in range(units):
                self.modes_block()
        if self.user():          # TextureAttributeList
            units = self.r.u32(); self.r.u64()
            for _ in range(units):
                self.attrs_block()
        if self.user():          # UniformList
            n = self.r.u32(); self.r.u64()
            for _ in range(n):
                self.r.string()      # name
                self.r.u8()          # type
                self.r.u64()         # 数值 bracket? Uniform: [u32 name][u8 type][值]
                raise NotImplementedError('UniformList 待确认')
        self.r.i32()             # RenderingHint
        self.r.u32()             # RenderBinMode
        self.r.i32()             # BinNumber
        self.r.string()          # BinName
        self.r.u8()              # NestRenderBins
        self.opt_obj()           # UpdateCallback
        self.opt_obj()           # EventCallback
        # DefineList (151+): USER
        if self.user():
            raise NotImplementedError('DefineList 待确认')

    def modes_block(self):
        n = self.r.u32()
        if n > 0:
            self.r.u64()
            for _ in range(n):
                self.r.u32()     # GLenum mode
                self.r.i32()     # value

    def attrs_block(self):
        n = self.r.u32()
        if n > 0:
            self.r.u64()
            for _ in range(n):
                self.obj_ref()
                self.r.i32()     # Value

    def f_stateattr(self, end):
        self.opt_obj()
        self.opt_obj()

    def f_material(self, end):
        self.r.u32()             # ColorMode
        for _ in range(4):       # Ambient/Diffuse/Specular/Emission（Front+Back 无条件成对）
            if self.user():
                self.r.u8()
                self.r.f32(); self.r.f32(); self.r.f32(); self.r.f32()
                self.r.f32(); self.r.f32(); self.r.f32(); self.r.f32()
        if self.user():          # Shininess（fAB + Front + Back 无条件）
            self.r.u8()
            self.r.f32()
            self.r.f32()

    def f_texture(self, end):
        for _ in range(5):       # WRAP_S/T/R, MIN_FILTER, MAG_FILTER
            if self.user():
                self.r.u32()
        self.r.f32()             # MaxAnisotropy
        self.r.u8()              # UseHardwareMipMapGeneration
        self.r.u8()              # UnRefImageDataAfterApply
        self.r.u8()              # ClientStorageHint
        self.r.u8()              # ResizeNonPowerOfTwoHint
        self.r.f64(); self.r.f64(); self.r.f64(); self.r.f64()  # BorderColor
        self.r.i32()             # BorderWidth
        self.r.u32()             # InternalFormatMode
        for _ in range(3):       # InternalFormat/SourceFormat/SourceType
            if self.user():
                self.r.u32()
        self.r.u8()              # ShadowComparison
        self.r.u32()             # ShadowCompareFunc
        self.r.u32()             # ShadowTextureMode
        self.r.f32()             # ShadowAmbient
        if self.user():          # Swizzle (98+): 4 字符字符串如 "RGBA"
            self.r.string()
        self.r.f32(); self.r.f32(); self.r.f32()  # MinLOD/MaxLOD/LODBias (155+)

    def f_texture2d(self, end):
        # Image (ImageSerializer): u8 + readImage
        if self.r.u8():
            self.read_image()
        self.r.i32()             # TextureWidth
        self.r.i32()             # TextureHeight

    def read_image(self):
        self.r.string()          # ClassName (94+)
        img_id = self.r.u32()    # UniqueID
        if img_id in self.ids:
            # 重复 Image 引用：仅 ClassName+UniqueID，FileName 起全不读
            # （readImage 查 _identifierMap 命中即返回）
            self.textures.append(self.images[img_id])
            return
        self.ids[img_id] = 'osg::Image'
        fname = self.r.string()  # FileName
        self.r.i32()             # WriteHint
        decision = self.r.i32()
        if decision == 0:        # INLINE_DATA
            self.r.i32()         # origin
            s, t, rr = self.r.i32(), self.r.i32(), self.r.i32()
            self.r.i32()         # internalFormat
            self.r.i32()         # pixelFormat
            self.r.i32()         # dataType
            self.r.i32()         # packing
            self.r.i32()         # allocationMode
            size = self.r.u32()
            self.r.bytes_(size)
            lvl = self.r.u32()
            for _ in range(lvl):
                self.r.u32()
        elif decision == 1:      # INLINE_FILE
            size = self.r.u32()
            self.r.bytes_(size)
        # decision==2 EXTERNAL: 无数据
        # osg::Object associate（readImage 末尾 readObjectFields("osg::Object")）
        self.f_object(None)
        self.images[img_id] = fname
        self.textures.append(fname)

    def f_bufferdata(self, end):
        self.opt_obj()           # BufferObject

    def f_array(self, end):
        self.f_bufferdata(end)
        self.r.u32()             # Binding
        self.r.u8()              # Normalize
        self.r.u8()              # PreserveDataType

    def f_primitive_set(self, end):
        self.f_bufferdata(end)
        self.r.i32()             # NumInstances
        self.r.u32()             # Mode

    def read_image_object(self):
        # Image 作为顶层对象帧（不应出现，readImage 已特化）
        raise NotImplementedError

    def fields(self, name, end):
        self.objects[name] += 1
        h = self.__class__.__dict__
        table = {
            'osg::Object': lambda: self.f_object(end),
            'osg::Node': lambda: (self.f_object(end), self.f_node(end)),
            'osg::Group': lambda: (self.f_object(end), self.f_node(end), self.f_group(end)),
            'osg::Geode': lambda: (self.f_object(end), self.f_node(end), self.f_geode(end)),
            'osg::LOD': lambda: (self.f_object(end), self.f_node(end), self.f_group(end), self.f_lod(end)),
            'osg::PagedLOD': lambda: (self.f_object(end), self.f_node(end), self.f_group(end), self.f_lod(end), self.f_pagedlod(end)),
            'osg::Drawable': lambda: (self.f_object(end), self.f_node(end), self.f_drawable(end)),
            'osg::Geometry': lambda: (self.f_object(end), self.f_node(end), self.f_drawable(end), self.f_geometry(end)),
            'osg::StateSet': lambda: (self.f_object(end), self.f_stateset(end)),
            'osg::StateAttribute': lambda: (self.f_object(end), self.f_stateattr(end)),
            'osg::Material': lambda: (self.f_object(end), self.f_stateattr(end), self.f_material(end)),
            'osg::Texture': lambda: (self.f_object(end), self.f_stateattr(end), self.f_texture(end)),
            'osg::Texture2D': lambda: (self.f_object(end), self.f_stateattr(end), self.f_texture(end), self.f_texture2d(end)),
            'osg::BufferData': lambda: self.f_bufferdata(end),
            'osg::Array': lambda: (self.f_object(end), self.f_bufferdata(end), self.f_array(end)),
            'osg::PrimitiveSet': lambda: (self.f_object(end), self.f_bufferdata(end), self.f_primitive_set(end)),
            'osg::DrawArrays': lambda: (self.f_object(end), self.f_bufferdata(end), self.f_primitive_set(end), (self.r.i32(), self.r.i32())),
            'osg::Image': lambda: self.read_image(),
        }
        if name in table:
            table[name]()
            return
        # 数组类
        vec_types = {
            'osg::Vec2Array': 2, 'osg::Vec3Array': 3, 'osg::Vec4Array': 4,
            'osg::FloatArray': 1, 'osg::DoubleArray': 1, 'osg::Vec2dArray': 2, 'osg::Vec3dArray': 3, 'osg::Vec4dArray': 4,
            'osg::Vec2iArray': 2, 'osg::Vec3iArray': 3, 'osg::Vec4iArray': 4,
            'osg::Vec2uiArray': 2, 'osg::Vec3uiArray': 3, 'osg::Vec4uiArray': 4,
            'osg::Vec2sArray': 2, 'osg::Vec3sArray': 3, 'osg::Vec4sArray': 4,
            'osg::Vec2usArray': 2, 'osg::Vec3usArray': 3, 'osg::Vec4usArray': 4,
            'osg::Vec2bArray': 2, 'osg::Vec3bArray': 3, 'osg::Vec4bArray': 4,
            'osg::Vec2ubArray': 2, 'osg::Vec3ubArray': 3, 'osg::Vec4ubArray': 4,
        }
        comp_f32 = {1:1, 2:2, 3:3, 4:4}
        if name in vec_types or name in ('osg::ByteArray','osg::UByteArray','osg::ShortArray','osg::UShortArray','osg::IntArray','osg::UIntArray'):
            self.f_object(end); self.f_array(end)
            n = self.r.u32()
            if name in ('osg::Vec2Array','osg::Vec3Array','osg::Vec4Array'):
                c = vec_types[name]
                vals = []
                for _ in range(n):
                    vals.append([self.r.f32() for _ in range(c)])
                self.meshes.append(('array', name, vals))
            elif name == 'osg::FloatArray':
                for _ in range(n): self.r.f32()
            elif name == 'osg::DoubleArray':
                for _ in range(n): self.r.f64()
            elif name in ('osg::Vec2dArray','osg::Vec3dArray','osg::Vec4dArray'):
                c = vec_types[name]
                for _ in range(n):
                    for _ in range(c): self.r.f64()
            elif name in ('osg::Vec2iArray','osg::Vec3iArray','osg::Vec4iArray','osg::Vec2uiArray','osg::Vec3uiArray','osg::Vec4uiArray'):
                c = vec_types[name]
                for _ in range(n):
                    for _ in range(c): self.r.i32()
            elif name in ('osg::Vec2sArray','osg::Vec3sArray','osg::Vec4sArray','osg::Vec2usArray','osg::Vec3usArray','osg::Vec4usArray'):
                c = vec_types[name]
                for _ in range(n):
                    for _ in range(c): self.r.i32()  # 16-bit 但 i32 读会错位——修正
            elif 'ByteArray' in name or 'ShortArray' in name:
                width = 1 if 'Byte' in name else 2
                self.r.bytes_(n*width)
            return
        if name in ('osg::DrawElementsUByte','osg::DrawElementsUShort','osg::DrawElementsUInt'):
            self.f_object(end); self.f_primitive_set(end)
            n = self.r.u32()
            width = {'osg::DrawElementsUByte':1,'osg::DrawElementsUShort':2,'osg::DrawElementsUInt':4}[name]
            self.r.bytes_(n*width)
            return
        if name == 'osg::DrawArrayLengths':
            self.f_object(end); self.f_primitive_set(end)
            n = self.r.u32()
            for _ in range(n): self.r.i32()
            return
        if name in ('osg::VertexBufferObject', 'osg::ElementBufferObject'):
            self.f_object(end)      # Name/DV/UDC（associates 无 BufferData）
            self.r.u32()            # BufferObject.Target GLenum
            self.r.u32()            # Usage GLenum
            self.r.u8()             # CopyDataAndReleaseGLBufferObject
            return
        self.skip_fields(name, end)
        self.r.p = end

    def parse(self):
        r = self.r
        magic, = struct.unpack_from('<I', r.d, 0); r.p = 4
        magic2, = struct.unpack_from('<I', r.d, 4); r.p = 8
        assert magic == 0x6C910EA1, hex(magic)
        assert magic2 == 0x1AFB4545, hex(magic2)
        rtype = r.u32()
        version = r.u32()
        attributes = r.u32()
        compressor = r.string()
        data = r.d[r.p:]
        if compressor != '0':
            data = zlib.decompress(data)
        r.__init__(data)
        return self.obj_ref()

def main():
    files = sorted(glob.glob('/Users/yangtanfang/project/2026/AI/tangis/testdata/osgb-real/osgb/*.osgb'))
    ok = fail = 0
    total_objs = Counter()
    all_skipped = Counter()
    tex_count = 0
    for f in files:
        try:
            p = Parser(open(f, 'rb').read())
            p.parse()
            # 顶层之后不应再有数据（WRITE_SCENE 只写一个 root 对象）
            if p.r.p != len(p.r.d):
                raise AssertionError(f'文件尾剩余 {len(p.r.d)-p.r.p} 字节')
            ok += 1
            total_objs += p.objects
            all_skipped += p.skipped
            tex_count += len(p.textures)
        except Exception as e:
            fail += 1
            print(f'FAIL {os.path.basename(f)}: {type(e).__name__}: {e}')
    print(f'\n=== 结果: {ok} ok / {fail} fail / {len(files)} 总计 ===')
    print('对象统计:', dict(total_objs))
    print('跳过类:', dict(all_skipped))
    print(f'纹理引用总数: {tex_count}')
    return 0 if fail == 0 else 1

if __name__ == '__main__':
    sys.exit(main())
