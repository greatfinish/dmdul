# Changelog

所有重要变更都会记录在这里。

版本格式遵循：

```text
v主版本.次版本.修订版本
```

当前项目仍处于早期可用和持续验证阶段，离线恢复结果受达梦版本、页大小、字符集、表类型、行格式、数据页损坏程度等因素影响。

------

## v0.9.0 - DM9 Compatibility

### Added

- 新增 DM9 VECTOR 行值与 DDL：支持 FLOAT32、FLOAT64、INT8、BINARY 和稀疏 FLOAT32，
  并恢复 HNSW/IVFFLAT 用户索引的基本组织类型。
- `unload user` 覆盖用户拥有的全部模式；额外模式的表使用包含模式名的防冲突输出文件名。
- 新增 DM9 兼容性实测文档，记录 UOS、32 KiB、UTF-8 冷快照的对象矩阵、三通道回灌
  证据和已知边界。

### Fixed

- 修复 32 KiB 数据页中记录头跨 4 KiB 保护边界时每个受影响边界漏一行；LOB 和 Long Row
  页也按页类型恢复保护字节。
- NOBRANCH page plan 解析 root 中的全部分支描述并合并 leaf/next 链，不再只读取第一支。
- DM9 普通表标志位不再被旧 HUGE 启发式误判；只有 `$AUX` 等明确证据存在时才进入 HUGE
  数据路径。
- 过滤 HNSW/IVFFLAT 自动生成的内部表和触发器，避免作为用户对象导出。
- DMP 主键改为内联 `CREATE TABLE`，与官方 `dexp` 元数据布局一致，确保外键应用前引用
  主键已经存在。
- SQL 超长语句告警采用 DM9 实测的 2499 字节可移植 stdin 下限；超过限制时提示改用
  dmfldr/DMP，避免 `disql` 返回 0 但静默跳过记录。
- 页检查先恢复可证明存在的保护尾区再做结构校验，消除干净 DM9 SYSTEM 页误报。
- 修复 GB18030/EUC-KR 短内联 `TEXT/CLOB` subtype `0x03` 未识别导致整行解析失败。
- dmfldr 控制文件统一按实际 UTF-8 输出声明 `CHARACTER_CODE`，不再把源数据库字符集误当
  成输入文本编码。
- 包规格与包体按 SQL 对象类型优先匹配；16/32 KiB 字典页中较长包体不再覆盖包规格。

### Tests

- UOS DM9 `03151060506-20260417-322930-20218` 冷快照完成 20 表、93 字段、程序对象、
  三类分区、LOB、Long Row、HUGE 和五种 VECTOR 的离线恢复；11335 行、0 失败、45 个
  计划页全直读、0 fallback、SYSTEM 预检 0 坏页。
- dmfldr 回灌后普通表、HUGE、分区、LOB/JSON 和向量差异均为 0。
- DMP 的 `SHOW/CTRL_INFO=4` 通过；新建 DM9 实例导入返回码 0、无警告、11335 行齐全、
  0 个无效对象，除已知 TIME 小数秒限制外 10006 条规范化标量/向量记录与源库一致。
- 新增 8/16/32 KiB × GB18030/UTF-8/EUC-KR 的 3×3 冷快照矩阵；每组 3027 行、0 失败、
  0 坏页、0 fallback，dmfldr 清空回装一致。8 KiB/GB18030、16 KiB/EUC-KR 和
  32 KiB/UTF-8 的 OWNER DMP 真导入均返回 0、无警告、无无效对象。
- 实测确认该 DM9 build 拒绝 `PAGE_SIZE=64`；64 仅作为 `EXTENT_SIZE` 页数纳入验证。

------

## v0.8.0 - HUGE Column-Store Recovery

### Added

- bootstrap 识别 HUGE 主表及 `$AUX/$RAUX/$DAUX/$UAUX` 内部对象；磁盘字典
  `tables.tsv` 升级到格式 3，持久化 HUGE 标志、SECTION、FILESIZE、WITH DELTA 和四个
  辅助表 ID，同时继续兼容旧格式字典。
- DDL 生成 `CREATE HUGE TABLE`，恢复 SECTION、FILESIZE、WITH/WITHOUT DELTA 和目标
  混合表空间；`describe` 增加 HUGE 存储参数与辅助对象定位信息。
- 数据卸载通过 `$AUX` 定位 HFS 列 section，按 section 流式读取列文件，并合并 `$RAUX`
  尾部行、`$DAUX` 删除范围和 `$UAUX` 更新值。统一逻辑行可输出 SQL、dmfldr 或 DMP。
- 卸载日志增加 HUGE 表数、读取 section 数和 HFS 文件数。

### Fixed

- `SYSINDEXES.KEYNUM=0` 且可空 `KEYINFO` 物理省略时，不再丢失 keyless table-data storage
  root；这同时减少普通堆存储和 HUGE RAUX 的全文件 fallback。
- 同一 HUGE 辅助对象残留多个历史 storage root 时选择最新目录 ID，避免 ALTER/重建后读取
  旧 `$AUX` 或 `$RAUX` 页面。
- 系统字段类型名跨页保护边界损坏时，按原始短字符串长度和仍完好的 ASCII 位置做确定性修复，
  不再把 `VARCHAR` 等类型写成乱码。

### Safety

- 在写出第一行前预检目标表全部 section。压缩 `CPR_FLAG != N`、加密
  `ENC_FLAG != N`、未验证的可空定长列或类型会明确失败，不生成部分成功的逻辑数据。
- 校验 HFS section 的 4 KiB 对齐、magic、`N_LEN`、文件边界、列行数一致性和变长 offset 表。
- `$UAUX` 改用紧凑 `(row_id,col_id)` 键，限制唯一更新数和累计值字节；单 section 变长列
  offset 表也设置 256 MiB 上限，避免损坏元数据或超大 delta 导致内存失控。

### Tests

- 新增 HUGE 对象链接、keyless SYSINDEXES、类型名修复、DDL、字典 TSV 往返、最新辅助
  storage 选择，以及合成 HFS `INT/VARCHAR/NULL` section 单元测试。
- ARM64 DM8 一致性快照实测 HFS 主 section 与 RAUX/DAUX/UAUX 合并；SQL 回灌双向
  `MINUS=0`。表级 DMP 经官方 `dimp REMAP_SCHEMA` 导入 1499 行，与同快照 SQL 回灌表
  双向 `MINUS=0`。压缩 DECIMAL 样本按预期在输出前拒绝。
- 补充 RAUX-only 100 行和两个完整 HFS section 加 RAUX 的 2500 行实机样本；SQL 回灌
  双向 `MINUS=0`。RAUX-only 表经 dmfldr 装载 100 行、0 拒绝，双向 `MINUS=0`。

### Documentation

- 新增 `docs/huge-tables.md`，README、标准恢复流程、存储格式差距分析和路线图同步说明
  HUGE/HFS 文件准备、实现路径、验证证据与当前安全边界。

------

## v0.7.3 - Two-Stage Page Diagnostics

### Added

- `bootstrap` 在字典读取前自动执行 SYSTEM.DBF 纯物理页预检，覆盖文件对齐、页头、
  PAGE_CHECK 和行页结构，并同时支持文件系统文件与 DMASM 逻辑 SYSTEM 文件。发现损坏时继续
  尝试字典恢复，以 `SUCCESS_WITH_WARNINGS` 结束并提示执行第二阶段全库 `check pages;`。
- `check pages` 成功后在 `output` 目录生成 `check_summary.md`、全量
  `check_bad_pages.tsv` 和 `check_affected_objects.tsv`。报告包含绝对字节偏移、三类损坏、
  已归属/未归属页、受影响物理对象和表、已知表段头命中数、损坏字节比例、归属依据及置信度。
- 新增 `TABLE`、`TABLE_ASSIST`、`UNATTRIBUTED` 保守对象分类。主/辅助 storage_id 归属为
  `HIGH`，唯一段范围回退为 `MEDIUM`；字典证据不足时不猜测 INDEX、LOB 或分区类型。
- 新增全量坏页同步回调。终端仍按每文件 4096 条限制保留内存明细，TSV 明细和影响统计不受
  该限制，适合大规模损坏文件。

### Fixed

- 独立 `check pages` 不再隐式加载目录中残留的 `dmdul_dict`。对象归属只采用当前会话经
  `bootstrap` 或显式 `load dictionary` 建立的字典，避免跨快照错误归属。
- 修复坏页超过每文件明细上限后 `HEADER_INVALID`、`CHECKSUM_FAIL`、
  `STRUCTURE_INVALID` 分类计数被低估的问题。分类计数现在与坏页总数一样按全量页面累加。
- 同一 storage_id 被多个字典对象声明或多个段范围重叠时，不再静默归属到首个对象；报告会
  标记 `ambiguous_storage_id` 或 `ambiguous_segment_range`。

### Tests

- 新增全量回调数量、明细截断后精确分类/影响统计、主 storage/辅助 storage/段范围归属、
  歧义拒绝，以及三份报告重复覆盖生成测试。
- 新增 bootstrap 在字典恢复前执行 SYSTEM 物理预检、DMASM 逻辑 ReaderAt 预检，以及独立
  `check pages` 不自动加载残留字典的回归测试。

### Documentation

- README、标准恢复流程和 PAGE_CHECK 实验文档补充坏页影响报告、字段含义、内存边界和证据限制。
- 明确两阶段恢复顺序：`bootstrap` 自动完成必需的 SYSTEM.DBF 纯物理预检；只有预检告警、
  整库/用户/模式恢复、介质可疑或需要审计报告时，才继续执行第二阶段全库 `check pages`。

------

## v0.7.2 - DMASM Multi-Database Discovery

### Added

- `set asm_disk` 现在直接扫描全部已配置磁盘组的 INODE 目录并发现 `SYSTEM.DBF`。每个候选
  都按数据库分组显示数据库名、字符集、页大小、页数、簇大小、大小写标志，以及与该库对应的
  ASM 数据文件路径表；同一磁盘集合包含多个数据库时会全部展示。
- 唯一的 `SYSTEM.DBF` 候选自动设为活动 `system` 并持久化到 `init.dul`；存在多个数据库时
  只展示和核对，不自动猜测活动数据库，仍要求用户执行 `set system <ASM path>;`。
- ASM 数据库发现结果会持久化到 `dmdul_dict/asm_databases.tsv` 和
  `dmdul_dict/asm_datafiles.tsv`。前者保存候选数据库身份、参数与当前选择状态，后者按候选关联
  保存每个数据库的完整 DBF 集合；启动、`load parameter`、`set system` 切换和 bootstrap
  字典重建都会同步刷新。切换到文件系统 `SYSTEM.DBF` 时仅清除 ASM `selected` 标记，不删除候选证据。

### Changed

- `list asmfile` 不再要求先执行 `set system`，可在 bootstrap 前独立列出 ASM 文件目录和
  `SYSTEM.DBF` 候选；`list datafile` 也会在唯一候选尚未选择时自动完成选择。
- 多库 ASM 数据文件归属优先采用各库 `dm.ctl` 中的精确 ASM 路径；目录后缀仅在唯一候选时
  跨磁盘组回退，避免两个数据库使用相同 `data/DAMENG` 目录时串库；`dm.ctl` 缺失或损坏时
  按受限目录/磁盘组规则安全降级，不会阻断裸盘发现。

### Documentation

- README 推荐恢复流程和标准恢复作业手册改为“文件系统 DBF / DMASM 成员裸盘”双入口，补充
  一致性快照、唯一/多候选选择、ASM 目录证据、旧字典隔离、预检和 bootstrap 顺序。

### Tests

- 新增 ASM SYSTEM 候选自动选择与多候选拒绝猜测、候选数据库/数据文件 TSV 原子持久化、
  bootstrap 字典目录替换后目录证据保留、文件系统切换清除选择状态，以及多库同目录隔离测试。

------

## v0.7.1 - DMASM Logical File Copy

### Added

- 新增交互命令 `cp <+GROUP/path/file> <filesystem file|directory>;`，把 DMASM INODE 中的
  单个逻辑文件流式复制到普通文件系统。
- 新增 `cp datafile <directory>;`，一次复制当前 ASM 数据库目录中识别到的全部 DBF，
  包含分布在不同磁盘组中的 SYSTEM、ROLL、MAIN、TEMP 和用户表空间文件。
- ASM 文件复制使用 4 MiB 有界缓冲、同目录临时文件、落盘同步和原子改名；默认拒绝覆盖
  已有文件，并在批量写入前检查目标文件名冲突。控制台与 `dul.log` 记录字节数、SHA-256
  和耗时。

### Tests

- 新增 ASM 跨 extent 文件复制、精确字节与 SHA-256、已有目标保护、读取失败临时文件清理、
  Windows 带空格路径和批量同名冲突测试。

### Documentation

- 重绘 README 能力链，明确区分“离线文件系统 DBF 直读”和“DMASM 成员裸盘 -> 磁盘组元数据
  -> ASM 逻辑 DBF”两条输入路径，并补全 `disql`、`dmfldr`、`dimp` 三种官方回灌工具。
- README、使用示例和 DMASM 专题文档新增 `cp` 单文件/整套 DBF 复制流程、安全边界及切换回
  文件系统恢复路径的步骤。

------

## v0.7.0 - DMASM Raw Recovery

### Added

- 新增 DMASM 裸盘只读恢复路径。`set asm_disk <member>[,<member>...];` 配合
  `set system +GROUP/.../SYSTEM.DBF;`，可直接从离线成员盘解析 ASM 逻辑文件，无需
  DMASMSVR、DMASMAPI 或 `asmcmd cp`。
- 新增 `list asmfile;`，显示 INODE 中恢复的 file id、逻辑大小、extent 数、INODE
  物理地址和完整 ASM 路径；`list datafile;` 同时支持 ASM 中的 SYSTEM、ROLL、TEMP、
  MAIN 和用户表空间 DBF。
- 新增 DMASM 非镜像环境只读解析器：支持 32 MiB 保留区、1 MiB AU、4 MiB extent、
  512 字节 INODE、32 字节 XDESC 链及跨成员盘地址；链成环、缺盘、错类型、未对齐和
  越界地址均会拒绝。
- 新增镜像版 DMASM `0x3001` 只读解析器：解码动态 AU 大小、磁盘/组头、512 字节
  INODE、32 字节 AU 描述符、`prev/next` 链、双副本数组和 16 位 logical AU sequence。
  已支持 1/4/32 MiB AU、EXTERNAL/NORMAL/HIGH、0/32 KiB 条带；NORMAL/HIGH 按显式
  copy 数组定位副本，某个副本发生 I/O 错误时自动尝试下一个副本。
- 新增镜像版 AU 1 group-header generation 和 AU 2 成员表解析。成员记录包含状态、
  disk id、AU 数、failure group 与磁盘名；普通读取只使用当前 NORMAL 成员。
- 新增多磁盘组联合读取。一次 `asm_disk` 可以同时配置多个组的成员，解析器按磁盘头
  group id 分组，按 AU 1 组名路由 `+GROUP/...`，并从相同数据库目录发现跨组 DBF。
- 镜像版 `.inode` 支持跨多个逻辑 AU 合并目录；AU=32 MiB 时仍按 1 MiB 分块扫描，
  不会一次分配整个 AU。Windows 可直接读取 VMware `monolithicFlat` 的 `*-flat.vmdk`。
- 镜像版大成员盘支持多个 descriptor 区域。解析器按 `AU_SIZE/32` 条描述符分段读取，
  不再要求整块成员盘的 AU map 全部容纳在 AU 0。
- `init.dul` 持久化 `asm_disk`；SQL、dmfldr、DMP 的表级、用户级和整库卸载均可复用
  ASM reader 与现有 page plan。
- bootstrap 会把字典引用的 `(group_id,file_id)` 与实际离线 DBF 集合核对。缺少成员或
  数据文件时输出 `missing_files`，最终状态为 `SUCCESS_WITH_WARNINGS`；DDL 仍可恢复，
  但受影响表不会再被误解为已确认的空表。
- ASM 字典的 `tables.tsv` 通过 storage root 和 leaf chain 生成
  `header_file/header_block/bytes/blocks/extents` 段摘要。该过程只读计划页，不扫描整个
  ASM 逻辑数据文件；普通文件模式对未解析表继续保留流式段扫描补充。
- 新增 [DMASM 裸盘离线读取与恢复](docs/dmasm-raw-recovery.md)，记录物理结构、公式、
  操作流程、安全边界和 DMDSC 实机证据。
- DMASM 文档按官方定义区分非镜像环境的 DESC/INODE/DATA 簇与镜像环境的描述/INODE/数据 AU，
  补充 DMASM REDO、超 65535-AU 文件、早期 64 GiB DESC 边界和残留恢复缺口。
- 新增 DMASM 磁盘逻辑结构与离线 ASM 文件定位图，提供可编辑 SVG 和 README/专题文档
  使用的 PNG，展示两代存储模型、四类关键内容及完整裸盘定位链。

### Fixed

- 镜像版 AU 描述符从物理 AU 8 开始；AU 0 前 256 字节属于磁盘头。排除
  `file_id=0xFFFE/0xFFFF` 分配器保留项，避免把管理字段误判为普通文件副本链。
- 镜像版 INODE 逻辑大小按 `0x118` 起的 7 字节小端单位值乘 256 解析，修复大于
  256 MiB 文件的错误尺寸；完整 file id 同时纳入 group id 和 AU 大小编码。
- 镜像版 INODE logical AU 数改为读取 `+0x103..+0x104` 的未对齐小端 `uint16`。
  旧逻辑按 `+0x100` 大端读取只在 0 至 255 AU 时碰巧正确，会把 33000 AU 截断为 232。
- 镜像版 INODE 副本数改为读取 `+0x113` 单字节。`+0x110` 是独立分配属性，部分 DBF
  会形成 `0x01000001` / `0x02000002`；此前按大端 u32 读取会把单/双副本误报为
  16777217/33554434。
- NORMAL 副本不再只依赖“扫描两个成员后按 file/sequence 聚合”的推断。解析器现在
  显式消费描述符 copy 数组，并在成员存在时交叉校验副本、前后链和 logical sequence。
- 修复 AU 描述符 sequence/健康位边界。logical sequence 位于 `+0x1B..+0x1C`，
  `+0x1D` 是副本失败位图；此前把健康位并入 sequence 会产生 131073 等伪序号并中断
  OFFLINE 副本读取。
- 修复 REPLACE 后旧成员重新出现时被误聚合为第三副本的问题。解析器选择最高 generation
  的组头副本，从 AU 2 成员表排除 OFFLINE/RECONNECT、DELETED、名称或 AU 数不匹配的盘；
  同 generation 的多个成员表副本不一致时拒绝继续。
- 修复 32 KiB 用户数据页的扇区保护尾解析。保护页尾部保留 40 字节，旧逻辑按固定
  8 字节尾计算会把 slot 目录错移 32 字节，并让跨 4 KiB 边界的字段混入保护值。现在
  根据 `n_slot/n_rec/free_end` 和可解码行头选择布局，再恢复七组边界原字节。

- Standard Bootstrap 的 SYSTEM page cache 现在继承逻辑 reader。ASM 模式可以完整走
  page 0 anchor、storage root 和 leaf chain，不再因把 `+DMDATA/...` 当成本地路径而
  回退全 SYSTEM 流式扫描。
- 收紧 PAGE_CHECK 哈希尾部的 slot 布局判定。32 KiB 页中，错误的固定尾部候选即使
  “大部分 slot 合理”也不会提前胜出，修复 SYSCOLUMNS 页错移 32 字节后漏用户表的问题。
- DMASM 中 `TEMP0.DBF` / `TEMP1.DBF` 正确映射为 group 3 的 file 0/1。
- 单字节 `CHAR(1)` / `CHARACTER(1)` 按 DM 行格式的固定长度列解析，宽 `CHAR` 继续使用
  变长编码。修复固定区后续 DATE/DATETIME 错位并导致整行跳过的问题。
- SYSTEXTS 增加标准数据行解析：支持大端行长度、2-bit metadata、固定 `ID/SEQNO` 和
  `0x02` / `0x04` 内联 CLOB 封装，同时保留旧偏移布局回退。修复 32 KiB 页环境中已找到
  VIEW 对象但正文为空的问题。
- 恢复出的 VIEW 源码即使省略模式名，也会按字典 owner 重写为 `owner.view_name`，避免
  以 SYSDBA 执行对象脚本时把视图建进错误模式。
- 同一离线输入出现重复 `(group_id,file_id)` 时不再静默采用先扫描到的文件，而是拒绝
  bootstrap/unload 并同时报告两个逻辑路径，防止跨库或重复快照混入。
- 镜像磁盘头按官方范围校验 AU 大小，只接受 1/2/4/8/16/32/64 MiB；磁盘几何使用
  溢出检查。非镜像 INODE 重复项按物理地址稳定选择，相同路径元数据冲突时拒绝继续。
- 清理流式扫描和 dmfldr 改造后不再可达的旧整文件扫描、CSV 渲染及兼容包装器；最新版
  `staticcheck` 全量检查无告警。

- 同步 `dmdul help` 的交互命令摘要，补齐 `list datafile`、`list asmfile`、`list schema`、
  `describe`、`unload schema`、`check pages` 和 `set asm_disk`；参数错误提示明确 `csv` 是
  `fldr` 的历史别名。

### Security

- 文档中的实机口令改为 `<PASSWORD>` 占位符；虚拟机配置、虚拟磁盘、内存与快照文件
  加入 `.gitignore`，避免宿主路径、MAC、UUID 或大型实验盘进入仓库。

### Verified

- 在双节点 DMDSC 测试环境的 `/dev/asmdisk/dmdata01` 上直接恢复 256 MiB
  `SYSTEM.DBF`（64 extents、32 KiB 页、8192 页），Standard Bootstrap 约 6 秒完成。
- 直接从 `DMTEST_TS01.DBF` 用户级抽取 3 张表：planned pages 3、direct pages read 3、
  fallback 0；`T_DEPARTMENT`、`T_EMPLOYEE`、`T_SALES_ORDER` 共 13 行零失败，DDL、中文、
  `CHAR(1)`、DATE、DATETIME、DECIMAL、注释和数据与在线查询逐值一致。
- 对象级抽取恢复 DMTEST 用户、3 张表、2 个序列、1 个完整视图、2 个索引、12 个约束和
  3 条表注释；视图模式名与字典 owner 一致。
- 镜像版六个确定性文件覆盖 EXT4/NORM4/EXT32、1/4/32 MiB AU、EXTERNAL/NORMAL
  和 0/32 KiB 条带。文件头、条带边界、AU 边界、4-AU group 边界、中点和尾页的
  4 KiB 标签、逻辑偏移及填充值全部与写入程序一致。
- 完成双节点镜像 DMDSC 冷快照验证：停止 DB/ASM/CSS 和两台 VM 后复制 8 条 VMDK 链，
  共 160 GiB；16 个源/目标文件 SHA-256 全部一致，8 条 VMDK 链检查通过。
- Windows 直接从冷快照的 5 个 `*-flat.vmdk` 完成 Standard Bootstrap：发现 3 个磁盘组、
  5 个成员、55 个 ASM 文件和 8 个 DBF，约 6 秒恢复 1097 个对象、2 个用户、5 张表、
  33 列、1 个视图、3 个序列、2 个例程和 1 个触发器。
- ASMTEST 对象 DDL 完整生成；4 张表导出 14 行，planned/direct pages 为 4/4，
  fallback 0、失败 0。在线只读基线与冷副本生成的 9 个 DDL/数据文件 SHA-256 全部一致。
- LAB03-HIGH 使用四成员、四 failure group、4 MiB AU 和 HIGH 三副本。两个 256 MiB
  确定性文件覆盖 coarse/32 KiB 条带，131072 个 4 KiB 页全量校验通过，每个逻辑 AU
  都恢复出三个物理副本；裸盘重建的真实 DBF 与官方复制文件逐字节一致。
- HIGH 表空间的 `ASMTEST.T_HIGH_COPY` 在线裸盘卸载 20000 行、失败 0，351 个计划叶子页
  全部直接读取、fallback 0；行号、条带标记和 512 字节 payload 逐行校验一致。
- LAB03 完成 12 条 VMDK 链冷快照：24 个源/副本文件共 223338305041 字节，SHA-256
  差异 0，12 条链均为 consistent。Windows 裸盘 Standard Bootstrap 发现 5 个磁盘组、
  12 个成员、73 个 ASM 文件和 9 个 DBF；冷副本同样导出 20000 行、失败 0，数据 SQL
  与在线结果 SHA-256 完全相同。验证后两节点均恢复 OPEN，在线统计保持不变。
- LAB04 完成 NORMAL 组 ADD/REBALANCE/OFFLINE/RECONNECT/REPLACE 差分：短窗口重连为
  `RESYNC 71/71`，替换为 `REPLACE 932/932`，旧 disk 3 变为 DELETED，备用盘获得
  disk 9。14 个 512 MiB 确定性文件在每个静态阶段均保持双副本。
- LAB04 在替换后同时输入 A/B/旧D/E/R，修复前稳定复现“每个 AU 三副本”；修复后只
  采用当前 `0/1/8/9` 成员，7 GiB 数据逐 4 KiB 页校验通过。
- LAB05 使用 80 GiB 稀疏盘、EXTERNAL、AU=1 MiB 创建 64 个子目录和 5200 个真实小文件，
  恢复 5270 条最终目录记录。`.inode` 跨 physical AU 8/2117/4166，2048 与 4096 条记录
  两个边界前后的文件均可打开。
- LAB05 使用 33000-AU 和 28000-AU 两个大文件真实跨越 AU 32768、65536 的第二、第三
  descriptor 区域；官方检查均为 `0 mirs err`，dmdul 在全部 DB/ASM/CSS 进程停止后
  恢复 33000/28000 条 AU map，并成功读取跨区位置与文件尾。
- LAB05 的 80 GiB VMware 稀疏成员盘在约 61 GiB DMASM 逻辑文件分配后，宿主实际分配
  仅 137.0625 MiB；保留完整 allocated-range 证据，确认大规模元数据样本未占满宿主盘。
- 使用 15 个离线镜像成员完成跨 EXT4/NORM4/EXT32/HIGH4/RBLN4 的端到端回归：
  Standard Bootstrap 为 `SUCCESS`，7 张表均生成段摘要；`ASMTEST.T_EMPLOYEE` 6 行通过
  1 个计划页直接读出，fallback 0、失败 0。
- 仅提供 NORM4/EXT32 的不完整成员集时，bootstrap 准确报告缺少的 5/0、8/0、9/0，
  状态为 `SUCCESS_WITH_WARNINGS`；尝试卸载缺失组中的表时明确报告目标数据文件不可用。

------

## v0.6.6 - CLI Output Polish

### Fixed

- 生成的 dmfldr 控制文件里给出的装载命令改为 `CONTROL="'x.ctl'"`。此前写的是
  `CONTROL='x.ctl'`,照抄到 shell 里必然失败:dmfldr 拒绝解析含 `.` 的未加引号参数值
  (报 `parameters parse error[...]`,方括号里是被点号截断后的前半截),而 POSIX shell
  会把裸单引号吃掉,dmfldr 根本看不到引号。单引号必须用双引号包住才能送达。
  实测矩阵还确认:**不含点号的值反而不能加引号**,`DIRECT='TRUE'` 本身就是 parse error;
  正确加引号后 `DIRECT=TRUE` 可正常工作。README、`docs/usage.md`、
  `docs/recovery-workflow.md` 同步订正。

### Changed

- `list user` / `list table` / `list schema` / `list datafile` 的表头改为**大写列名 +
  下划线规则行**(对齐 disql / sqlplus 的结果集渲染),一眼能分清哪行是列名、哪行是数据。
  列宽改为按实际内容自适应(此前是写死的 22/34/28 字符,长用户名和长路径会被挤在一起,
  短的又留大片空白),最后一列不再补齐,输出不带尾随空格。
- bootstrap 诊断行的标签由 `[BOOTSTRAP]` 改为小写 `[bootstrap]`,与其余输出的观感一致。

------

## v0.6.5 - Importable Large-Table DMP

### Fixed

- **修复超过 8 MiB 的表 DMP 无法被 `dimp` 导入**。DMP 数据段按 8 MiB 切 phase,此前是
  在精确 8388608 字节处硬切,会把一行数据劈成两半跨 phase。`dimp` 读完第一个数据
  phase 后就在半行处报 `[WARNING]data abnormal, import fail...` 并放弃整张表——1000 万行
  的表只导进 47113 行(正好等于第一个数据 phase 的 rows 字段)。
  对照官方 `dexp` 的多 phase 转储发现,它的 phase 长度总是**略大于** 8 MiB
  (实测 8493595 / 8388757 / 8540709,限额 8388608),即填到限额后把当前行写完才收尾,
  一行永不跨 phase。改为同样语义:phase 边界只在行与行之间判定,行内不再分片,
  随之删除跨 phase 续行的 `0xFFFFFFFF` 标记机制。
  修复后同一张 1000 万行表 `dimp` 报 `terminate import success without warning`,
  30 秒导完 10000000 行,`SUM(ID)`/`MIN`/`MAX` 与源表一致。
  此前只用 `dimp SHOW=Y`(仅解析元数据、不导数据)验证过大表,因此漏检;
  回归测试改为断言"行不跨 phase、phase 长度 overshoot 限额"。

### Added

- 新增[离线恢复标准流程](docs/recovery-workflow.md):从取一致性快照、预检、bootstrap、
  盘点对象、选通道(SQL / dmfldr / DMP)、卸载、回灌到隔离测试库、双向 MINUS 校验的
  完整作业手册,含常见坑对照表和实测性能参考。千万行级的表不要直接双向 MINUS——
  实测能把一台 4 GB 的 DM 实例 OOM 掉,应按主键分块比对。
- `docs/development.md` 的发布构建示例补上 `-s -w`。历史发布包都带这两个 flag,
  文档漏写导致 v0.6.4 的包比 v0.6.3 大了近一倍(压缩包 1.9 MB → 3.4 MB),
  而代码本身只增长 0.5%。同时清掉引用了并不存在的 `build.ps1` 等陈旧内容。

------

## v0.6.4 - dmfldr Loadable Export

### Changed

- **CSV 输出改为 dmfldr 分隔文本 + 每表控制文件**。`set data_format fldr;`(`csv`
  作为历史别名保留)现在生成 `<owner>_<table>_data.txt` 和配套的
  `<owner>_<table>_data.ctl`,可直接交给官方 dmfldr 装载:

  ```bash
  dmfldr USERID=SYSDBA/password@127.0.0.1:5236 CONTROL='HR_TEST_EMP_INFO_data.ctl'
  ```

  控制文件里的分隔符、NULL 标记、字符集和 BLOB 编码都按数据文件写死。所有规则都是
  在 DM8 实例上用 `/dm8/bin/dmfldr` 实测确定的,与文档写法有出入的地方以实测为准:

  - `BLOB_TYPE = 'HEX_CHAR'`。用 `'HEX'` 会把十六进制字符本身存进 BLOB,长度翻倍。
  - `NULL_MODE = TRUE` + `NULL_STR`,NULL 写作 `\N`;空字段因此表示空字符串,NULL
    与空字符串可区分。
  - 不输出 `ENCLOSED BY`(语法错误)和列级 `FORMAT`(精度对不上就整表失败);所有恢复
    类型在 dmfldr 默认解析下都能正确装载。
  - 分隔符按列类型选:列类型不可能产生 `|`/CR/LF 时用可读的 `|` + LF,只要有字符类型
    列就改用 SOH(`0x01`)分隔、STX+LF(`0x02 0x0A`)换行。dmfldr 既无可用包围符也不做
    反转义(`ESCAPED BY` 实测不反转义),可打印分隔符无法与列内容区分;dmdul 从不在
    字段值里写 C0 控制字符,因此这组分隔符不会与数据冲突。
  - 时间类型小数秒截到 6 位:dmdul 按列声明精度渲染,DM 最多存 6 位,dmfldr 会直接
    拒绝更长的小数秒(TIMESTAMP(9) 因此曾整列装载失败)。
  - 命令行参数值含 `.` 或 `-` 时必须加单引号,生成的控制文件注释里已给出正确写法。

  实机验证:DULTEST 9 张表 53064 行经 dmfldr 装载后逐表 `MINUS` 双向比对差异为 0,
  覆盖 152 KiB CLOB、128 KiB BLOB、JSON、含换行与竖线的文本、13 种 INTERVAL、
  TIMESTAMP(9) 和带时区时间戳,dmfldr 报告 0 失败行;1000 万行 `T_CUSTOMER_MOCK`
  卸载耗时 92 秒、0 失败行。

### Added

- 新增 `describe <owner.table_name>;`(别名 `desc`,借鉴 DUL 的 `desc owner.table`):
  打印单表的恢复定义与**物理位置**——table_id、表空间/组号、storage_id、B 树 root
  (file#/page#)、段头(file#/block#)、块数/簇数、存储属性(含 CLUSTERBTR /
  USING LONG ROW / 分区 / 临时表)、`assist_ids`,以及完整列清单(列号、名称、
  类型长度精度、可空、默认值)和分区明细。恢复前确认"表被定位到了、数据在哪"。
- `unload` 逐表打印导出行数(借鉴 DUL 的 `. unloading table NAME N rows unloaded`):
  多表卸载时每张表单独显示 `. unloading table OWNER.TABLE  N rows unloaded`,
  有失败行时附加 `(N failed)`,不再只有最后一个总计。

### Fixed

- SQL DDL 现在为**非同名附加模式**输出 `CREATE SCHEMA ... AUTHORIZATION`。达梦中
  用户与模式是一对多关系:建用户自动创建同名默认模式,但附加模式(`CREATE SCHEMA
  x AUTHORIZATION user`)此前不会被 DDL 重建,导致多模式用户的 SQL DDL 恢复到新库
  时,附加模式的 `CREATE TABLE schema.tbl` 因模式不存在而失败。修复后每个非同名
  模式以 `/` 批终结符输出(DM 的 CREATE SCHEMA 会吞掉后续语句直到 `/`,实测仅用
  `;` 会导致模式建不成)。DMP 路径本就正确处理多模式,不受影响。实机 SCHTEST
  用户(含 SCHTEST_EXTRA 附加模式)SQL DDL 干净往返、模式与表全部重建。

------

## v0.6.3 - Recovery UX & Offline-First Resolution

### Changed

- 数据文件解析全局改为**以用户放置的离线文件为准**:`data_dir`(默认可执行文件
  同目录)里的同名文件优先于 `dm.ctl` / `control.dul` 记录的(常为绝对)路径,
  后者降级为映射参考,仅当本地无该文件时才回退使用。这样在与线上库同机恢复时,
  不会因 dm.ctl 存的原始绝对路径而误读线上原文件——`bootstrap` / `unload` /
  `recover` 与 `list datafile` 全部读用户的离线副本。(此前仅 `check` 做了
  data_dir-only;现在对所有命令一致。)

### Added

- 新增 `list datafile;` 命令(借鉴 DUL 的 `show datafiles`):列出已识别的所有
  数据文件及其表空间、组/文件号、页数、大小和读取状态(OK / UNREADABLE /
  SIZE?),无需先 bootstrap。恢复前的预检查——一眼确认文件都被正确识别、
  没有读不到或被截断的。
- 启动与 `set system` 后自动探测并打印一行数据库身份(借鉴 Oracle DUL 的
  "Found db_name = ..."):`detected: db_name=... instance=... page_size=...
  pages=... charset=... case_sensitive=...`,让用户立刻确认"打开的是对的库",
  无需先 bootstrap/show parameter。仅读 SYSTEM.DBF 文件头 + 可选 dm.ctl/ini,
  不扫字典,几乎零耗时。
- 默认从**可执行文件同目录**读取 `SYSTEM.DBF` 和数据文件(其次当前目录):默认
  `system` 路径与派生的 `data_dir` 都指向 dmdul 所在目录,把离线文件放在 dmdul
  旁边即可启动直接 `bootstrap`,无需任何 `set`。启动时自动探测该位置——找到则设
  好 `system`/`data_dir` 并打印身份;找不到则打印一行提示,引导手动
  `set system` / `set data_dir`。

------

## v0.6.2 - USING LONG ROW DDL Restoration

### Added

- DDL 输出 `STORAGE(USING LONG ROW)`:逆向确认 `SYSOBJECTS.INFO3` 的 **bit 50**
  即 USING LONG ROW 存储标志(用列相同、只差该存储选项的最小对照表 diff 出单 bit,
  并跨 9 张表验证)。字典现捕获该标志,恢复宽行表时 DDL 自动带
  `USING LONG ROW` 子句,不再需要手工补(即解除 v0.6.1 的已知限制)。
  bootstrap 与 `load dictionary` 两条路径均生效。
- 实机 T_LONGROW(3×VARCHAR(4000),12000/11700 字节两行)DMP 往返验证:
  `dimp` 用导出 DMP 自动建 `USING LONG ROW` 表并导入,MINUS 双向比对完全一致。
  (注:SQL 格式的超宽行受 disql stdin 每行 2499 字符限制无法直接导回,大宽行
  表应使用 DMP 通道——本就是推荐做法。)

------

## v0.6.1 - Long Row & Safe Check Paths

### Fixed

- 修复 `STORAGE(USING LONG ROW)` 宽行卸载:实测发现达梦对超过约半页(8K 页约
  4000 字节)的行外 VARCHAR/CHAR 列,会溢出到**常规 LOB(0x20)页**而非长行
  (0x22)页。此前普通 VARCHAR 行外路径只搜 0x22 长行页,导致这类列所在的宽行
  整行卸载失败(实测 12000 字节行失败、11700 字节行成功)。改为先试 LOB(0x20)
  再试长行(0x22),两种溢出形态都能正确读出。实机 T_LONGROW(3×VARCHAR(4000))
  两行全部导出,内容逐字节与源表吻合(各列纯净单字符、长度 4000/3900 精确)。

### Known Limitation

- 恢复 `STORAGE(USING LONG ROW)` 表时,导出的 DDL 暂不含 `USING LONG ROW` 子句
  (字典尚未捕获该存储标志),建出的是普通表。宽行**数据可正确提取**,但导回前
  需手工给建表语句补 `STORAGE(USING LONG ROW)`,否则 DM 以超长记录拒绝。

### 调研结论(v0.6.x 方向澄清)

- 实测确认**达梦不存在 Oracle 式的行迁移(row migration)和行链(row chaining)**:
  UPDATE 撑大行是整行重新放置(无转发指针),普通超大行直接以 `[-2665]` 拒绝
  (最大 in-row 记录约半页)。超大数据走 LOB(行外 0x20,已支持)或
  `STORAGE(USING LONG ROW)`(长行 0x22)。路线图「迁移行/链式行支持」据此
  澄清为「Long Row 宽行支持加固」,即本次修复。

### Changed

- `check pages` 默认只在 `data_dir` 内按页头识别定位 DBF 文件,不再跟随
  `dm.ctl` / `control.dul` 的绝对路径。此前若原库文件仍在原位置,check 会读到
  线上原文件而非 `data_dir` 里的离线副本(表现为坏页数为 0)。需要跨目录跟随
  control 路径时,用 `check pages [<dbf>...] control` 显式开启。表空间名仍取自
  control 元数据。新增该行为的回归测试。

------

## v0.6.0 - Offline Page Diagnostics

### Added

- **新增 `check pages` 页损坏诊断命令**(借鉴官方 dmdbchk):离线只读
  扫描数据文件,分三层证据判定坏页,无需在线实例:
  - 文件层:文件大小非页大小整数倍(截断/页大小不符);
  - 页头层:页自描述三元组 `(group,file,page)` 与物理位置不符(页头错乱),
    以及格式化后被清零的页(自描述在但 kind/正文全零,区别于保留填充页);
  - 结构层:数据页(kind 0x14/0x16)的 slot 计数、freeEnd、记录数与行长合计
    自相矛盾(对齐 dmdbchk 的 rec_total_len 检查)。
  正确跳过空页、保留/填充页、B 树内部分支页(tree_level>0)和合法内部页,
  避免误报。坏页坐标格式 `page(tablespace,file,page)` 与 dmdbchk 一致,
  便于交叉核对;控制台摘要 + `dul.log` 坏页清单。
  `check pages [<dbf-name>[,...]]` 可限定文件;`set page_check 0/1/2/3` 与
  `set page_hash` 可在 PAGE_CHECK 启用时叠加校验和层。
- **存储范围诊断(字典可用时自动启用)**:check 自动加载磁盘字典后,三项增强:
  - **坏页归属**:按页 storage_id 映射到 `owner.table`;storage_id 被清零的页
    用表段范围(header_file/block/blocks)回退归属;
  - **B 树叶链断链/环检测**:遍历各表存储根→叶链,只报"根有效但链中途断裂/成环"
    的高置信损坏(对齐 dmdbchk 的 "page is broken");根指针陈旧/不匹配等
    字典漂移不误报(unload 路径本就用 fallback 处理);
  - **字典自一致性检查**:重复表 ID、指向不存在表的列、指向不存在 owner 的表。
    目标同 dmdbchk 的对象 ID 合法性检查(发现不可能的目录项),但用更稳健的
    引用一致性方式,不依赖未逆向的 DM ID 预留页格式。
- 关键设计:DM 实例常年 `PAGE_CHECK=0`,官方 dmdbchk 手册也承认"用户数据被改到
  非校验区查不出";dmdul 的页头+结构证据链在无校验和时仍能定位坏页,是对
  dmdbchk 的差异化补充。bootstrap 流程零改动、不变慢——check 复用已落盘字典。

### Validation

- DM8 build 2025-01-17:CHKTEST.T_CHK 2 万行独立表空间,`dd` 注入 4 种损坏
  (页头 page_no 错乱、行数据区破坏、slot 计数破坏、整页清零)。dmdul check
  4/4 精确检出并全部归属到 CHKTEST.T_CHK(含 storage_id 清零页经段范围回退),
  坐标与官方 dmdbchk 报告逐一吻合;叶链检测精确报告 T_CHK 因 page 60
  断裂的单条真损坏,32 张陈旧根指针表零误报;干净全库(MAIN 16384 页 +
  TBS_CHK 8192 页,含真实数据/索引/分区/空页/保留页/B 树内部页)0 坏页、
  0 叶链问题、0 字典问题。

------

## v0.5.8 - Bounded-Memory DMP & LOB Unload

### Changed

- DMP 写入器全面缓冲化：所有顺序写经 512 KiB 缓冲追加并以逻辑偏移做 phase
  核算，消除此前每字段 2 次 write + 2 次 seek 的系统调用风暴（1000 万行
  ≈ 5 亿次 syscall → 每 512 KiB 一次）；WriteAt 补写与 MD5 计算前显式 flush。
- DMP 行编码移入并行 worker：行在解码线程预编码为 wire 段（原子段不跨
  phase 边界，与 WriteRow 字节级等价，有单测证明），writer 仅按序追加；
  流式 LOB 字段仍走 WriteRow 保持大对象不落内存。
- 实测（4C/4GiB，1000 万行）：DMP 卸载 175 秒 → 116 秒；dimp SHOW=Y
  识别行数正确；新增 DMP 并行/顺序输出字节一致性回归。

### Fixed

- 并行卸载新增在飞字节背压阀，关闭大 LOB 表在 SQL/CSV 格式下的内存放大
  风险。此前 worker 按固定 256 页成批解码，行外 LOB 内容在渲染时被物化进
  批缓冲，单批可达数 GiB 且多批堆叠。现改为：worker 将每个 256 页作业按
  字节切成子块（整页边界，默认 4 MiB/块），并由加权信号量把全部在飞解码
  字节限制在默认 256 MiB；writer 按 (作业号, 子块号) 严格有序应用并在应用
  时释放额度。信号量对“writer 正在应用的作业”永久豁免，因此在严格有序
  应用下无死锁（1 字节额度的极限单测可完整跑完且输出逐字节一致）。
  实测 953 MiB CLOB 表（10 万行×10 KiB）：峰值 RSS 随额度线性变化
  （32/256/1024 MiB 额度 → 129/615/1292 MiB RSS），与表内 LOB 总量无关；
  普通表（1000 万行）不受影响，仍 51 秒、堆 214 MiB。
  `DMDUL_UNLOAD_MEM_BYTES` 可作为应急调节。

------

## v0.5.7 - Scalable Unload

### Fixed

- 修复千万行级大表卸载的内存失控：行覆盖键（column-prefix coverage key）此前对
  每张表的每一行无条件生成 O(列数) 个前缀字符串，1000 万行 13 列实测堆峰值 47 GiB，
  4 GiB 内存主机直接被 OOM 终止。三处修复：
  - 表自身主 storage 不再被注册为 historical 候选（`shouldAllowHistoricalRows`
    排除 self-storage），普通表完全跳过覆盖键生成；
  - `0x02000000|table_id` 猜测辅助 id 仅在字典完全没有 storage 信息时才参与扫描；
  - 覆盖键状态改为按表共享并引入 200 万键上限，超限自动停止生成并释放，
    仅当确有 pending 短行时在 fallback reason 中提示人工核对。
- 存储/段两级 fallback 扫描补齐页级去重：所有阶段共享 processed 页集合，
  同一物理页全程只解析一次，行不会因 fallback 重访而重复导出。

### Added

- **并行数据卸载**：page plan 直读阶段自动按页批次并行解码（worker 数取
  `min(CPU 核数, 8)`，无需任何参数）。普通行页内自包含，按页并行天然安全；
  行外 LOB 与 Long Row 页链由锚点行所属 worker 经互斥页缓存整链跟随，不拆链。
  单一 writer 按批次序合并，输出与单线程逐字节一致（600 页合成样本 +
  1000 万行实机 3.75 GiB 输出 MD5 双重验证）。MaxRows、恢复扫描与各级
  fallback 保持顺序路径。`DMDUL_UNLOAD_WORKERS` 仅作为测试与应急开关。
- SQL 行渲染热路径优化：每表 INSERT 前缀只构建一次，行内值用单个 Builder
  拼接，消除逐行的列名引用、切片与 Join 分配。
- 新增 env 门控的手工性能剖析测试（`DMDUL_PROFILE_DIR`）与
  `DMDUL_DEBUG_COVERAGE` 覆盖键诊断输出。

### Validation

- DM8 build 2025-01-17（4C/4GiB 主机）：MOCK.T_CUSTOMER_MOCK 1000 万行 2.13 GiB
  表空间，223712 页全部 page plan 直读、0 失败；修复与并行前后对比：
  SQL 卸载 修复前 OOM → 单线程 228 秒 → 并行 87 秒（输出 MD5 与单线程一致），
  DMP 卸载 347 秒 → 175 秒；8 worker 本机 SQL 49 秒。本地剖析堆峰值
  47 GiB → 240 MiB。DULTEST 九表 53064 行回归含 ALTER 历史行语义不变。

------

## v0.5.6 - disql Compatibility Fixes

### Fixed

- 修复 DDL SQL 输出中触发器、过程、函数、包等 PL/SQL 块缺少 `/` 批终结符的问题。
  此前生成的 `_ddl.sql` / `objects.sql` 在 disql 中执行时，PL/SQL 块之后的所有语句
  会被静默吞入语句缓冲区（最终以 "input too long" 失败），导致后续建表和数据导入
  级联失败。DMP 元数据记录由 dimp 逐条执行，不受影响也不添加 `/`。
- 版本 fallback 字符串从 v0.5.4 更新为 v0.5.5。

### Added

- SQL 导出新增超长语句告警：单条 INSERT 超过 disql 160 KiB 输入缓冲
  （实测 DM8 build 2025-01-17 上限为 163840 字节）时，导出结束时提示受影响表，
  并建议使用 JDBC/ODBC 客户端导入或改用 `data_format dmp`。disql 会用
  "input too long" 中止此类语句并静默丢行，多见于行外大 LOB 表。

### Validation

- 在 DM8 build 2025-01-17（192.168.17.37 快照）上完成回归：DULTEST 9 表
  53064 行 SQL/DMP 双通道导出导回，MINUS 逐行比对一致（唯一差异为已告警的
  TIME 小数秒清零）；修复后的 DDL 含触发器场景经 disql 全量导入零报错。

------

## v0.5.5 - Logical Export Engine

本版本把早期“SQL DDL + 每表纯数据 DMP”升级为与达梦逻辑导出语义对应的原生逻辑
DMP。每次导出生成一个同时包含当前可恢复对象元数据、空表定义和表数据的文件，并继续
保留配套 SQL，便于审计、人工修订和兜底恢复。

### Added

- 新增 `unload schema <schema>[,<schema>...];` 和 `list schema [owner];`；TABLES 与 OWNER
  同样支持逗号选择多个对象，FULL、OWNER、SCHEMAS、TABLES 四种 DMP 级别互斥。
- `dmdul_dict` 新增 `schemas.tsv`，保存模式 ID、模式名及所属用户；旧字典缺少该文件时
  按用户同名默认模式兼容加载。
- 新增原生多 schema footer 和对象元数据记录写入，覆盖用户、角色、表、索引、约束、
  注释、视图、序列、过程、函数、包/包体、触发器、同义词、系统权限、角色授权和对象授权。
- 新增逻辑 DMP 汇编器：复用已有表数据 phase 并流式复制，不把表数据或大型 LOB
  重新加载到内存；空表只写 phase-1 元数据。
- DMP 检查器支持多 schema footer、schema owner、表所属模式和多 phase 累计行数。

### Changed

- `data_format=dmp` 不再按非空表散落生成纯数据文件；TABLES、OWNER、SCHEMAS、FULL
  每次分别生成一个原生逻辑 DMP，并保留配套 `_ddl.sql` 审计文件。
- `unload user` 的 DMP 语义改为 OWNER：包含所选用户以及这些用户拥有的全部模式；
  `unload schema` 只包含明确选择的模式，二者不再混同。
- 默认卸载目录固定为启动 DMDUL 时当前目录下的 `output/`，不再跟随 `data_dir`；
  显式 `set output_dir` 仍具有最高优先级。

### Validation

- 使用 DM8 `03134284336-20250117-257733-20132` 完成四种模式实机验证：
  - TABLES：`dimp SHOW=Y` 识别表、数据、3 个索引、2 个约束和注释，实际重映射导入成功；
  - OWNER：实际导入 3 张表、EMP_INFO 4 行、4 个 routine 和 11 个 synonym，无告警；
  - SCHEMAS：单模式、多非空模式以及空模式与非空模式组合均可识别；
  - FULL：识别 6 个模式、28 张表及视图、序列、routine、包、触发器、同义词、授权和注释。
- 新增 TABLES、OWNER、SCHEMAS、FULL 单文件行为、空表元数据、多模式 footer、对象授权
  编码、模式字典往返及默认输出目录回归测试。
- `go test -count=1 ./...`、`go vet ./...` 和 `git diff --check` 通过。

### Notes

- 当前不生成压缩、加密或多文件 DMP。
- TABLES 以完整父表为最小单位，暂不支持只选择单个叶子分区；分区行由 DM 导入时路由。
- 不同 DM8 构建和文件版本仍需先使用 `dimp SHOW=Y`、`CTRL_INFO=4` 和隔离库回灌验证。

## v0.5.4 - Protected Page Reads & Auditable Residual Recovery

本版本收口页保护边界、固定长度 NULL、二级索引误识别和残留页归属问题，并把
`recover table` 的物理来源证据写入控制台与 `dul.log`。

### Fixed

- 修复序列 DDL 把 `MIN_VALUE` 误当作 `START WITH` 的问题；现在从
  `SYSOBJECTS.INFO5` 的运行槽定位器读取 `SYSTEM.DBF` 序列状态页，并恢复与
  `DBA_SEQUENCES.LAST_NUMBER` 一致的安全高水位，避免恢复后从旧值重新发号。
- 序列上下限、初始值、运行值和增量改为有符号 64 位解析，支持负数及降序序列；
  `CACHE_SIZE=0` 现在显式生成 `NOCACHE`，并补齐 `NOORDER`。
- 序列状态页的 `+0x52` 字段按活动槽数量处理，不再误当作最大槽号；删除序列留下槽空洞时，
  仍可通过 `INFO5` 定位器恢复页容量范围内的高编号有效槽。
- 循环序列越过上下界时把恢复起点折回另一端；已耗尽的非循环序列会在创建后消费一次边界值，
  保留后续 `NEXTVAL` 继续报耗尽的状态，而不是输出越界或重复发号的 DDL。
- 修复切换 `data_dir` 后仍保留上一个工作目录内存字典的问题；`system`、`control`、
  `data_dir` 或 `charset` 改变后均要求重新 bootstrap 或显式加载字典。
- 新 bootstrap 一开始即清除旧内存字典；扫描或写盘失败后不再允许 unload 静默沿用旧结果。
- `list/unload/recover` 自动加载字典时会核对当前实际存在的 SYSTEM 路径、页大小和页数；
  字典属于另一份数据库时明确拒绝，防止旧 `meta.tsv` 把会话静默切回旧库。
- 用户数据页、LOB 页和 Long Row 页改为按磁盘原始字节读取；页保护边界恢复仅保留在
  `SYSTEM.DBF` 标准字典下载路径，避免接近满页时把页尾 slot 项覆盖到活动行。
- SYSTEM 字典页尾 slot 判断支持 `0xFFFF` 空闲/删除槽哨兵，避免混合槽目录被误认为保护备份。
- 显式 2-bit metadata 已标记字段存在时，不再把全零定长值当作 NULL；
  `TIME '00:00:00.000000'` 可正确导出。
- 数据候选排除二级索引 storage，避免索引键/ROWID 被解析成表记录；
  `0x02000000 | table_id` 猜测只在真实数据 storage id 缺失时启用。
- 修复只有 `tables.tsv.partitioned=YES`、缺少分区键和边界明细时退化为普通表 DDL 的问题；
  `RANGE` 整数边界和二进制 `MAXVALUE` 标志现在可直接解码。
- 交互登录测试不再硬编码历史版本号，改为跟随 `internal/version.Version`。

### Changed

- 孤儿 storage 残留页只允许映射到一个目标表；一次恢复选择多个目标表时禁用该启发式路径；
  `SYSINDEXES` 与磁盘字典中的全部 `storage_id/assist_ids` 都参与活动归属排除。
- 孤儿页不再因单条记录碰巧可解析就接受，改为对最多 16 条物理记录执行多行一致性校验。
- `recover table` 按目标表及源 `group/file/storage_id` 聚合物理证据，记录页数、页范围、
  定位/导出/失败行数和 `dictionary` / `heuristic-orphan` 归属类型。
- Standard Bootstrap 第二阶段新增 `SYSOBJINFOS` 精确下载；按真实五列行结构解析
  `TYPE$='TABPART'`，不再通过原始字符串搜索猜测分区键位置。
- `dmdul_dict` 新增 `partitions.tsv` 和 `partition_keys.tsv`，完整保留分区顺序、键列及
  `HIGH_VALUE` 字节；人工修订并 `load dictionary;` 后会直接参与 DDL 和分区数据定位。
- 生成用户 DDL 的占位密码更新为满足常见 PWD_POLICY 的临时密码；恢复后仍必须立即重置。
- 序列运行槽不可访问或校验失败时，DDL 才回退到持久化初始值并输出显式复核告警。
- `dmdul_dict` 改为同级临时目录完整生成、反向加载校验后再切换；已有目录自动归档为
  `dmdul_dict.backup-YYYYMMDD-HHMMSS`，避免中断写入产生新旧 TSV 混合字典。
- `.gitignore` 排除字典备份和 bootstrap staging 目录，避免恢复字典被误提交到仓库。
- 内置版本更新为 `v0.5.4`。

### Added

- 新增页尾 `0xFFFF` 槽哨兵、用户页原样读取、午夜 TIME、多目标孤儿页拒绝、
  孤儿页多行一致性、二级索引 storage 分类和恢复来源日志回归测试。
- 新增 `DataRecoverySource` 结果信息，供交互层和后续恢复报告消费。
- 新增 SYSOBJINFOS TABPART 行解析、分区字典 TSV 往返和整数 RANGE DDL 回归测试。
- `sequences.tsv` 新增 `last_number`、运行页 `file/page/slot` 和槽状态字段，支持人工审查、
  修改和 `load dictionary;` 后复用。
- bootstrap 结构化日志新增 `SEQUENCE_STATE mode=runtime-page-slot`，记录读取页数和成功恢复数。
- bootstrap 日志新增 `dmdul_dict-backup` 输出证据，并新增残留文件隔离、旧目录备份、
  `data_dir` 切换和失败 bootstrap 清理内存字典的回归测试。

### Validation

- 使用 `192.168.17.37:/tmp/dulsnap` 的同一份离线快照完成修改前后对照：
  - `T_BIG`：1 条错行降为 0
  - `T_PART`：2 条错行降为 0
  - `T_BASIC`：午夜 TIME 从 NULL 修正为 `00:00:00.000000`
  - `T_LOB`：由 3 条成功/1 条失败修正为 4 条成功/0 条失败
  - `T_DEL`：恢复结果由含 100 条二级索引伪记录的 200 条收紧为 100 条
  - `T_PART`：恢复 `RANGE("id")`、`1000/2000/MAXVALUE`，磁盘字典重载后 DDL 保持一致；
    数据仍为 16 个计划页、16 次直接读取、0 次 fallback、3000 行成功
  - 三条测试序列离线恢复的安全高水位为 `100/100/21`，与在线
    `DBA_SEQUENCES.LAST_NUMBER` 一致；生成 DDL 使用相同 `START WITH`
  - 32K、GB18030 FEIGE 快照中的 376 条用户序列全部恢复运行值，包含活动数为 378、
    定位槽为 379 的删除空洞场景
  - 在已有 `dmdul_dict` 的 8K 快照目录重复 bootstrap：旧目录成功备份、新字典完整启用，
    `SEQ_T_SEQ_TRG_TEST.last_number=21` 保持不变
- `go test -count=1 ./...` 通过。
- `go vet ./...` 通过。
- `git diff --check` 通过。

### Notes

- `heuristic-orphan` 只能证明页内容与目标结构高度兼容，不能单靠离线物理页证明原始表归属；
  必须结合源页证据、业务字段和隔离库回放复核。
- `/tmp/dulsnap` 中已不存在 `T_TRUNC` 的可识别残留载荷，因此该样本恢复 0 条符合快照现状，
  不代表所有 TRUNCATE 场景都能恢复。
- DELETE 行的事务可见性和 Undo PRE IMAGE 还未完整恢复，删除行中的历史值仍可能受覆盖影响。

## v0.5.2

### Fixed

- 修正 DM8 普通行记录头部解析。
- 明确 `row[0:2]` 为大端 `u16` 物理行长字段：
  - 低 15 位表示物理行长度
  - `0x8000` 表示删除标志
- 修复此前可能将 `00 2C` / `00 2E` 误判为行状态的问题。
- 修复 `n_rec` 滞后时可能截断有效 slot 的问题。
- metadata 2-bit 状态 `10` 当前明确拒绝解析，不再启发式猜测。

### Changed

- 普通 `unload` 已收紧为 slot-only。
- 删除 slot 和无 slot 物理残留仅由 `recover table` 扫描。
- `isLiveDataRow` 语义调整为物理可解析行判断，避免误表达为事务可见。
- 不对迁移行 / 链式行做未经验证的跨页拼接。

### Added

- 新增 19 字节事务/MVCC/Undo 行尾识别：
  - `clu_rowid(6)`
  - `roll_file(1)`
  - `roll_page(4)`
  - `roll_offset(2)`
  - `trx_id(6)`
- 新增 PAGE_CHECK 识别与校验支持：
  - `PAGE_CHECK=0`：无校验
  - `PAGE_CHECK=1`：页头 `0x18` CRC32/IEEE
  - `PAGE_CHECK=2`：页尾 HASH，slot 目录随摘要长度前移
  - `PAGE_CHECK=3`：页头 `0x18` CRC32C/Castagnoli
- 新增文档：
  - `docs/row-page-format.md`
  - `docs/page-check.md`

### Validation

- `go test -count=1 ./...` 通过。
- `go vet ./...` 通过。
- `git diff --check` 通过。
- 已在 `192.168.17.37` DM8 测试环境完成 DELETE 未提交、Rollback、Commit、Checkpoint 差分实验。
- 已验证 4K / 8K / 16K / 32K 页大小 slot 定位。
- 已验证 PAGE_CHECK=0/1/2/3。
- 已完成真实 SYSTEM.DBF bootstrap 和表数据直读验证。

### Notes

- slot-only 不等于事务一致性读。
- 未提交 INSERT / DELETE 的最终可见性仍需后续离线事务状态和完整 Undo PRE IMAGE 链解析后才能准确判断。
- 迁移行 / 链式行需要等待可重复物理样本，不在普通行解析中贸然拼接。

## v0.5.1 - Direct Page Plan & Layered Fallback

Release theme: execute normal table unload through direct page-plan reads, with bounded layered
fallbacks and auditable physical I/O diagnostics.

### Added

- `unload` / `recover` 控制台与 `dul.log` 新增 `planned pages`、`direct pages read`、
  `fallback pages scanned` 和 `fallback reason` 诊断。

### Changed

- 表数据正常导出改为真正执行 `storage root -> internal refs -> leaf chain` page plan，并通过
  `ReadAt` 只读取计划页；不再先全面扫描数据文件后再用计划过滤。
- page plan 不完整或计划页校验失败时，按“同 group `storage_id` 扫描 -> 段范围读取”分层回退；
  全文件残留页扫描仅用于 `recover table`。
- `tables.tsv` 中的 `storage_id/root_file/root_page` 现在会直接参与数据页计划生成。

### Fixed

- leaf 链断裂、循环或页身份/storage id 不一致时不再接受部分 page plan。
- 精确 page ref 不再被可能过期的段范围或表空间元数据错误否决。

### Validation

- 新增“计划成功时直接读取页数等于计划页数”回归测试。
- 新增断链、同 group storage fallback、segment fallback 和 recover 全文件扫描测试。
- `go test -count=1 ./...` 与 `go vet ./...` 通过。

## v0.5.0 - Data Type Matrix & Output Layout

Release theme: complete regular-column recovery across SQL/CSV/DMP and organize large unload
artifact sets under a predictable output directory.

### Added

- 补齐 SQL/CSV/DMP 共用的常规字段类型恢复路径，包括 13 种 INTERVAL、9 位时间戳、
  带时区时间、ROWID、BFILE、JSON/JSONB、国家字符类型及达梦兼容别名。
- 新增 `docs/data-types.md`，记录官网类型对照、字典类型归一化、输出格式限制和
  2026-07-13 DM8 实机验证矩阵。

### Changed

- 默认 `unload` / `recover` 结果统一写入与 `dmdul_dict` 同级的 `output/` 目录；显式
  `output_dir` 仍具有最高优先级。
- `init.dul`、`control.dul`、`dul.log` 和 `dmdul_dict` 保留在工作目录，避免大型用户级或
  整库导出污染工作目录。
- DDL 格式化保留 `CHARACTER VARYING`、`NATIONAL CHARACTER`、
  `NATIONAL CHARACTER VARYING` 和 `NCHAR VARYING` 的长度。

### Fixed

- 修复 `BINARY(n)` 被误按变长字段解析的问题。
- 修复负数 NUMBER base-100 指数偏移导致部分负小数错位的问题。
- 修复 ROWID 物理 12 字节 `epno/partno/real_rowid` 布局及 18 字符显示值输出。
- 生成的 `CONS<number>` 内部约束名不再写入恢复 DDL，由达梦重新分配名称。
- 明确 JSON/JSONB DMP 应使用 `FAST_LOAD=N`，避免导入后内容不可查询。

### Validation

- 完整常规类型矩阵通过官方 `dimp` 导入验证。
- 15 个 JSONB 标量和容器样例通过 `FAST_LOAD=N` 验证。
- 恢复后的 SQL DDL 与数据在隔离模式中完成回放验证。

## v0.4.1 - Standard Bootstrap & Native DMP Export

Release theme: complete the offline chain from standard SYSTEM bootstrap and
dictionary recovery to SQL/CSV/DMP data output and official `dimp` loading.

### Added

- Added Dameng native-compatible pure-data DMP export format.
- Added `data_format=dmp` for interactive unload workflows.
- Added table-, user-, and database-level DMP export; user/database exports create one DMP per non-empty table.
- Added DMP large-table support:
  - 64-bit long-field size encoding
  - multi-phase output
  - dump paths larger than 4 GiB
- Added streaming out-of-line CLOB/BLOB locator export.
- Added `STORAGE(USING LONG ROW)` data path support.
- Added RANGE/LIST/HASH partition-table DMP compatibility with `dimp`; rows are exported through the parent table and routed by DM during import.
- Added UTF-8, GB18030, and EUC-KR DMP headers, including page size, extent size, `UNICODE_FLAG`, and `CASE_SENSITIVE` metadata.
- Added standard two-stage bootstrap through page-0 anchors, storage roots, internal page references, and leaf chains.
- Bootstrap now automatically detects database parameters from offline files.
- Added persistent parameter metadata in init.dul.
- Added interactive commands:

  - `show parameter;`
  - `load parameter;`

- Bootstrap now records parameter source information.

Supported parameters:

- PAGE_SIZE
- EXTENT_SIZE
- PAGE_COUNT
- UNICODE_FLAG / CHARSET
- CASE_SENSITIVE
- DB_NAME
- INSTANCE_NAME

### Improved

- Avoid using stale init.dul charset/case settings when switching databases.
- Database metadata is now associated with the current SYSTEM.DBF.
- Parameter loading can restore previous bootstrap environment.

### Parameter Detection

Detection priority:

| Parameter      | Source                   |
| -------------- | ------------------------ |
| PAGE_SIZE      | SYSTEM.DBF + 0x84        |
| EXTENT_SIZE    | SYSTEM.DBF + 0x80        |
| PAGE_COUNT     | SYSTEM.DBF + 0x8C        |
| CHARSET        | SYSTEM.DBF page 4 + 0x2D |
| CASE_SENSITIVE | SYSTEM.DBF page 4 + 0x2C |
| INSTANCE_NAME  | SYS.SYSOPENHISTORY       |
| DB_NAME        | dm.ctl                   |

Default values:

| Parameter      | Default  |
| -------------- | -------- |
| PAGE_SIZE      | 8192     |
| EXTENT_SIZE    | 16       |
| CHARSET        | GB18030  |
| CASE_SENSITIVE | 1        |
| DB_NAME        | DAMENG   |
| INSTANCE_NAME  | DMSERVER |

### Validation

Verified with:

- oldpro
- FEIGE
- QYT

Validation includes:

- bootstrap
- load parameter
- show parameter
- unload database

Native DMP validation also includes:

- official `dimp DATA_ONLY=Y FAST_LOAD=Y` loading
- UTF-8, GB18030, and EUC-KR headers
- RANGE/LIST/HASH partition tables
- 32 MiB out-of-line LOB streaming
- multi-phase output and 64-bit size regression coverage

## v0.4.0

### Added

- 新增 DM8 DMP 格式解析与纯数据流式写入能力。
- DMP 头支持 UTF-8、GB18030、EUC-KR 的 encoding code 与 UNICODE_FLAG 配对。
- DMP 写入器支持 8 MiB 多 phase 自动切分和 Reader 流式长字段，已用分区表及 32 MiB LOB 完成官方 `dimp/FAST_LOAD` 回灌。
- DMP 行数解析支持累计 phase-2 以后各 phase，并识别 `0xFFFFFFFF` 行续传标记。
- `set data_format dmp;` 正式接入 `unload table`、`unload user` 和 `unload database`。
- 用户级和整库级 DMP 按表生成，空表不生成空 DMP，便于逐表重试和并行快速装载。
- 行外 CLOB/BLOB 在 DMP 导出路径中按 locator 页链流式读取，不再先拼接完整 LOB 到内存。
- 用户数据页扫描和 LOB 页缓存统一恢复 4 KiB 扇区边界的页保护字节，避免 16K/32K 页中的字段内容被保护字节污染。
- DMP 写入器支持暂停/继续，整库存在大量表时最多只保持有限数量的活动文件句柄。
- 新增 `case_sensitive=auto|0|1`；`auto` 从 `SYSTEM.DBF` 第 4 页偏移 `0x2C` 读取建库大小写敏感标志，也允许控制页损坏时显式修正 DMP 文件头。
- `bootstrap` 日志和 `dmdul_dict/meta.tsv` 记录已解析的 `case_sensitive` 值及物理来源；该偏移已通过 6 个已有实例和一组同字符集差分实例验证。
- 补充 BIT、BYTE、BINARY、`INTERVAL YEAR TO MONTH` 和 DM ROWID 显示值解析。
- 增加超过 4 GiB 长字段长度的 64 位编码回归测试，整表通过多 phase 持续输出。
- 新增 `docs/dmp-format-research.md`，记录 DMP 头、phase、字段、footer、压缩和官方回灌验证结论。
- 新增 `unload object <owner|all>;`，用于单独导出 owner 或整库对象字典 DDL，不导出表数据。
- DDL 和数据导出器新增单次扫描、按表多文件分流能力。
- 新增标准两阶段 `bootstrap` 流程。
- 第一阶段通过 `SYSTEM.DBF` page 0 的 anchor 定位核心系统字典入口：
  - `SYSOBJECTS`
  - `SYSINDEXES`
- 第二阶段根据核心字典定位并下载扩展系统字典：
  - `SYSCOLUMNS`
  - `SYSTEXTS`
  - `SYSGRANTS`
  - `SYSHPARTTABLEINFO`
- 新增结构化 `bootstrap` 日志。
- `bootstrap;` 现在会在控制台和 `dul.log` 中记录：
  - SYSTEM.DBF 基本信息
  - page size / extent size / charset
  - DBF 文件 group/file/page 信息
  - SYSOBJECTS / SYSINDEXES anchor 信息
  - root page / storage id / leaf pages / rows
  - 核心字典表扫描方式
  - fallback 原因
  - 输出目录
  - 对象统计
  - 执行耗时
  - 最终状态
- `dmdul_dict/meta.tsv` 新增 bootstrap 模式信息：
  - `bootstrap_mode`
  - `bootstrap_fallback`

### Changed

- `unload user <owner>;` 改为按表生成 `<prefix>_<table>_ddl.sql` 和 `<prefix>_<table>_data.sql|csv|dmp`。
- `unload user` 不再生成混合了所有模式对象的用户级 DDL；完整对象定义改由 `unload object` 负责。
- 单表 DDL 不再夹带同一 owner 的无关视图、序列、函数、包或同义词。
- CSV 格式下空表只生成 DDL，不生成空 CSV。
- DMP 格式下空表只生成 DDL，不生成空 DMP。
- 移除重复语义的 `unload user all;`；整库恢复统一使用 `unload database;`。
- DMP 研究确认 TIME 小数秒无法由当前版本官方 DMP 通道无损保存，并确认跨数据库字符集导入不会可靠自动转码。
- `bootstrap` 优先使用 `standard-two-stage` 标准字典下载模式。
- 当 anchor、storage root 或 leaf chain 无效时，自动回退到原有按页流式扫描模式。
- 空 TEMP 文件或可重建临时文件会标记为 `IGNORED_TEMP`，不影响最终成功状态。
- `dul.log` 从简单命令日志升级为可诊断的恢复证据链日志。
- `list user;` 会显示当前 bootstrap 模式和 fallback 状态。
- README、使用文档和开发文档已调整为交互式流程优先。

### Removed

- 移除功能性命令行子命令：
  - `inspect`
  - `inspect-ctl`
  - `scan-system`
  - `scan-partitions`
  - `export-ddl`
  - `export-data`
- 移除仅用于旧 inspect 命令的 `internal/storage` 包。
- 后续 DDL、数据导出、分区扫描、残留页恢复统一通过交互式 DUL Shell 执行。

### Breaking Changes

- 从 v0.4.0 开始，`dmdul` 不再支持直接命令行导出 DDL 或数据。
- 以下命令已废弃并移除：

```text
dmdul export-ddl
dmdul export-data
dmdul inspect
dmdul inspect-ctl
dmdul scan-system
dmdul scan-partitions
```

请改用交互式流程：

~~~text
dmdul
DMDUL> bootstrap;
DMDUL> unload database;
DMDUL> unload user HIS;
DMDUL> unload table SYSDBA.T1;
DMDUL> recover table USERS1.T_TEST;
~~~

### Validation

- `go test -count=1 ./...` 通过。
- `go vet ./...` 通过。
- oldpro 样例验证通过：
  - users: 6
  - tables: 28
  - columns: 122
  - rows exported: 12679
  - rows failed: 0
- FEIGE 样例验证通过：
  - users: 125
  - tables: 3814
  - columns: 65253
  - routines: 115
  - bootstrap mode: `standard-two-stage`
  - fallback: `false`
- 真实 `load dictionary; list table HR_TEST;` 流程验证通过。
- 表级、用户级、整库级 DMP 自动化测试通过，覆盖空表跳过、输出前缀、MD5/头校验和表行数。
- 300 页 LOB 链流式读取测试通过，页缓存保持固定上限。
- oldpro 真实离线样例通过官方 `dimp DATA_ONLY=Y FAST_LOAD=Y` 回灌：LOB 表 10 行、RANGE 分区表 10 行，导入无告警。

### Notes

- `standard-two-stage` 模式下，对象数可能比旧版本略少，因为旧版本在 root/internal 页中可能误识别少量分隔键对象。
- 如果标准 anchor 或 root-chain 损坏，dmdul 会自动 fallback 到流式扫描，并在日志中记录原因。



## v0.3.0

### Added

- 新增 `STORAGE(USING LONG ROW)` 表初步恢复能力。
- 新增 21 字节 LOB locator 解析。
- 新增从当前活动行追踪 `0x20` LOB 页和 `0x22` Long Row 页的读取能力。
- 新增显式 2-bit 行 NULL metadata 解码。
- 新增 metadata 优先的数据行解析路径，旧启发式解析保留为兜底。
- 初步补充以下基础类型解析入口：
  - `REAL`
  - `FLOAT`
  - `DOUBLE`
  - `TIME`
  - 带时区时间类型
  - `INTERVAL DAY TO SECOND`
  - `ROWID`

### Changed

- 数据行解析核心从主要依赖启发式判断，调整为优先使用显式 metadata。
- 变长字段解析增强，支持识别普通变长值、内联 LOB envelope 和 21 字节 locator。
- LOB / Long Row 读取不再依赖全文件扫描 LOB 页，而是从当前行 locator 出发按页链读取。
- 数据页候选过滤进一步收紧，降低普通索引页、主键页被误识别为普通表数据页的概率。
- DDL 类型格式化补充带时区时间、`INTERVAL`、`ROWID` 等类型处理。

### Fixed

- 修复 `STORAGE(USING LONG ROW)` 表导出时多导出索引伪行的问题。
- 修复部分 CLOB / Long Row 场景下 locator 无法正确追踪的问题。
- 修复 `TIME` 类型误走 8 字节 `DATETIME` / `TIMESTAMP` 路径的问题。
- 保留 `ALTER TABLE ADD COLUMN` 历史行恢复能力，同时避免普通索引页短行被误导出。

### Validation

- `go test -count=1 ./...` 通过。
- Windows x64 构建验证通过。
- Linux x64 构建验证通过。
- 真实样例 `JYC.T_LONG_ROW_LOB` 验证通过：
  - `rows exported: 5`
  - `rows failed: 0`
  - 没有额外索引伪行。
- 回归样例 `JYC."t"` 验证通过：
  - 仍可导出 `ALTER TABLE ADD COLUMN` 前的历史行。
  - `id=10, name=NULL, birth=NULL` 未被误伤。

### Notes

- Long Row / 行外 LOB 当前为初步支持。
- 复杂 LOB 页链、损坏页、多版本残留页、特殊字符集和更多基础类型样例仍需继续验证。

## v0.2.1

### Fixed

- 修复 `ALTER TABLE ADD COLUMN` 后历史行解析问题。
- 旧记录会按历史列布局解析，新增尾部列在允许时补 `NULL`。
- 修复主键页、索引页中前缀键值被误识别为普通表数据的问题。
- 改进候选行去重逻辑，优先保留完整表行，降低重复短行、假行导出概率。

### Validation

- `go test -count=1 ./...` 通过。
- Windows x64 构建验证通过。
- Linux x64 构建验证通过。
- 真实样例验证：
  - `JYC."t"` 新旧行混合场景导出正常。
  - `rows failed: 0`。

------

## v0.2.0

### Added

- 增强表数据定位能力，从“段范围扫描”升级为：
  - storage root
  - internal page refs
  - leaf chain
- 新增表数据 page plan 定位逻辑。
- 新增 DROP / TRUNCATE 后残留页恢复能力。
- 新增交互式命令：

```text
recover table <owner.table_name>;
```

- `export-data` 新增恢复模式参数：

```text
-recover
```

- `bootstrap` 生成的 `tables.tsv` 新增恢复辅助字段：
  - `storage_id`
  - `root_file`
  - `root_page`
  - `assist_ids`

### Changed

- 表数据导出优先使用 storage root / leaf chain 精确定位。
- 段范围信息 `header_file/header_block/blocks` 作为辅助校验。
- 当 root、leaf chain 或 page refs 不完整时，回退到段范围扫描或全文件扫描。
- TRUNCATE 后恢复模式不再被当前缩小后的 `blocks/extents` 限制。

### Fixed

- 进一步降低同名表、相似行格式、隐藏索引页导致误匹配的概率。
- 改进相同 owner 或不同 owner 下相似表结构的数据页识别准确性。

### Validation

- `go test ./...` 通过。
- Windows x64 构建验证通过。
- Linux x64 构建验证通过。
- 真实样例验证：
  - `SY."t"` 导出正确。
  - `unload database` 导出成功，`rows failed: 0`。

------

## v0.1.9

### Added

- 新增存储过程恢复。
- 新增函数恢复。
- 新增包和包体恢复。
- `bootstrap` 生成 `routines.tsv`。
- `load dictionary;` 支持加载 `routines.tsv`。
- `unload table`、`unload user`、`unload database` 支持输出：
  - `CREATE OR REPLACE PROCEDURE`
  - `CREATE OR REPLACE FUNCTION`
  - `CREATE OR REPLACE PACKAGE`
  - `CREATE OR REPLACE PACKAGE BODY`

### Changed

- `dmdul_dict` 对象字典范围扩大到存储过程、函数、包和包体。
- `DATABASE_ddl.sql` 整库 DDL 输出范围进一步完善。

### Validation

- `go test ./...` 通过。
- 真实样例验证：
  - `routines loaded: 10`
  - `routines exported: 10`

------

## v0.1.8

### Added

- 新增序列恢复能力。
- 新增触发器恢复能力。
- 新增对象级字典测试。
- 完善 `dictionary_extras` 相关对象解析逻辑。

### Changed

- 更新 README 和文档，补充序列、触发器和对象字典说明。
- 改进 `dmdul_dict` 中扩展对象的加载和导出流程。

### Validation

- `go test ./...` 通过。
- Windows x64 构建验证通过。

------

## v0.1.7

### Added

- 新增视图恢复能力。
- 新增同义词恢复能力。
- 新增表、视图、序列对象授权恢复能力。
- 新增 `dictionary_extras.go`。
- `bootstrap` 生成以下对象字典文件：
  - `views.tsv`
  - `synonyms.tsv`
  - `tab_privs.tsv`

### Changed

- `unload database` 的 DDL 范围从用户、表、索引、约束扩展到更多数据库对象。
- 更新 README、`docs/config.md`、`docs/usage.md`。

### Validation

- `go test ./...` 通过。
- Windows x64 构建验证通过。
- Linux x64 构建验证通过。

------

## v0.1.6

### Added

- 新增 `unload database;` 整库离线导出能力。
- 新增字典驱动恢复流程。
- `dmdul_dict` 开始真正参与 DDL 和数据导出。
- `bootstrap` 增强 `tables.tsv` 段定位字段：
  - `header_file`
  - `header_block`
  - `bytes`
  - `blocks`
  - `extents`
- 新增 dictionary override 和 segment 推断测试。

### Changed

- DDL 和数据导出优先使用 `dmdul_dict` 中修正后的用户、表、字段、类型、表空间和存储组织信息。
- `SYSTEM.DBF` 继续用于索引、约束、分区和物理定位等底层信息。
- 整库导出可以同时生成：
  - `DATABASE_ddl.sql`
  - `DATABASE_data.sql`

### Fixed

- 修复相同表名不同 owner 时可能误匹配数据页的问题。
- 降低普通索引页被误识别为表数据页的概率。

### Validation

- `go test ./...` 通过。
- 真实样例验证：
  - `unload database;` 可导出整库 DDL 和数据。
  - `SY."t"` 不再串到 `SYSDBA.T`。

------

## v0.1.5

### Added

- 新增数据字典持久化能力。
- 新增 `dmdul_dict` 字典文件加载和保存逻辑。
- 新增 `dul.log` 交互式操作日志。
- `dul.log` 每条命令和错误记录带本地时间戳。
- 新增字典文件相关测试。

### Changed

- 交互式流程可以通过 `load dictionary;` 加载已保存的文本字典。
- `list user`、`list table` 等命令可以展示字典来源和统计信息。

### Validation

- `go test ./...` 通过。

------

## v0.1.4

### Changed

- 重构 README 项目展示。
- 增加项目定位、功能预览、安装构建、快速开始、安全提醒和版本规划。
- 补充 GitHub 项目首页展示内容。

### Notes

- 该版本主要为文档和项目展示优化版本。

------

## v0.1.3

### Added

- 新增交互式 DUL Shell。
- 新增 REPL 模式。
- 新增 DUL 风格命令：
  - `bootstrap;`
  - `list user;`
  - `list table <owner>;`
  - `unload table <owner.table_name>;`
  - `unload user <owner>;`
- 新增 `control.dul` 相关处理逻辑。
- 新增字典模块初始实现。

### Changed

- 改进 CLI 使用体验。
- 操作方式从单纯命令行参数扩展为交互式恢复流程。

### Validation

- `go test ./...` 通过。

------

## v0.1.2

### Changed

- 调整部分命令参数。
- 改进隐藏 `TABOBJ` / `INDEX` 内部对象识别。
- 增强表号低位 `assist id` 处理。
- 增加页级样例行确认候选表逻辑。

### Fixed

- 降低普通索引页被误导出为表数据页的概率。
- 改进离线数据页候选判断准确性。

### Validation

- `go test ./...` 通过。

------

## v0.1.1

### Added

- 新增 `.gitattributes`。
- 规范 Go、Markdown、Shell、PowerShell 等文件换行符策略。

### Changed

- 改进跨平台开发时的换行符一致性。

------

## v0.1.0

### Added

- 初始版本发布。
- 新增项目基础结构：
  - `cmd/dmdul`
  - `internal/cli`
  - `internal/dm`
  - `internal/storage`
  - `internal/version`
  - `docs`
  - `research`
- 新增基础命令：
  - `inspect`
  - `inspect-ctl`
  - `scan-system`
  - `export-ddl`
  - `export-data`
  - `scan-partitions`
  - `version`
  - `help`
- 初步支持：
  - `SYSTEM.DBF` 基础解析
  - `dm.ctl` 基础解析
  - 用户表 DDL 导出
  - 普通表数据导出
  - 分区表扫描研究命令

### Validation

- `go test ./...` 通过。
