# DM9 兼容性验证

本文记录 dmdul 对达梦 DM9 的首轮离线兼容性验证。结论来自真实冷快照、官方
`dexp/dimp` 对照和隔离实例回灌，不代表所有 DM9 build 的物理格式都相同。

## 验证结论

测试 build 的核心持久化字典与既有 DM8 样本保持高度兼容，但用户数据页和新对象仍要求
专门适配。完成适配后，Standard Bootstrap、对象 DDL、普通行、LOB、Long Row、分区、
HUGE 和 VECTOR 均可离线恢复；SQL、dmfldr 和 DMP 三条输出路径均完成实机验证。

动态性能视图数量和系统包版本的差异不影响这条离线链路。dmdul 的 bootstrap 读取
`SYSOBJECTS`、`SYSCOLUMNS`、`SYSINDEXES` 等持久化系统表，不依赖在线 `V$` 视图。

## 测试环境

| 项目 | 取值 |
| --- | --- |
| 操作系统 | 统信 UOS Server 20 1070e |
| 数据库 | DM Database Server 64 V9 |
| DB Version | `0x7000d` |
| Build | `03151060506-20260417-322930-20218` |
| 页大小 | 8/16/32 KiB |
| 簇大小 | 16/32/64 页 |
| 字符集 | GB18030、UTF-8、EUC-KR |
| 大小写敏感 | `CASE_SENSITIVE=1` |
| 快照文件 | 同一停库时点的 `SYSTEM.DBF`、`MAIN.DBF` 和 `dm.ctl` |

对比样本中，DM8 与 DM9 都包含 74 张核心 `STAB`、736 个系统表字段和 241 个传统静态
字典视图；已检查对象的名称、ID、字段数量和字段顺序一致。差异主要出现在系统包和
`V$/GV$` 动态性能视图层，因此不能只凭“字典表数量相同”判定物理解析无需改动。

## 页大小与字符集矩阵

同一 DM9 build 按支持范围执行了 3×3 冷快照矩阵。每个实例使用相同对象集：普通树表、
NOBRANCH 堆表、Long Row、行外 LOB、基础类型、RANGE/LIST/HASH 分区、VECTOR、额外模式、
约束索引、序列、触发器、视图、同义词、函数、过程、包、包体和对象授权。每组包含 3000
行跨页数据以及对应字符集的非 ASCII 标识符、注释、数据和程序源码。

| 页大小 | 字符集 | 簇大小 | 导出/回装行数 | 计划页/直读页 | fallback | 坏页 | 结果 |
| --- | --- | --- | ---: | ---: | ---: | ---: | --- |
| 8 KiB | GB18030 | 16 页 | 3027 | 85/85 | 0 | 0 | PASS |
| 8 KiB | UTF-8 | 16 页 | 3027 | 85/85 | 0 | 0 | PASS |
| 8 KiB | EUC-KR | 16 页 | 3027 | 85/85 | 0 | 0 | PASS |
| 16 KiB | GB18030 | 32 页 | 3027 | 53/53 | 0 | 0 | PASS |
| 16 KiB | UTF-8 | 32 页 | 3027 | 53/53 | 0 | 0 | PASS |
| 16 KiB | EUC-KR | 32 页 | 3027 | 53/53 | 0 | 0 | PASS |
| 32 KiB | GB18030 | 64 页 | 3027 | 38/38 | 0 | 0 | PASS |
| 32 KiB | UTF-8 | 64 页 | 3027 | 38/38 | 0 | 0 | PASS |
| 32 KiB | EUC-KR | 64 页 | 3027 | 38/38 | 0 | 0 | PASS |

该 DM9 build 的 `dminit help` 明确列出页大小仅支持 4/8/16/32 KiB。实际执行
`PAGE_SIZE=64` 返回：

```text
[PAGE_SIZE] value error, it can only be 4, 8, 16, 32.
fail to init db.
```

因此“64”在本次矩阵中是 `EXTENT_SIZE=64` 页，不是 64 KiB 数据页。4 KiB 虽受 DM9
初始化工具支持，但尚未纳入本轮 dmdul 兼容验证。

## 本次代码适配

### VECTOR 类型与向量索引

- 解码 `VECTOR` 的 FLOAT32、FLOAT64、INT8、BINARY 和稀疏 FLOAT32 行值。
- 从 `SYSCOLUMNS.SCALE$` 恢复 `VECTOR(dim, type[, SPARSE])`。
- 过滤 HNSW/IVFFLAT 自动生成的内部表与内部触发器，避免把索引实现对象当成用户表。
- 恢复 HNSW 和 IVFFLAT 用户索引的基本组织类型。

当前只恢复向量索引的组织类型。距离函数、精度和构建参数没有从该样本的离线字典中取得，
生成 DDL 使用 DM9 可执行的默认参数，导入后应按业务要求复核并重建索引。

### 数据页与页保护

- 32 KiB 用户页可能在每个 4 KiB 边界保存保护字节备份。记录头从 `0x3FFE` 等边界位置
  开始时，必须先识别保护尾区再恢复行头，否则每个受影响边界会漏一行。
- LOB (`0x20`) 和 Long Row (`0x22`) 页没有普通 slot 目录，改为按页类型恢复保护字节。
- NOBRANCH root 可包含多组 12 字节分支描述，page plan 需要合并全部 leaf/next 链。

### 非 Unicode 字符集与程序源码

- DM9 的短内联 `TEXT/CLOB` 在 GB18030/EUC-KR 数据库中使用 subtype `0x03`，UTF-8
  使用 `0x04`；两者采用相同长度字段和负载结构，解析器现已显式支持。
- dmdul 生成的 dmfldr 文本始终是 UTF-8。控制文件的 `CHARACTER_CODE` 描述输入文件，
  不是目标数据库字符集，因此统一声明 `UTF-8`，由 dmfldr 转换到 GB18030/EUC-KR。
- 16/32 KiB 字典页中，包规格的序号文本可能误指向较长的包体。例程恢复现在先校验
  `PACKAGE`/`PACKAGE BODY` 类型，再比较文本完整度，避免用包体覆盖包规格。

### 字典与对象编排

- 同一表存在多个 keyless storage root 时选择最新有效目录项，避免读取 ALTER/TRUNCATE
  前的旧存储。
- DM9 普通表的标志位可能与旧 HUGE 启发式重叠；只有存在 `$AUX` 证据时才按 HUGE 主表处理。
- `unload user` 会包含该用户拥有的全部模式，而不只处理用户同名模式。
- DMP 的主键内联到 `CREATE TABLE`，与官方 `dexp` 一致。外键在所有引用主键可用后创建。

### SQL 回灌告警

本次 DM9 `disql` 从 stdin 读取单行时最多接受 2499 字节。超过限制会报告
`DISQL-10053` 并跳过该语句，进程退出码仍可能为 0。dmdul 因此按 2499 字节的可移植
下限告警，并建议宽行改用 `data_format=fldr` 或 `data_format=dmp`。部分 DM8 build
允许更大的单条输入，但不能把该行为外推到 DM9。

## 对象与数据矩阵

| 类别 | 已验证内容 |
| --- | --- |
| 常规对象 | 用户、额外模式、角色授权、对象授权、表、索引、主外键、唯一/检查约束、注释 |
| 程序对象 | 视图、同义词、序列、触发器、过程、函数、包、包体 |
| 标量类型 | 字符/国家字符、BIT/BYTE、整数、DECIMAL/NUMBER、REAL/FLOAT/DOUBLE、二进制 |
| 日期时间 | DATE、TIME(6)、TIMESTAMP(9)、WITH TIME ZONE、LOCAL TIME ZONE |
| 区间与文档 | YEAR TO MONTH、DAY TO SECOND、TEXT、CLOB、BLOB、JSON/JSONB |
| 复杂存储 | 堆表、树表、临时表、Long Row、行外 LOB、RANGE/LIST/HASH 分区、HUGE |
| 向量 | FLOAT32、FLOAT64、INT8、BINARY、SPARSE、HNSW、IVFFLAT |

冷快照的离线字典共识别 20 张测试业务表、93 个字段、1 个视图、1 个序列、4 个程序单元、
1 个触发器、1 个同义词和 5 条对象权限。数据结果如下：

```text
rows exported:          11335
rows failed:                0
planned pages:             45
direct pages read:         45
fallback pages scanned:     0
SYSTEM precheck bad pages:  0
```

两张向量索引测试表分别恢复 5001 行。修复页保护边界前，它们会分别漏 7 行和 3 行；这些
记录都从保护边界附近开始，回归测试已锁定该物理场景。

## 回灌验证

### SQL

- 19 张表及相关对象 DDL 在 DM9 隔离模式中创建成功。
- 函数、过程、包、包体、视图和触发器均为 `VALID`。
- 宽行 SQL 文件内容完整，但不能直接通过当前 DM9 `disql` stdin 回灌；工具会明确告警。

### dmfldr

- 首轮 32 KiB/UTF-8 样本的 11335 行全部装载，拒绝行和格式错误均为 0。
- 16 组普通表/分区/HUGE/向量双向 `MINUS` 或规范化文本比较差异为 0。
- CLOB/BLOB 使用 `DBMS_LOB.COMPARE` 验证，JSON/JSONB 规范化比较差异为 0。
- `TIME(6)` 小数秒完整保留。
- 3×3 矩阵每组 3027 行均完成清空后原表回装；Long Row 长度 12000/9000、CLOB 长度
  12000、BLOB 长度 16、非 ASCII 数据和对象状态均与回装前一致。

### DMP

- `dimp SHOW=Y` 和 `CTRL_INFO=4` 校验通过。
- 元数据对象统计与同一源库的官方 `dexp ROWS=N` 一致：`TABLE_CONS=2`、
  `TABLE_CONS_UNIQUE=1`，主键内联在建表语句中。
- 在新建 DM9 实例中导入返回码为 0，11335 行全部导入，无警告、无无效对象，外键为
  `ENABLED/VALIDATED`。
- 除 `TIME` 外的 10006 条规范化标量和向量输出与源库逐行一致。
- 矩阵另选 8 KiB/GB18030、16 KiB/EUC-KR、32 KiB/UTF-8 三组执行删除用户后的
  OWNER DMP 真导入；三组均返回 0、无警告、3027 行、0 个无效对象，中文、韩文和包源码
  均可正确恢复。

DMP 对 `TIME` 小数秒的限制与既有 DM8 验证相同：`12:34:56.123456` 会变为
`12:34:56.000000`，导出阶段会报告受影响行数。需要保留该精度时使用 SQL 客户端或 dmfldr。

## 推荐验证流程

```text
DMDUL> set system /recovery/SYSTEM.DBF;
DMDUL> set data_dir /recovery;
DMDUL> bootstrap;
DMDUL> check pages;
DMDUL> list user;
DMDUL> unload object DM_TEST;
DMDUL> set data_format fldr;
DMDUL> unload user DM_TEST;
```

先在隔离 DM9 实例执行 DDL，再使用生成的 dmfldr 控制文件装载数据。需要对象和数据放在
同一个逻辑文件时改用 `data_format=dmp`，并先执行 `dimp SHOW=Y` 与 `CTRL_INFO=4`。

## 当前边界

- 当前页大小/字符集矩阵仍只来自上述一个 DM9 build；4 KiB 页、更多 DM9 build、DMASM
  和 DSC 仍需增加冷快照验证。该 build 不支持 64 KiB 数据页。
- DM9 向量索引的高级构建参数尚未离线恢复，导入后需要复核。
- DMP 不能无损保存 `TIME` 小数秒。
- 动态性能视图与系统包的版本差异不用于离线 bootstrap，也不能代替物理页验证。
- 损坏页、断链和未提交事务仍遵循项目通用恢复边界，不保证得到数据库一致性读。
