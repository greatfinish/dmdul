# DMASM LAB05-METADATA-SCALE 实验记录

LAB05 验证镜像版 DMASM 在大容量成员盘、大文件目录和跨描述区分配下的物理格式，并把
结论落实到 `dmdul` 裸盘解析器。实验日期为 2026-08-09，DM8 build 为
`03134284604-20260707-335949-20228`。

## 环境

- 双节点：`192.168.17.107/.108`，现有镜像版 DMDSC 实验集群；
- 磁盘组：`META1`，group id 5，EXTERNAL，AU=1 MiB；
- 成员盘：单块 80 GiB VMware `monolithicFlat` 稀疏盘，DMASM 格式化容量 80000 AU；
- 设备：`/dev/dmasm/meta1`，磁盘名 `DMASMMETA1A`；
- 目录：`+META1/catalog/d000` 至 `d063`；
- 小文件：`f000001.dat` 至 `f005200.dat`，每个文件 1 MiB；
- 跨区文件：33000 MiB 的 `descriptor_span_au1.dat` 和 28000 MiB 的
  `descriptor_span_au2.dat`。

小文件与两个大文件均由 DMASM 正式分配，不是手工伪造裸盘字节。官方 `check` 对两个
大文件分别列出 33000 和 28000 个 logical AU，结果均为 `total 0 mirs err`。

宿主 `*-flat.vmdk` 的逻辑容量为 80 GiB。完成 5200 个一 AU 小文件及 33000/28000-AU
两个大文件分配后，`fsutil sparse queryrange` 统计的宿主实际分配空间仍为
143720448 字节（137.0625 MiB，占逻辑容量约 0.1673%）。这说明样本既经过了真实 DMASM
目录、INODE 和 AU map 分配，又没有因约 61 GiB 的逻辑零内容占满宿主磁盘。完整区间见
[host-sparse-ranges.txt](host-sparse-ranges.txt)。

## 关键结论

### 多描述 AU 与 descriptor region

一个 AU 内可容纳的 32 字节描述符数量为：

```text
capacity = au_size / 32
         = 1048576 / 32
         = 32768
```

镜像版官方术语是“描述 AU、INODE AU、数据 AU”。本实验将每个描述 AU 管理的地址范围
称为 descriptor region。80,000 AU 的成员盘具有三个描述 AU，起始物理 AU 分别为
0、32768、65536。任意物理 AU `P` 的描述符位置为：

```text
region_start     = floor(P / capacity) * capacity
local_au         = P - region_start
descriptor_offset = region_start * au_size + local_au * 32
```

AU 0 的前 256 字节是磁盘头，所以普通分配从物理 AU 8 开始。后续 region 的起始 AU 本身
就是描述 AU，普通文件映射从 32769、65537 开始。最终三个描述 AU 中的活动文件描述符
分别为 32760、32767 和 804 条，详见 [descriptor-regions.tsv](descriptor-regions.tsv)。

33000-AU 文件从物理 AU 5339 开始，sequence 27429 首次进入第二描述区的 AU 32769，
最后一个 sequence 32999 位于 AU 38339。28000-AU 文件从 AU 38340 开始，sequence
27196 首次进入第三描述区的 AU 65537，最后一个 sequence 27999 位于 AU 66340。

这与 DMASM 非镜像环境的 DESC/INODE/DATA“簇”不是同一分配模型。非镜像环境一个簇
固定包含 4 个连续的 1 MiB AU；LAB05 使用的是镜像环境，文件最小分配单位直接是 AU。

### 多 INODE AU

镜像版 INODE 记录固定为 512 字节，从 AU 内偏移 0 开始，因此 1 MiB AU 正好保存 2048
条记录。5200 个小文件、64 个子目录和内置对象共形成三个 INODE AU：

- sequence 0 -> 物理 AU 8：`+META1` 至 `f001980.dat`；
- sequence 1 -> 物理 AU 2117：`f001981.dat` 至 `f004028.dat`；
- sequence 2 -> 物理 AU 4166：`f004029.dat` 至两个跨区文件。

完整边界见 [inode-boundaries.tsv](inode-boundaries.tsv)。`.inode` 的描述符链在目录容量
越界时先落盘，INODE 自身的 size/extent 字段可能稍后更新；解析器按连续 descriptor
sequence 和 `prev/next` 链读取，避免在 2048 或 4096 条记录处截断。

### 大 extent 计数

镜像版 INODE 的 logical AU 数位于 `+0x103..+0x104`，是未对齐的小端 `uint16`。
33000 的原字节为 `E8 80`。旧解析把 `+0x100..+0x103` 当作大端 `uint32`，在 0 至 255
范围内碰巧正确，但会把 33000 截断为 232。文件逻辑大小仍按 `+0x118` 起的 7 字节
小端单位值乘 256 计算。

## 阶段结果

[stages.tsv](stages.tsv) 保存每个稳定阶段的文件数、目录条目、INODE AU、剩余 AU 和最高
file id。几个关键拐点是：

| 阶段 | 目录条目 | INODE AU | 结果 |
| --- | ---: | ---: | --- |
| 1900 文件 | 1968 | 1 | 尚未越过 2048 条记录 |
| 2000 文件 | 2068 | 2 | 新增 AU 2117 |
| 4000 文件 | 4068 | 2 | 尚未越过 4096 条记录 |
| 4100 文件 | 4168 | 3 | 新增 AU 4166 |
| 5200 文件 | 5268 | 3 | 最高小文件 ID `0x850014A2` |
| 两个跨区文件后 | 5270 | 3 | 三个描述区均有真实文件映射 |

## 冷态验证

按 `DB -> ASM -> CSS` 顺序停止两节点全部写进程后，直接读取 `/dev/dmasm/meta1`：

```text
group=META1 disk_aus=80000 files=5270 inode_aus=3
span_file=+META1/descriptor_span_au1.dat extents=33000 first_second_region_sequence=27429
span_file=+META1/descriptor_span_au2.dat extents=28000 first_third_region_sequence=27196
PASS
```

测试同时读取两个跨区点和两个文件尾，耗时约 0.07 秒。关键描述区和 INODE AU 的冷态
哈希见 [cold-metadata-sha256.txt](cold-metadata-sha256.txt)。验证后两节点按
`CSS -> ASM -> DB` 恢复，六个端点均为 `OPEN / WORKING / OK / TRUE`，两台主机没有
failed systemd unit。

完整原始证据保存在实验机：

```text
/dmdata/DMASM_MIRROR/lab05-metadata-scale/evidence
```

其中包括各阶段三个描述区、三个 INODE AU、官方递归目录、官方文件检查、冷态读取日志
和服务停启日志。二进制证据不纳入 Git。

## 解析器与测试

- `loadMirrorAllocationMaps` 按描述区分段读取，不再假设全部描述符位于 AU 0；
- `loadMirrorInodes` 按 `.inode` descriptor sequence 合并所有 INODE AU；
- INODE extent 数改为读取 `+0x103` 的小端 16 位值；
- `TestRawASMMirrorReadsDescriptorFromSecondRegion` 覆盖合成跨区描述符；
- `TestRawASMMirrorParsesWideExtentCount` 覆盖 33000-AU INODE；
- `TestRawASMMirrorMetadataScaleDisk` 是环境变量控制的真实裸盘集成测试。

真实盘测试命令：

```bash
DMDUL_TEST_DMASM_METADATA_SCALE_DISK=/dev/dmasm/meta1 \
DMDUL_TEST_DMASM_METADATA_SCALE_FILES=5200 \
DMDUL_TEST_DMASM_METADATA_SCALE_SPAN=+META1/descriptor_span_au1.dat \
DMDUL_TEST_DMASM_METADATA_SCALE_SPAN2=+META1/descriptor_span_au2.dat \
go test -run '^TestRawASMMirrorMetadataScaleDisk$' -v ./internal/dm
```

## 工具兼容性

该 build 的 `dmasmtoolm` 批处理输入必须使用 LF。CRLF 会让 `exit\r` 无法匹配命令，
客户端在标准输入结束后持续打印 `ASM>`。`generate-file-batches.ps1` 因此固定以 ASCII/LF
写出命令文件。

以上结论来自受控样本，不是达梦公开格式规范。正式恢复仍应使用全部节点停写后的完整
成员盘副本，并保留原盘只读。
