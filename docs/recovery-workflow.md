# 达梦离线恢复标准流程

本文是 dmdul 的作业手册：从拿到普通文件系统 DBF 或离线 DMASM 成员裸盘，到把数据落回
一个可查询的达梦实例。每一步都说明该做什么、该核对什么、什么情况下必须停下来。

所有命令都在 dmdul 交互界面里执行（提示符 `DMDUL>`），回灌命令在操作系统 shell 里执行。

## 0. 红线

开工前先确认这四条，任何一条不满足都不要继续：

1. **不要对着还在运行的库读数据文件。** dmdul 是离线工具，实例开着时页面随时被写脏，
   读到的撕裂页解码出来是"看起来成功但内容错乱"的行——`rows failed: 0` 检测不到这种
   损坏。必须先停库或做一致性快照（见 1）。
2. **不要在原始文件上操作。** 把离线文件复制到独立的恢复目录，原件只读留档。dmdul 只读
   数据文件，但误操作、误覆盖的代价不可逆。
3. **不要直接往生产库回灌。** 所有导出的 DDL 和数据必须先在隔离测试库验证通过。
4. **确认这确实是最后手段。** DMRMAN 备份、归档恢复、闪回、`dexp` 逻辑导出都优先于 dmdul。

## 1. 获取同一时间点的离线输入

dmdul 支持两条输入路径，开始前必须先确定使用哪一条：

| 输入路径 | 必需输入 | `dm.ctl` 的角色 |
| --- | --- | --- |
| 文件系统 DBF/HFS | `SYSTEM.DBF`、目标表空间 DBF；目标含 HUGE 表时还要完整 HFS 根目录 | 可选核对；数据文件页头和离线副本优先 |
| DMASM 裸盘 | 承载目标数据库 DBF 的全部相关磁盘组成员盘 | 从 ASM 目录中自动发现，缺失时按受限规则降级 |

不要把不同时间点、不同数据库或不同快照链的文件混在同一个恢复任务里。

### 1.1 文件系统 DBF

**首选：停库后复制。**

```bash
DmServiceDMSERVER stop
mkdir -p /recover/snap
cp /dmdata/DAMENG/*.DBF /dmdata/DAMENG/dm.ctl /recover/snap/
# 目标包含 HUGE 表时，还要复制对应混合表空间的完整 HFS 根，例如：
cp -a /dmdata/DAMENG/HMAIN /recover/snap/
DmServiceDMSERVER start
```

**次选（不能停库）：先 checkpoint 再复制。**

```sql
CHECKPOINT(100);
```

```bash
cp /dmdata/DAMENG/*.DBF /dmdata/DAMENG/dm.ctl /recover/snap/
cp -a /dmdata/DAMENG/HMAIN /recover/snap/  # 仅在目标包含 HUGE 表时需要
```

checkpoint 把脏页刷盘，但复制期间仍可能发生新写入，因此普通逐文件复制不能构成严格的一致性
快照。一致性弱于停库，使用这条路径时务必在第 9 步扩大比对范围。

**最差（文件已损坏/实例起不来）：直接复制现有文件。** 此时一致性已经无从谈起，
先看 bootstrap 自动 SYSTEM 预检，再按第 4 步的第二阶段 `check pages` 评估可恢复范围。

必须复制的文件：

| 文件 | 必需 | 用途 |
| --- | --- | --- |
| `SYSTEM.DBF` | 是 | 系统表空间，字典的唯一来源 |
| 业务表空间 `*.DBF` | 是 | 表数据本体 |
| `MAIN.DBF` | 是 | 默认表空间，多数用户表在这里 |
| `HMAIN/` 或其他 HFS 根 | 视情况 | HUGE 列数据；必须与 SYSTEM/MAIN DBF 来自同一快照 |
| `dm.ctl` | 否 | 表空间名与文件映射参考；缺失时按页头自识别 |
| `dm.ini` | 否 | 页大小等参数参考 |
| `ROLL.DBF` / `TEMP.DBF` | 否 | 回滚段/临时段，dmdul 不使用 |

### 1.2 DMASM 成员裸盘

DMDSC/DMASM 环境应停止所有可能写盘的数据库和 ASM 节点，再复制裸设备或创建存储一致性
快照。NORMAL/HIGH 冗余并不能抵消跨时间点采集：同一个 extent 的不同副本如果来自不同时间，
读取成功也不代表内容一致。

必须满足：

- 所有成员盘来自同一时间点；
- 提供承载 SYSTEM、MAIN 和目标用户表空间 DBF 的全部相关磁盘组；
- 尽量保留同一冗余组的全部可用成员，便于副本校验和坏副本切换；
- Linux 使用裸设备或其只读镜像；VMware `monolithicFlat` 使用实际数据区 `*-flat.vmdk`；
- 不要传只有描述信息的 `.vmdk` 文件，也不要混用基础盘与不同时间点的 delta 快照。

dmdul 会直接恢复 DMASM 目录、INODE、AU 描述符、副本和条带映射，不要求先启动 DMASMSVR，
也不要求先执行 `asmcmd cp`。

当前 DMASM 逻辑 Reader 只覆盖 DBF。若目标包含 HUGE 表，还必须另行提供对应的 HFS 文件
目录；DMASM 成员裸盘中的 HFS 文件映射尚未接入 HUGE 解析器。

## 2. 建立独立恢复目录并选择输入

每个离线快照使用一个新的可写工作目录。`init.dul`、`control.dul`、`dul.log`、`dmdul_dict/`
和 `output/` 都留在这个目录，避免旧字典被另一个数据库或快照误用。

### 2.1 文件系统 DBF

把 dmdul 可执行文件和离线文件放在同一个目录，启动后无需任何 `set` 即可 bootstrap：

```bash
mkdir -p /recover/work
cp /recover/snap/* /recover/work/
cp dmdul /recover/work/
cd /recover/work && ./dmdul
```

启动时 dmdul 会自动探测并打印一行数据库身份，**先核对这一行是不是你要恢复的库**：

```text
detected: db_name=DAMENG instance=DMSERVER page_size=8192 pages=9472 charset=UTF-8 (UNICODE_FLAG=1) case_sensitive=0 (SYSTEM.DBF: /recover/work/SYSTEM.DBF)
```

文件放在别处时显式指定：

```text
DMDUL> set system /recover/work/SYSTEM.DBF;
DMDUL> set data_dir /recover/work;
DMDUL> set output_dir /recover/out;
```

> `data_dir` 里的同名文件优先于 `dm.ctl` 记录的绝对路径。这一条很关键：与原库同机恢复时，
> `dm.ctl` 存的是 `/dmdata/DAMENG/...`，如果跟随它就会读到线上原文件而不是你的离线副本。

目标包含 HUGE 表时，把 `HMAIN/` 或其他 HFS 根放在 `data_dir` 下。`bootstrap` 会把 HUGE
主表和事务辅助对象写入磁盘字典；`describe owner.table;` 可核对 SECTION、FILESIZE、
WITH DELTA 和四个辅助表 ID。完整支持边界见
[DM8 HUGE 列存储表离线恢复](huge-tables.md)。

### 2.2 DMASM 裸盘

先启动 dmdul，设置可写工作目录，再一次配置所有同一时点的成员盘：

```text
DMDUL> set data_dir /recover/asm-work;
DMDUL> set output_dir /recover/asm-work/output;
DMDUL> set asm_disk /snapshot/data01.raw,/snapshot/data02.raw,/snapshot/fra01.raw;
```

`set asm_disk` 会立即扫描全部磁盘组的 INODE 目录，并按数据库打印：

- 数据库名、字符集、页大小、页数、簇大小和大小写敏感标志；
- ASM `SYSTEM.DBF` 与可选 `dm.ctl` 路径；
- SYSTEM、MAIN、ROLL、TEMP 和用户表空间 DBF 的 group/file、页数、大小、状态及 ASM 路径。

只有一个 `SYSTEM.DBF` 候选时，工具自动把它设为活动 `system`。存在多个候选时不会根据
数据库名或目录猜测，必须从输出中选择目标库：

```text
DMDUL> set system +DATA/data/DMDB/SYSTEM.DBF;
```

发现结果在 bootstrap 前就写入：

| 文件 | 内容 |
| --- | --- |
| `dmdul_dict/asm_databases.tsv` | 每个数据库候选的身份、参数、SYSTEM/dm.ctl 路径、成员盘和 `selected` 状态 |
| `dmdul_dict/asm_datafiles.tsv` | 以 `candidate_no + system_path` 关联的完整 DBF 集合及状态 |

多库环境切换 `set system` 时，`selected` 会同步更新。重启和 `load parameter;` 会按
`init.dul` 重新扫描成员盘并核对目录；切换到文件系统 `SYSTEM.DBF` 时，候选证据仍保留，
但所有 ASM 候选都会改为 `selected=NO`。

需要把 ASM 逻辑文件交给其他工具或保存独立证据时，可以先物化为普通 DBF：

```text
DMDUL> cp datafile /recover/dbf-copy;
```

复制并非 bootstrap/unload 的前置条件。复制完成后若改走文件系统路径，应显式切换：

```text
DMDUL> set data_dir /recover/dbf-copy;
DMDUL> set system /recover/dbf-copy/SYSTEM.DBF;
```

## 3. 预检数据库身份与文件集

文件系统输入直接列出数据文件：

```text
DMDUL> list datafile;
```

DMASM 输入先查看全部目录和候选数据库。`list asmfile` 不要求预先设置 `system`：

```text
DMDUL> list asmfile;
```

如果有多个候选，先执行 `set system <ASM path>;`，再只查看目标数据库的文件集：

```text
DMDUL> list datafile;
DMDUL> show parameter;
```

逐列检查：

- `status` 全是 `OK`。`UNREADABLE` 是权限或文件损坏，`SIZE?` 是文件大小不是页大小整数倍
  （被截断，或页大小判断错了）。
- 表空间清单和你预期的一致，没有缺文件。
- `pages` × 页大小 = 文件实际大小。
- group/file 身份没有异常重复，路径确实指向本次离线副本或目标 ASM 逻辑文件。
- ASM 多库场景中，`asm_databases.tsv` 只有目标候选为 `selected=YES`，且
  `asm_datafiles.tsv` 中每行的 `system_path` 都属于该候选。

数据库名相同不足以证明文件属于同一个库。至少同时核对 SYSTEM 路径、页大小、字符集、
group/file、表空间和文件状态。任一关键文件缺失、候选未选择或身份冲突时都应停止。

不需要在这里额外执行 `check pages SYSTEM.DBF;`。下一步的 bootstrap 会自动完成同等的
SYSTEM 纯物理预检，避免用户重复输入命令。

## 4. bootstrap 重建字典

```text
DMDUL> bootstrap;
```

这是整个流程的地基，看四处：

1. `[bootstrap] phase=metadata status=OK` —— 页大小、字符集、大小写敏感必须正确。
   字符集错了所有中文都是乱码；大小写敏感错了 DDL 里的标识符引号会错。
2. `[bootstrap] phase=precheck name=SYSTEM.DBF status=OK mode=physical-only` —— bootstrap
   已在读取字典前扫描完整 SYSTEM.DBF。它检查文件对齐、页头、PAGE_CHECK 和数据页结构，
   同时支持文件系统和 DMASM 逻辑 SYSTEM 文件。
3. `[bootstrap] stage=1 phase=validate name=core-catalog status=OK` —— 核心目录通过，
   说明 SYSOBJECTS/SYSINDEXES 读出来了。
4. `[bootstrap] phase=complete status=SUCCESS mode=standard-two-stage` ——
   `mode` 是 `standard-two-stage` 才是正常路径；出现 fallback 模式说明字典有损，
   后续结果要加倍怀疑。

SYSTEM 预检发现坏页时不会立即终止恢复。bootstrap 会继续尝试解析可读字典，并以
`SUCCESS_WITH_WARNINGS` 结束；控制台同时提示执行第二阶段 `check pages;`。

根据第一阶段结果选择下一步：

| bootstrap 结果 | 后续操作 |
| --- | --- |
| SYSTEM 预检和字典恢复均正常，且只恢复少量明确表 | 检查 `dmdul_dict` 和 `describe` 结果后可以直接卸载 |
| SYSTEM 预检出现告警，但字典仍可恢复 | 先审核字典，再执行全库或指定 DBF 的 `check pages` |
| 字典进入 fallback、文件缺失或身份冲突 | 不直接开始整库卸载；先补齐文件并执行 `check pages` 收集物理证据 |
| bootstrap 无法建立字典 | 先作无归属的 `check pages`；v0.10.0 新增 `scan storage` 和人工列定义恢复，也可修复字典后再做对象归属 |

这里的顺序是硬性约束：从当前快照恢复对象归属时，必须先 `bootstrap` 或显式
`load dictionary`，再执行第二阶段对象归属检查。纯物理扫描本身不受此限制。

无字典救援不执行自动对象归属，也不加载目录中的旧字典。`scan storage;` 先产生
`storage_scan.tsv` 和样例，之后按人工提供的列定义执行 `recover storage ...`。
这一入口从 v0.10.0 起提供，详见 [无字典救援步骤及限制](storage-rescue.md)。

如果页头损坏，bootstrap 会用多页身份、页类型及校验/结构证据探测页大小；候选不唯一或与
页头冲突时停止。不要按文件能被 8 KiB 整除就手工假定页大小。4/8/16/32 KiB 是当前接受范围，
64 KiB 不作为已支持页大小。

字典落盘在 `dmdul_dict/`，是纯文本 TSV。后续会话可以直接：

```text
DMDUL> load dictionary;
```

字典明显有错时可以手工修 `dmdul_dict/*.tsv` 再 `load dictionary`，但这是专家操作，
改错了会让恢复结果静默错乱。

ASM 模式下，bootstrap 会原子重建完整字典目录，再把 `asm_databases.tsv` 和
`asm_datafiles.tsv` 写回新目录，因此 bootstrap 前采集的候选和文件集证据不会丢失。

如果工作目录里已经有旧 `dmdul_dict`：

- 要从当前 SYSTEM 重新恢复字典，先执行 `bootstrap;`；不要在此之前运行会自动加载字典的
  `list` 或 `unload`；
- 只有明确复用已审核字典时才跳过 bootstrap 并执行 `load dictionary;`；
- 自动加载会核对 SYSTEM 路径、页大小和页数，但最稳妥的做法仍是每个快照使用独立工作目录。

### 第二阶段：按需检查全部数据文件

以下任一条件成立时，在 bootstrap 成功后执行：

- SYSTEM 自动预检出现 `WARNING`；
- bootstrap 使用 fallback 或报告缺失数据文件；
- 要恢复整个数据库、用户或模式；
- DBF 来源、存储介质或一致性存在疑问；
- 需要形成完整坏页审计证据。

```text
DMDUL> check pages;
```

针对少量明确表的恢复，可只检查相关 DBF：

```text
DMDUL> check pages MAIN.DBF,TBS_APP01.DBF;
```

独立 `check pages` 只使用当前会话由 `bootstrap` 建立或用户显式 `load dictionary` 加载的字典，
不会隐式加载目录中残留的 `dmdul_dict`。没有字典时仍执行完整物理检查，但所有坏页标记为
`UNATTRIBUTED`。

当前全库 `check pages` 只扫描文件系统 `data_dir`，不直接遍历全部 DMASM 逻辑 DBF。ASM 输入
需要第二阶段全库诊断时，先执行 `cp datafile <directory>;`，再切换到复制目录后运行。

坏页坐标格式 `page(tablespace,file,page)` 与官方 dmdbchk 一致。命令完成后生成：

| 文件 | 用途 |
| --- | --- |
| `output/check_summary.md` | 扫描总量、三类损坏、归属率、受影响对象/表和损坏字节比例 |
| `output/check_bad_pages.tsv` | 全量坏页坐标、绝对字节偏移、storage_id、对象归属和损坏证据 |
| `output/check_affected_objects.tsv` | 按物理 storage 聚合坏页数量、损坏类型及已知表段头命中数，便于确定恢复顺序 |

终端每文件最多保留 4096 条坏页明细，TSV 和聚合统计仍覆盖全部坏页。`TABLE_ASSIST` 只证明
辅助 storage 隶属于父表，不猜测 INDEX、LOB、分区或其他具体类型。

## 5. 盘点对象

```text
DMDUL> list user;
DMDUL> list table <owner>;
DMDUL> describe <owner>.<table>;
```

`describe` 会打印表的**物理位置**——storage_id、B 树 root（file#/page#）、段头、
块数/簇数、存储属性、`assist_ids`，以及完整列清单。
恢复前用它确认两件事：**表被定位到了**，**它的数据在哪**。

`assist_ids` 里出现多个 storage_id 是正常的（分区、TRUNCATE 前的旧存储都会在），
但如果一张普通表的 `blocks` 是 0，说明段信息丢了，数据大概率取不出来。

## 6. 选择导出通道

| 通道 | 命令 | 产物 | 适用 |
| --- | --- | --- | --- |
| SQL | `set data_format sql;` | `_ddl.sql` + `_data.sql` | 小表、需要人工审阅和改动、逐条挑数据 |
| dmfldr | `set data_format fldr;` | `_ddl.sql` + `_data.txt` + `_data.ctl` | 大批量回灌，最快 |
| DMP | `set data_format dmp;` | `_ddl.sql` + `.dmp` | 要连元数据一起走官方 `dimp`；宽行表必选 |

选择要点：

- **超宽行（`STORAGE(USING LONG ROW)`）走 DMP。** disql 从 stdin 读入时每行上限 2499 字符，
  超宽行的 INSERT 语句根本喂不进去。
- **`TIME` 类型有非零小数秒时不要走 DMP。** DM 原生 DMP 通道不保存 `TIME` 的小数秒，
  dmdul 会打印告警。这种字段走 SQL 或 dmfldr。
- **JSON/JSONB 走 DMP 时必须 `FAST_LOAD=N`。**
- 其余情况按数据量选：几千行以内 SQL 最省事，上万行以上 dmfldr 或 DMP。

## 7. 卸载

```text
DMDUL> unload table <owner>.<table>;     -- 单表
DMDUL> unload object <owner>;            -- 用户拥有的对象 DDL
DMDUL> unload user <owner>;              -- 用户级
DMDUL> unload schema <schema>;           -- 模式级
DMDUL> unload database;                  -- 整库
DMDUL> recover table <owner>.<table>;    -- DELETE/DROP/TRUNCATE 后的残留数据
```

看完成后的这几行：

```text
rows exported: 10000000
rows failed: 0
planned pages: 223712
direct pages read: 223712
fallback pages scanned: 0
fallback reason: none
```

- `rows failed` 非 0：有行解不出来，SQL 通道会在输出里留 `-- FAILED ... page=N slot=N` 注释
  指出坐标；dmfldr/DMP 通道只计数（写注释会破坏装载）。
- `fallback reason` 不是 `none`：page plan 没建全，dmdul 退到了扫描模式。结果仍可能正确，
  但覆盖范围没有保证，要在第 9 步重点比对。
- `direct pages read` 明显小于 `planned pages`：有页读不出来，对照 `check pages` 结果。

## 8. 回灌到隔离测试库

先建一个专用用户，**不要用原用户名**，避免和线上对象撞车：

```sql
CREATE USER RECOVER_V IDENTIFIED BY "<password>" DEFAULT TABLESPACE <ts>;
GRANT DBA TO RECOVER_V;
```

### SQL 通道

```bash
disql SYSDBA/password < HR_TEST_EMP_INFO_ddl.sql
disql SYSDBA/password < HR_TEST_EMP_INFO_data.sql
```

多模式用户的 DDL 里会有 `CREATE SCHEMA ... AUTHORIZATION`，以 `/` 结尾单独成批——
DM 的 `CREATE SCHEMA` 会吞掉后续语句直到 `/`，不要手工把它删掉。

### dmfldr 通道（最快）

先建表，再装数据：

```bash
disql SYSDBA/password < HR_TEST_EMP_INFO_ddl.sql
dmfldr USERID=SYSDBA/password@127.0.0.1:5236 CONTROL="'HR_TEST_EMP_INFO_data.ctl'"
```

- **注意那层双引号，不是笔误。** dmfldr 拒绝解析含 `.` 的未加引号参数值（报
  `parameters parse error[...]`，方括号里是被点号截断后的前半截），而 shell 会把裸单
  引号吃掉。所以单引号必须用双引号包住才能活着送到 dmfldr。反过来，**不含点号的值不
  能加引号**——`DIRECT='TRUE'` 本身就是 parse error。
- 控制文件里的分隔符、NULL 标记、字符集、`BLOB_TYPE` 都已按数据文件写死，通常不用改。
- 装载完检查 `_data.bad`：文件不存在或为空才算干净。
- 换目标模式时改 `.ctl` 里的 `INTO TABLE` 一行即可。

### DMP 通道

先看包里有什么，再导：

```bash
dimp SYSDBA/password FILE=MOCK.dmp SHOW=Y NOLOGFILE=Y
```

```bash
dimp SYSDBA/password FILE=MOCK.dmp REMAP_SCHEMA=MOCK:RECOVER_V LOG=dimp.log
```

- `SHOW=Y` 只解析元数据、不导数据，**不能用它判断数据是否可导**。
- JSON/JSONB 必须加 `FAST_LOAD=N`。
- BFILE 只恢复 locator，目标库要预先建同名 DIRECTORY 并准备外部文件。
- 结尾必须是 `terminate import success without warning`。出现
  `[WARNING]data abnormal, import fail...` 就是数据段结构有问题，不要接受这次恢复结果。

## 9. 校验

**这一步不能省。** `rows failed: 0` 只说明 dmdul 自己没报错，不代表数据对。

行数与聚合值：

```sql
SELECT COUNT(*), SUM(<numeric_col>), MIN(<pk>), MAX(<pk>) FROM RECOVER_V.<table>;
```

源库还在就做双向 MINUS，这是最强的比对：

```sql
SELECT COUNT(*) FROM (SELECT * FROM <src_owner>.<table> MINUS SELECT * FROM RECOVER_V.<table>);
SELECT COUNT(*) FROM (SELECT * FROM RECOVER_V.<table> MINUS SELECT * FROM <src_owner>.<table>);
```

两个方向都必须是 0。

> **千万行级的表不要直接 MINUS。** MINUS 需要把两侧结果集全部排序/哈希，
> 两张 1000 万行表的双向比对实测能把一台 4 GB 的实例直接 OOM（内核杀掉 dmserver）。
> 大表按主键分块比，每块几十万行：

```sql
SELECT COUNT(*) FROM (
  SELECT * FROM <src_owner>.<table> WHERE <pk> BETWEEN 1 AND 500000
  MINUS
  SELECT * FROM RECOVER_V.<table>  WHERE <pk> BETWEEN 1 AND 500000);
```

表有主键、两侧行数又相等时，单向为空即可判定相等（无重复行，A ⊆ B 且 |A| = |B| ⟹ A = B），
不必跑两遍。

LOB 列不能直接进 MINUS，单独比：

```sql
SELECT a.id, LENGTH(a.c), LENGTH(b.c), DBMS_LOB.COMPARE(a.c, b.c)
FROM <src_owner>.<table> a, RECOVER_V.<table> b WHERE a.id = b.id;
```

源库已经没了，就退而求其次：核对行数是否符合业务预期、抽样人工确认、检查主键连续性、
对金额类字段做求和交叉核对。

## 10. 常见坑

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| `set asm_disk` 没有发现 SYSTEM 候选 | 成员盘不完整、传入 VMDK descriptor、磁盘组不可识别或权限不足 | 核对全部同一时点成员盘；VMware 改用 `*-flat.vmdk` |
| ASM 显示多个数据库，bootstrap 无法继续 | 目标数据库尚未选择 | 核对每个候选文件集后执行 `set system +GROUP/.../SYSTEM.DBF;` |
| ASM 候选里混入另一数据库的 DBF | 旧版本按相同目录后缀跨库归属 | 使用 v0.7.2+ 重新扫描，核对两张 ASM TSV 和各库 `dm.ctl` 路径 |
| 启动后显示了旧库对象 | 同一工作目录残留了以前的完整 `dmdul_dict`，命令触发自动加载 | 为当前快照换独立目录，或在任何 `list`/`unload` 前先执行 `bootstrap;` |
| 全部中文乱码 | 字符集判断错 | `set charset gb18030;` 后重新 bootstrap |
| `check pages` 报 0 坏页但数据明显有问题 | 读到了线上原文件而不是离线副本 | 确认 `list datafile` 里的 path 指向恢复目录 |
| DDL 建表失败：模式不存在 | 多模式用户，`CREATE SCHEMA` 被跳过或漏了 `/` | 用 v0.6.3+ 重新导出 DDL |
| 超宽行 INSERT 报 input too long | disql stdin 每行 2499 字符上限 | 改走 DMP 通道 |
| dmfldr 装载后 BLOB 长度翻倍 | 控制文件用了 `BLOB_TYPE='HEX'` | 用 v0.6.4+ 重新导出（应为 `'HEX_CHAR'`） |
| `dmfldr` 报 `parameters parse error[...]` | 含 `.` 的值没带引号到 dmfldr；shell 把裸单引号吃掉了 | 写成 `CONTROL="'x.ctl'"`；不含点号的值反而不能加引号 |
| 比对大表时实例被 OOM 杀掉 | 千万行双向 MINUS 撑爆内存 | 按主键分块比对，见第 9 步 |
| `dimp` 报 data abnormal，只导进一部分 | 数据段 phase 边界切在行中间 | 用 v0.6.5+ 重新导出 DMP |
| `rows exported` 比预期多 | 命中了 TRUNCATE 前的旧存储（历史行） | 用 `describe` 看 `assist_ids`，按主键去重 |
| 内存吃紧 | 大 LOB 表解码缓冲 | `DMDUL_UNLOAD_MEM_BYTES` 调小（默认 256 MiB） |

## 11. 实测参考数据

DM8 build 2025-01-17、8 KiB 页、UTF-8、4C/4GiB 虚拟机：

| 场景 | 规模 | 卸载耗时 | 回灌耗时 |
| --- | --- | --- | --- |
| `T_CUSTOMER_MOCK` 13 列 SQL | 1000 万行 | 约 51 秒 | —— |
| `T_CUSTOMER_MOCK` dmfldr | 1000 万行 / 1.76 GB | 92 秒 | —— |
| `T_CUSTOMER_MOCK` DMP | 1000 万行 / 1.66 GB | 120 秒 | `dimp` 30 秒 |
| DULTEST 全类型 9 张表 dmfldr | 53064 行 | 数秒 | 双向 MINUS 差异 0 |

## 12. 一个完整实例

[实战：用 dmdul 离线恢复 1000 万行表](practice-10m-two-channel.md)按本流程走了一遍完整的
DMP 与 dmfldr 双通道往返，含每一步的真实命令、真实输出和输出该怎么读。
