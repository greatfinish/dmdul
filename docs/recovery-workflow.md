# 达梦离线恢复标准流程

本文是 dmdul 的作业手册：从拿到一堆 DBF 文件，到把数据落回一个可查询的达梦实例，
每一步该做什么、该看什么、什么情况下必须停下来。

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

## 1. 取一致性离线文件

**首选：停库后复制。**

```bash
DmServiceDMSERVER stop
mkdir -p /recover/snap
cp /dmdata/DAMENG/*.DBF /dmdata/DAMENG/dm.ctl /recover/snap/
DmServiceDMSERVER start
```

**次选（不能停库）：先 checkpoint 再复制。**

```sql
CHECKPOINT(100);
```

```bash
cp /dmdata/DAMENG/*.DBF /dmdata/DAMENG/dm.ctl /recover/snap/
```

checkpoint 把脏页刷盘，但复制期间仍可能有新写入落到已复制的文件上，一致性弱于停库。
用这条路径时，务必在第 9 步做全量比对而不是抽样。

**最差（文件已损坏/实例起不来）：直接复制现有文件。** 此时一致性已经无从谈起，
按第 3 步的 `check pages` 结果评估可恢复范围。

必须复制的文件：

| 文件 | 必需 | 用途 |
| --- | --- | --- |
| `SYSTEM.DBF` | 是 | 系统表空间，字典的唯一来源 |
| 业务表空间 `*.DBF` | 是 | 表数据本体 |
| `MAIN.DBF` | 是 | 默认表空间，多数用户表在这里 |
| `dm.ctl` | 否 | 表空间名与文件映射参考；缺失时按页头自识别 |
| `dm.ini` | 否 | 页大小等参数参考 |
| `ROLL.DBF` / `TEMP.DBF` | 否 | 回滚段/临时段，dmdul 不使用 |

## 2. 建立恢复目录

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

## 3. 预检

**先确认文件都被认出来了**，不需要先 bootstrap：

```text
DMDUL> list datafile;
```

逐列检查：

- `status` 全是 `OK`。`UNREADABLE` 是权限或文件损坏，`SIZE?` 是文件大小不是页大小整数倍
  （被截断，或页大小判断错了）。
- 表空间清单和你预期的一致，没有缺文件。
- `pages` × 页大小 = 文件实际大小。

**再评估损坏程度**（文件可疑时执行，干净库可跳过）：

```text
DMDUL> check pages;
```

坏页坐标格式 `page(tablespace,file,page)` 与官方 dmdbchk 一致，可交叉核对。
字典可用时还会把坏页归属到 `owner.table`，并检测 B 树叶链断裂。
坏页落在业务表上就意味着那张表会有数据丢失，提前知道比事后发现好。

## 4. bootstrap 重建字典

```text
DMDUL> bootstrap;
```

这是整个流程的地基，看三处：

1. `[BOOTSTRAP] phase=metadata status=OK` —— 页大小、字符集、大小写敏感必须正确。
   字符集错了所有中文都是乱码；大小写敏感错了 DDL 里的标识符引号会错。
2. `[BOOTSTRAP] stage=1 phase=validate name=core-catalog status=OK` —— 核心目录通过，
   说明 SYSOBJECTS/SYSINDEXES 读出来了。
3. `[BOOTSTRAP] phase=complete status=SUCCESS mode=standard-two-stage` ——
   `mode` 是 `standard-two-stage` 才是正常路径；出现 fallback 模式说明字典有损，
   后续结果要加倍怀疑。

字典落盘在 `dmdul_dict/`，是纯文本 TSV。后续会话可以直接：

```text
DMDUL> load dictionary;
```

字典明显有错时可以手工修 `dmdul_dict/*.tsv` 再 `load dictionary`，但这是专家操作，
改错了会让恢复结果静默错乱。

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
DMDUL> unload user <owner>;              -- 用户级
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
