# DMASM 裸盘离线读取与恢复

`dmdul` 可以绕过 DMASMSVR 和 `asmcmd cp`，直接从离线 DMASM 成员盘恢复 ASM 逻辑文件。
解析器把 `+GROUP/path/file.DBF` 映射为只读 `ReaderAt`，再复用 Standard Bootstrap、
storage page plan、数据页和 LOB 解析器。整个过程不生成中间 DBF，也没有裸盘写入口。

当前支持两类已经实机验证的 DMASM 布局：

| 布局 | 版本 | 已验证范围 |
| --- | ---: | --- |
| 非镜像环境 | 实测格式标识 `0x1004` | 1 MiB AU、4 MiB extent、INODE/XDESC 链 |
| 镜像环境 | 实测格式标识 `0x3001` | 1/4/32 MiB AU、EXTERNAL/NORMAL/HIGH、0/32 KiB 条带、多成员与多磁盘组 |

> 这是基于特定 DM8 build 和物理差分实验得到的恢复能力，不是达梦公开的磁盘格式规范。
> 正式恢复必须使用所有节点停止写入后的完整副本，并在隔离库验证导出结果。

![DMASM 磁盘逻辑结构与离线 ASM 文件定位示意图](images/dmasm-file-layout-map.png)

可编辑源图为 [dmasm-file-layout-map.svg](images/dmasm-file-layout-map.svg)。图中红、蓝、黄、
紫分别表示 DESC、INODE、DATA 和 DMASM REDO；非镜像与镜像环境分栏展示。

## 安全操作顺序

对普通文件系统上的 DBF，停库后复制文件即可。对 DMDSC 和共享 DMASM 盘，建议按以下
顺序操作：

1. 在一个控制节点停止数据库组，再停止 ASM 组。
2. 停止所有节点上的数据库、ASM 和 CSS 服务。
3. 确认没有 `dmserver`、`dmasmsvr`、`dmasmsvrm` 或 `dmcss` 进程持有成员盘。
4. 复制所有成员盘，或创建同一时点的存储层一致性快照。
5. 只在副本上运行 `dmdul`，原始盘保持只读和隔离。

只停止一个 DMDSC 节点不构成一致性快照。另一个节点仍可能修改 ASM 元数据、数据页、
联机日志和 DCRV。

## 使用方式

### Linux 裸设备

将同一恢复时点的所有用户磁盘组成员一次性配置。成员可以来自多个 ASM 磁盘组：

```text
sudo ./dmdul

DMDUL> set asm_disk /dev/dmasm/ext4a,/dev/dmasm/ext4b,/dev/dmasm/norm4a,/dev/dmasm/norm4b,/dev/dmasm/ext32a;
DMDUL> set system +NORM4/data/MIRRORDB/SYSTEM.DBF;
DMDUL> list asmfile;
DMDUL> list datafile;
DMDUL> bootstrap;
DMDUL> unload object ASMTEST;
DMDUL> unload user ASMTEST;
```

裸设备通常需要 `root` 或等价的只读设备权限。`dmdul` 始终使用 `os.Open` 打开成员盘。

### Windows flat VMDK 冷副本

VMware `monolithicFlat` 的 `*-flat.vmdk` 就是成员盘的原始字节，可直接作为输入：

```text
DMDUL> set asm_disk C:\snapshot\ext4a-flat.vmdk,C:\snapshot\ext4b-flat.vmdk,C:\snapshot\norm4a-flat.vmdk,C:\snapshot\norm4b-flat.vmdk,C:\snapshot\ext32a-flat.vmdk;
DMDUL> set system +NORM4/data/MIRRORDB/SYSTEM.DBF;
DMDUL> list datafile;
DMDUL> bootstrap;
```

不要把 VMDK descriptor 文件本身作为成员盘。应传入 descriptor 中 `FLAT` 行引用的
`*-flat.vmdk`。其他虚拟磁盘类型需要先确认其逻辑扇区表示方式。

`asm_disk` 和 ASM 逻辑 `system` 路径会写入 `init.dul`。`list asmfile` 列出 INODE 目录，
`list datafile` 读取每个 DBF 的第 0 页并显示 tablespace、group/file、页数和对齐状态。

bootstrap 还会把字典中各表的 `group_id/root_file` 与实际输入的 DBF 身份逐项核对。输入
不完整时会输出类似下面的诊断，并以 `SUCCESS_WITH_WARNINGS` 结束：

```text
[bootstrap] phase=datafile name=required-files status=WARNING missing_files="5/0(GROUP_5,tables=2)"
```

这个状态允许继续恢复 SYSTEM 字典和 DDL，但不能把受影响表的零行结果解释为空表。补齐
同一冷快照中的成员盘后重新 bootstrap，直到 `missing_files` 消失。

## 读取架构

```text
raw member disks / flat VMDKs
              |
              v
      disk header + group id
              |
              v
       INODE file catalogue
              |
              v
 AU map + copy array + striping
              |
              v
       logical ASM ReaderAt
              |
       +------+------+
       |             |
       v             v
   bootstrap      page plan
                     |
                     v
                  unload
```

`RawASMStorage` 先按磁盘头中的 group id 对输入成员分组。镜像组再选择最高 AU 1
generation 的组头副本，从 AU 2 成员表排除 OFFLINE/RECONNECT、DELETED 和已替换旧盘，
最后按组名路由 `+GROUP/...`。数据库目录按去掉组名前缀后的路径匹配，因此 SYSTEM、
ROLL、MAIN、TEMP 和分布在其他磁盘组中的用户 DBF 可以组成同一个离线数据源集合。
每个 DBF 仍以页 0 的 `(tablespace group, file, page=0)` 作为最终身份校验。
如果两个逻辑 DBF 声称同一 `(group,file)`，解析器会报告两个路径并停止，而不会按扫描
顺序任选一个。字典表的段摘要则从 storage root 和完整 leaf chain 推导，只读取 page plan
中的页；`tables.tsv` 因而可以写入 `header_file/header_block/blocks/extents`，无需扫描整个
ASM 数据文件。

## 官方存储模型与本文术语

达梦官方资料描述了两代不同的分配模型，不能把“簇”和“AU”混为同一个概念：

| 布局 | 最小文件分配单位 | 元数据分类 | 32 字节描述项管理对象 | 本文与代码中的名称 |
| --- | --- | --- | --- | --- |
| DMASM 非镜像环境 | 簇；一个簇由 4 个连续的 1 MiB AU 组成 | DESC 描述簇、INODE 簇、DATA 数据簇 | 一个 INODE 簇或 DATA 数据簇 | `XDESC`、`INODE AU`、4-AU `extent` |
| DMASM 镜像环境 | AU；大小可在创建组时指定 | 描述 AU、INODE AU、数据 AU | 一个 INODE AU 或数据 AU，并包含副本地址 | descriptor、INODE、logical/data AU |

用户给出的“最大值 64G”结构图属于 DMASM 非镜像环境：一个描述 AU 最多管理 16384 个簇，
每簇 4 个 1 MiB AU，因此覆盖 65536 AU，即 64 GiB。镜像环境改为按 AU 管理；一个描述 AU
可管理的 AU 数为 `AU_SIZE / 32`。AU=1 MiB 时是 32768 个 AU，这与 LAB05 在物理 AU
0、32768、65536 发现三个描述 AU 的结果一致。

官方概念在当前解析链中的落点如下：

- **DESC 描述簇 / 描述 AU**：保存文件 ID、前后地址、逻辑顺序和副本地址，不保存数据库
  文件正文。非镜像布局通过 XDESC 链定位 4-AU 数据簇；镜像布局通过分段 descriptor map
  定位每个逻辑数据 AU。
- **INODE 簇 / INODE AU**：保存 ASM 文件和目录的路径、大小、分配数量、副本数及条带
  属性。早期解析器扫描 `0x11` 类型 AU；镜像版从 `.inode`（low file id 2）的连续
  logical sequence 恢复完整目录。
- **DATA 数据簇 / 数据 AU**：保存 ASM 文件正文。`dmdul` 在 ASM 层不解释其中的数据库
  页语义，而是按 XDESC/AU map、镜像副本和条带规则提供逻辑 `ReaderAt`；上层再按 DBF
  页格式执行 bootstrap、page plan 和 unload。

官方还定义了 DMASM REDO：创建、删除、扩展、截断 ASM 文件时，DESC 与 INODE 的修改
先写 REDO，数据 AU 的普通写入不记录在该日志中。当前解析器尚不重放 DMASM REDO，
因此只把所有节点停写后的稳定元数据作为可恢复依据。

| 对象 | DMASM 非镜像环境当前覆盖 | DMASM 镜像环境当前覆盖 |
| --- | --- | --- |
| DESC | 能沿 32 字节 XDESC 前后链跨 AU 读取；真实 64 GiB 边界待验证 | LAB05 已实测三个描述 AU，并恢复两个大文件合计 61000 条 logical AU map |
| INODE | 能扫描活动 `0x11` INODE AU；真实多 INODE 簇待验证 | LAB05 已实测三个 INODE AU、5270 条目录记录和 2048/4096 边界 |
| DATA | 能把 4-AU 数据簇还原为逻辑文件，交给 DBF 页解析器 | 已覆盖 EXTERNAL/NORMAL/HIGH、副本重试和 0/32 KiB 条带 |
| DMASM REDO | 未解析、未重放 | 未解析、未重放 |

参考：[DMASM 非镜像环境介绍](https://eco.dameng.com/document/dm/zh-cn/pm/dmasm-introduce.html)、
[DMASM 镜像介绍](https://eco.dameng.com/document/dm/zh-cn/pm/dmasm-image-introduction.html)。

## DMASM 非镜像环境的实测布局

### 磁盘与 AU

| 项目 | 已验证值 |
| --- | ---: |
| 裸盘保留区 | 32 MiB |
| AU 大小 | 1 MiB |
| 数据 extent | 4 AU，即 4 MiB |
| 实测格式标识 | `0x1004` |
| INODE AU 类型 | `0x11` |
| XDESC AU 类型 | `0x13` |

这里的 XDESC AU 是官方“DESC 描述簇”的物理承载；每个有效 XDESC 项描述一个由 4 个
连续 AU 组成的数据簇。当前解析器支持 XDESC 项跨 AU 链接，但尚未用大于 64 GiB 的
非镜像环境真实成员盘验证第二个 DESC 描述簇。

磁盘头关键字段为小端序：

| 偏移 | 长度 | 含义 |
| ---: | ---: | --- |
| `0x00` | 2 | group id |
| `0x02` | 2 | disk id |
| `0x04` | 4 | AU number |
| `0x08` | 4 | format/version |
| `0x10` | 4 | AU type |
| `0x14` | 4 | last AU number |
| `0x18` | 变长 | `DMASM...` 磁盘名 |

### INODE 与 XDESC

INODE 记录固定为 512 字节，从 INODE AU 的 `0x400` 开始排列：

| 偏移 | 含义 |
| ---: | --- |
| `0x000` | file id，4 字节 |
| `0x004` | `+GROUP/...` 零结尾路径，最多 256 字节 |
| `0x104` | 逻辑文件大小，8 字节 |
| `0x124` | extent 数量，4 字节 |
| `0x128` | 首个 XDESC 地址，10 字节 |
| `0x140` | 目录标志 |

一个非镜像布局地址由 `disk_id:u16 + au_no:u32 + offset:u32` 组成。XDESC 项固定为
32 字节，从 XDESC AU 的 `0x400` 开始排列：

```text
descriptor_index = (descriptor_offset - 0x400) / 32
data_au          = descriptor_au + descriptor_index * 4
physical_offset  = 32 MiB + data_au * 1 MiB
```

解析器按 INODE 的 extent 数遍历 XDESC 链，并校验成员盘、AU 范围、描述符对齐、AU 类型
和链路成环。

## DMASM 镜像环境的实测布局

镜像环境不使用固定 32 MiB 保留区。AU 0 从物理偏移 0 开始，磁盘头、AU 分配描述符、
INODE 和数据 AU 都按磁盘声明的 AU 大小定位。

### 磁盘头与组头

磁盘头字段均为小端序：

| 偏移 | 长度 | 含义 |
| ---: | ---: | --- |
| `0x00` | 2 | AU 大小，单位 MiB |
| `0x02` | 2 | disk id |
| `0x08` | 4 | 实测格式标识 `0x3001` |
| `0x0C` | 4 | 签名 `0x21352811` |
| `0x10` | 2 | group id |
| `0x18` | 4 | 成员盘可用 AU 数 |
| `0x1C` | 32 | `DMASM...` 零结尾磁盘名 |

AU 1 是组头。LAB04 的成员变化差分确认了以下字段：

| AU 1 偏移 | 长度 | 含义 |
| ---: | ---: | --- |
| `0x00` | 2 | group id |
| `0x06` | 4 | group-header generation，小端 |
| `0x0A` | 2 | 副本数 |
| `0x0C` | 32 | 组名 |
| `0x34` | 2 | 当前成员数 |
| `0x36` | 2 | 当前 failure group 数 |
| `0x40` | 4 | 下一个成员号 |

generation 会在 ADD、OFFLINE、RECONNECT 完成、超时恢复和 REPLACE 等阶段推进，但不会
同步写到每块活跃成员盘。解析器先找出最高 generation 的可读组头，再读取它对应的
AU 2 成员表，不能把“组头较旧”等同于“该成员已失效”。

AU 2 从 `+0x00` 开始按 128 字节排列成员记录：

| 记录偏移 | 长度 | 含义 |
| ---: | ---: | --- |
| `0x00` | 2 | 实测状态码：0 DELETED、1 NORMAL、5 OFFLINE/RECONNECT |
| `0x02` | 2 | disk id |
| `0x08` | 4 | 成员可用 AU 数 |
| `0x0C` | 2 | failure group id |
| `0x2C` | 32 | failure group 名 |
| `0x4C` | 32 | `DMASM...` 磁盘名 |
| `0x7C` | 4 | 校验/尾标记，算法未解码 |

普通读取只纳入状态 1 且 disk id、磁盘名、AU 数均匹配的输入盘。同一 generation 的
多个成员表副本必须一致。该规则能排除替换后重新出现、仍携带完整旧 AU map 的原盘。

### 32 字节 AU 描述符

每个物理 AU 对应一条 32 字节描述符。单个描述区容量为 `AU_SIZE / 32` 条；成员盘超过
该容量时，会在每个容量边界开始新的描述区。物理 AU `P` 的描述符位置为：

```text
capacity          = au_size / 32
region_start      = floor(P / capacity) * capacity
local_au          = P - region_start
descriptor_offset = region_start * au_size + local_au * 32
```

AU=1 MiB 时每个描述区覆盖 32768 个物理 AU。LAB05 的 80000-AU 成员盘实测描述区位于
AU 0、32768、65536；两个大文件分别跨入第二、第三描述区。AU 0 前 256 字节由磁盘头
占用，所以第一个普通分配描述符对应物理 AU 8；后续描述区的起始 AU 本身保留，普通
文件映射从 AU 32769、65537 开始。

本文把每个描述 AU 覆盖的地址范围称为 descriptor region。region 的起始物理 AU
就是官方定义的“描述 AU”，其余被描述的普通分配 AU 承载 INODE 或文件数据。

| 描述符偏移 | 长度 | 含义 |
| ---: | ---: | --- |
| `0x00` | 6 | 前一逻辑 AU 的主地址 |
| `0x06` | 6 | 后一逻辑 AU 的主地址 |
| `0x0C` | 6 | 其他副本地址 1 |
| `0x12` | 6 | 其他副本地址 2 |
| `0x18` | 2 | INODE low file id |
| `0x1A` | 1 | 未确认；当前样本为 0 |
| `0x1B` | 2 | 16 位小端 logical AU sequence |
| `0x1D` | 1 | 副本失败位图；NORMAL 样本中 bit 0/1 对应 logical mirror slot 0/1 |
| `0x1E` | 2 | 描述符校验字段，算法尚未确认 |

镜像版地址由 `disk_id:u16 + au_no:u32` 组成，共 6 字节；全 `0xFF` 表示空地址。
`file_id=0xFFFE/0xFFFF` 是分配器保留标记，不按普通文件的链与副本语义解释。

NORMAL 的一个逻辑 AU 由“当前物理 AU + copy 数组”组成。输入包含被引用成员时，解析器
会校验副本的 file id 和 sequence，并校验 `prev/next` 分别指向 `sequence-1/+1`。
成员缺失时仍保留地址证据，只要另一个完整副本可读就能继续；读取失败时按副本顺序重试。
OFFLINE 差分中，`+0x1D` 从 0 变为 `0x01/0x02`，与 `V$ASM_FAIL_AU` 的失败盘、AU 和
mirror slot 一致；RESYNC、RECOVER 或 REPLACE 完成后恢复为 0。解析器把该字节与
sequence 分开保存，避免健康位被误解为 sequence 高位。

### 512 字节 INODE

镜像版 `.inode` 的 low file id 为 2。INODE 记录按 512 字节排列，目录跨多个 INODE AU
时按 logical AU sequence 合并。AU 较大时采用 1 MiB 分块扫描，不会一次读取整个 AU。

| 偏移 | 长度 | 字节序 | 含义 |
| ---: | ---: | --- | --- |
| `0x000` | 2 | 小端 | low file id |
| `0x002` | 1 |  | 类型；1/3 为目录，2/4 为文件 |
| `0x003` | 至 `0x0FF` |  | `+GROUP/...` 零结尾路径 |
| `0x103` | 2 | 小端 | 文件占用的逻辑 AU 数，未对齐 `uint16` |
| `0x110` | 1 |  | 分配属性，部分 DBF 非零，具体位义尚未解码 |
| `0x113` | 1 |  | 副本数 |
| `0x114` | 1 |  | 条带大小指数；5 表示 32 KiB |
| `0x115` | 1 |  | AU group；当前样本为 4 |
| `0x118` | 7 | 小端 | 逻辑大小单位数，乘 256 得到字节数 |

完整 file id 的已验证组合规则为：

```text
file_id = ((0x80 + group_id) << 24)
        | ((2 * log2(AU_MiB)) << 16)
        | low_file_id
```

例如 EXT4 的 4 MiB 文件为 `0x80040012`，NORM4 为 `0x81040012`，EXT32 的
32 MiB 文件为 `0x820A0012`。

`+0x103` 字段由 LAB05 的 33000-AU 文件确认：原字节为 `E8 80`。此前按
`+0x100..+0x103` 大端读取只在 0 至 255 AU 时碰巧成立，会把 33000 截断为 232。

### 细粒度条带

`striping=0` 时，逻辑 AU 顺序直接对应 AU map sequence。`striping=32 KiB` 时，当前
样本以 4 个 AU 为一组交错。设 `S` 为条带字节数，`A` 为 AU 字节数，`G` 为 AU group：

```text
group_bytes    = A * G
map_base       = (logical_offset / group_bytes) * G
within_group   = logical_offset % group_bytes
stripe_index   = within_group / S
map_index      = map_base + (stripe_index % G)
physical_inner = (stripe_index / G) * S + (within_group % S)
```

读取在每个条带边界重新计算映射，因而可以跨条带、AU、4-AU group 和文件尾执行
`ReadAt`。六个确定性文件在这些边界上的页标签和逻辑偏移均已逐项通过。

## 实机验证

### DMASM 非镜像实验环境

- 单成员 `DMDATA`，1 MiB AU，32 KiB 数据库页。
- 从 INODE 恢复 256 MiB `SYSTEM.DBF`，Standard Bootstrap 约 6 秒。
- DMTEST 3 表共 13 行，planned/direct pages 为 3/3，零 fallback、零失败。

### 镜像版 LAB02 环境

镜像环境使用 DM8 build `20260707`，包含以下用户磁盘组：

| 磁盘组 | group id | AU | 冗余 | 成员 | 主要验证 |
| --- | ---: | ---: | --- | ---: | --- |
| EXT4 | 0 | 4 MiB | EXTERNAL | 2 | 单副本跨成员分配、0/32 KiB 条带 |
| NORM4 | 1 | 4 MiB | NORMAL | 2 | 双副本数组、任一单成员读取 |
| EXT32 | 2 | 32 MiB | EXTERNAL | 1 | 32 MiB AU、32 KiB 细条带 |

六个确定性文件总计 1.5 GiB。每个 4 KiB 页写入固定文件标签和逻辑偏移，官方
`check file` 均为 `0 mirs err`。`dmdul` 在文件头、条带边界、AU 边界、4-AU group
边界、中点和尾页读取到的标签、偏移和填充值全部一致。

完整冷快照验证还覆盖了数据库恢复链：

- 停止两节点数据库、ASM、CSS 和两台 VM 后，复制 8 条 VMDK 链，共 160 GiB；
- 16 个 VMDK/flat 文件源与副本 SHA-256 全部一致，8 条 descriptor 链均为 consistent；
- Windows 直接读取 5 个用户组 `*-flat.vmdk`，没有启动 DMASM 或生成中间 DBF；
- 发现 3 个磁盘组、5 个成员、55 个 ASM 文件和 8 个数据库数据文件；
- Standard Bootstrap 约 6 秒，恢复 1097 个对象、2 个用户、5 张表、33 列、1 个视图、
  3 个序列、2 个例程和 1 个触发器；
- ASMTEST 的 4 张表导出 14 行，planned/direct pages 为 4/4，零 fallback、零失败；
- 用户、表、约束、索引、注释、视图、序列、例程和触发器 DDL 均成功生成；
- 在线只读基线与冷副本产生的 9 个 DDL/数据文件 SHA-256 全部一致；
- 冷验证后两节点恢复为 OPEN，行数和函数结果保持不变。

### 镜像版 LAB03-HIGH 环境

LAB03 在同一双节点 DMDSC 环境中增加 `HIGH4` 磁盘组：四个成员分别属于四个 failure
group，AU 为 4 MiB，冗余级别为 HIGH。两个 256 MiB 确定性文件分别使用 coarse 和
32 KiB 条带；每个 4 KiB 逻辑页都写入文件标签和逻辑偏移。

- 两个文件各包含 64 个逻辑 AU，每个逻辑 AU 都解析出 3 个物理副本；
- 全量校验 131072 个 4 KiB 页，标签、逻辑偏移和填充值全部正确；
- `dmdul` 从裸成员盘重建的 256 MiB 数据文件与 `dmasmtoolm cp` 官方副本逐字节一致；
- HIGH 表空间中的 `ASMTEST.T_HIGH_COPY` 包含 20000 行，在线裸盘卸载按 351 个 page-plan
  叶子页直读，导出 20000 行、失败 0、fallback 0；
- 32 KiB 用户页同时验证了 40 字节扇区保护尾：七组 4 字节边界备份、4 字节保护字段和
  8 字节固定尾。解析器只在 slot 结构证据成立时恢复边界原字节。
- 两节点完全停写后复制 12 条 VMDK 链，共 223338305041 字节；24 个源/副本文件的
  SHA-256 全部一致，12 条 descriptor 链均为 consistent；
- Windows 直接从冷副本发现 5 个磁盘组、12 个成员、73 个 ASM 文件和 9 个 DBF；
  Standard Bootstrap 约 6 秒完成，冷副本仍导出 20000 行、失败 0、351/351 页直读；
- 在线与冷副本数据 SQL 的 SHA-256 完全相同。验证结束后两节点恢复 OPEN，在线统计不变。

实验资产和逐项证据见 [DMASM LAB03-HIGH 实验记录](../research/dmasm-lab03/README.md)。

### 镜像版 LAB04-REBALANCENORMAL 环境

LAB04 使用四成员 NORMAL 基线、第五块扩容盘和一块独立替换盘，写入 14 个 512 MiB
确定性文件。每个 4 KiB 页都完成标签、逻辑偏移和填充值校验。

- E 作为独立第五 failure group 时，在多档空间占用率下均无重平衡计划；改为加入已有
  `RBL_FG1` 后获得 `disk_id=8`，样本中 232 份 AU 副本从 A 迁移到 E。
- C OFFLINE 后产生 64 个失败 AU。超过修复期限后，DMASM 执行 `RECOVER 930/930`，
  删除 C 并把缺失副本恢复到其余成员。
- D 的短窗口恢复抓到 `RECONNECT` 和 `RESYNC 7/71 -> 71/71`；完成后失败 AU 清零，
  7 GiB 确定性文件全页校验通过。
- 备用 R 替换 D 时完成 `REPLACE 932/932`。旧 D 的 `disk_id=3` 变为 DELETED，R 获得
  新的 `disk_id=9` 并继承 `RBL_FG4`。
- 旧 D 再次出现时，修复前会被误聚合为第三副本；generation + AU 2 成员表筛选后，
  同时输入 A/B/旧D/E/R 仍只恢复出当前 `0/1/8/9` 两副本映射，7 GiB 全页校验通过。

实验资产和字段差分见
[DMASM LAB04-REBALANCENORMAL 实验记录](../research/dmasm-lab04/README.md)。

### 镜像版 LAB05-METADATA-SCALE 环境

LAB05 使用单成员 EXTERNAL、AU=1 MiB 的 `META1` 组和一块 80 GiB 稀疏成员盘。分阶段
创建 64 个子目录与 5200 个 1 MiB 文件，并增加 33000 MiB、28000 MiB 两个跨区文件。

- 目录最终包含 5270 条记录，file id 从 `0x85000001` 增长到 `0x850014A4`；
- `.inode` 每个 AU 保存 2048 条 512 字节记录，链为 physical AU 8、2117、4166；
- 三个描述区分别位于 physical AU 0、32768、65536，最终都有真实文件映射；
- 33000-AU 文件的 sequence 27429 首次映射到 AU 32769；
- 28000-AU 文件的 sequence 27196 首次映射到 AU 65537；
- 官方 `check` 分别列出 33000、28000 个 AU，均为 `0 mirs err`；
- 数据库、ASM、CSS 全部停止后，真实盘集成测试在约 0.07 秒内恢复 5270 条目录记录、
  3 个 INODE AU，并成功读取两个跨区点和文件尾。

实验同时修复了两个容量相关问题：描述符读取不再只读 AU 0；镜像 INODE extent 数改为
读取 `+0x103` 的小端 16 位值。完整阶段表、边界和冷态哈希见
[DMASM LAB05-METADATA-SCALE 实验记录](../research/dmasm-lab05/README.md)。

## 与 Oracle ASM 的关系

Oracle DUL 的研究思路可以复用：先恢复文件目录和 AU/extent 映射，再把逻辑文件交给
数据库页解析器。但 Oracle ASM 的 allocation unit、file directory 和 metadata block
不能作为 DMASM 的字段定义。`解密Oracle ASM内核.pdf` 适合帮助建立实验方法和术语，
不能证明 DMASM 的任何偏移或算法。

## 当前边界

- 镜像版磁盘头接受官方定义的 AU=1/2/4/8/16/32/64 MiB；当前确定性样本只验证了
  `0x3001`、AU=1/4/32 MiB、EXTERNAL/NORMAL/HIGH 和 0/32 KiB 条带。
- 64/128/256 KiB 条带和其他 DM8 build 尚无确定性样本。
- 非镜像环境已能沿 XDESC 链跨描述 AU 读取，但尚未取得超过 64 GiB、真实进入第二个
  DESC 描述簇的成员盘，也没有验证超过 2048 文件后的多 INODE 簇目录。
- 镜像版描述区已验证 AU=1 MiB、80000 AU、三个描述区；更大的成员盘和更多描述区尚未
  取得实机样本。
- 描述符 `+0x1D` 已确认是副本失败位图，但只验证了 NORMAL 的 bit 0/1；HIGH 的 bit 2、
  多副本组合和 `+0x1E` 校验算法仍需差分样本。
- AU 2 状态码 5 在本次实验中同时覆盖 OFFLINE 与 RECONNECT，裸记录本身尚不能区分
  两种在线视图状态。普通读取会排除所有非 NORMAL 成员。
- logical AU sequence 当前按 16 位读取。单文件超过 65536 个 AU 时是否切换到间接 extent
  映射尚未验证；官方镜像指标允许单文件达到 `0x1000000` 个 AU，因此这是必须补齐的
  大文件格式缺口。解析器不会把 `+0x1D` 健康位扩展为 sequence。
- INODE 时间字段尚未解码，不参与地址选择。
- DMASM REDO 的记录格式、校验和重放顺序尚未解码；创建、扩展、截断、删除或重平衡
  正在进行时取得的快照，不能依靠当前解析器自动完成元数据事务。
- 当前只恢复活动 INODE 和可达 descriptor 链，尚未实现已删除 ASM 文件、释放 INODE、
  孤立数据 AU 和 file id 复用历史的恢复。
- 空闲 AU、保留 AU、分配器高水位及回收链的完整格式尚未解码。当前容量统计来自活动
  descriptor 和官方工具对照，不能替代 DMASM 自身的空间一致性检查。
- 当前目标是 DBF 恢复，不解析 DCR、VOTE、联机日志和归档日志的业务语义。
- ASM 数据库目录的跨组发现要求去掉组名前缀后的目录相同，并以 DBF 页 0 再次核验身份。
- 运行中裸盘读取只能用于研究。即使一次读取成功，也不能替代完整冷快照。

### 后续实验优先级

1. **P0：非镜像环境 64 GiB 边界**。使用 96 GiB 以上非镜像成员盘，同时跨第二 DESC 描述簇
   和第二 INODE 簇，验证截图所示三类簇布局及 XDESC 地址公式。
2. **P0：镜像版超 65535 AU 单文件**。创建至少 66000-AU 文件，确定 INODE 如何表达
   官方 `0x1000000` AU 上限，以及是否存在间接 AU map 或高位字段。
3. **P0：DMASM REDO 差分**。在 CREATE/EXTEND/TRUNCATE/DROP 和 ADD/REBALANCE 的
   REDO 已写、元数据未完全落盘窗口采集快照，解码提交状态和重放顺序。
4. **P1：生命周期与残留恢复**。验证 rename/move/delete、file id 重用、释放 INODE 和
   孤立 descriptor/data AU，形成 ASM 文件级残留恢复模式。
5. **P1：组合覆盖**。补 AU=2/8/16/64 MiB、64/128/256 KiB 条带、HIGH 失败位组合、
   更多 descriptor region、DCRV 系统组和元数据副本损坏场景。
