# 实战：用 dmdul 离线恢复 1000 万行表（DMP 与 dmfldr 双通道往返）

本文是一次完整的实机记录：用 dmdul v0.6.6 从达梦的数据文件里离线提取一张 1000 万行的表，
分别经 **DMP 通道**（官方 `dimp` 导入）和 **dmfldr 通道**（官方 `dmfldr` 装载）灌回数据库，
两次都验证行数一致。每一步都给出真实命令、真实输出和该步骤到底在做什么。

环境：CentOS 7、DM8 build 2025-01-17、8 KiB 页、UTF-8、大小写不敏感、4C/4GiB 虚拟机。
目标表 `MOCK.T_CUSTOMER_MOCK`，13 列客户模拟数据，1000 万行，约 2.2 GB。

> **本次演示直接读取了运行中实例的数据文件**，这是为了缩短演示链路。真实恢复场景**必须**
> 先停库或做一致性快照——实例开着时页面随时被写脏，读到的撕裂页解码出来是"看起来成功但
> 内容错乱"的行，`rows failed: 0` 检测不到这种损坏。完整的前置要求见
> [离线恢复标准流程](recovery-workflow.md)第 0、1 节。

---

## 一、准备与启动

把 dmdul 二进制放到数据文件所在目录，加执行权限：

```bash
cd /dmdata/DAMENG
chmod +x dmdul
./dmdul version
```

```text
dmdul v0.6.6 (14d79a7, built 2026-07-22)
```

版本串里带着 git 提交号和构建日期，出问题时能精确对上代码。

直接启动进入交互式界面：

```bash
./dmdul
```

```bash
dmdul: Release v0.6.6 - Dameng Database Offline Recovery & Data Unloader
Copyright (c) 2026 greatfinish. All rights reserved.
https://github.com/greatfinish/dmdul
Type help; for available commands.
detected: db_name=DAMENG instance=DMSERVER page_size=8192 pages=9472 charset=UTF-8 (UNICODE_FLAG=1) case_sensitive=0 (SYSTEM.DBF: /dmdata/DAMENG/SYSTEM.DBF)
```

**注意最后那行 `detected:`。** dmdul 默认从可执行文件同目录找 `SYSTEM.DBF`，找到就立刻读文件头
探测数据库身份并打印出来，不需要任何 `set` 命令，也不扫字典，几乎零耗时。

这一行是**第一道关卡**，开工前先核对：

| 字段 | 本次值 | 该确认什么 |
| --- | --- | --- |
| `db_name` | `DAMENG` | 是不是你要恢复的那个库 |
| `page_size` | `8192` | 页大小判断错了后面全盘皆错 |
| `charset` | `UTF-8` | 字符集错了所有中文变乱码 |
| `case_sensitive` | `0` | 影响 DDL 里标识符加不加引号 |
| `SYSTEM.DBF` | 实际路径 | 确认读的是你放的那个文件 |

文件不在同目录时手工指定：

```bash
DMDUL> set system /recover/work/SYSTEM.DBF;
DMDUL> set data_dir /recover/work;
DMDUL> set output_dir /recover/out;
```

---

## 二、预检：确认文件都被认出来了

```text
DMDUL> list datafile;
```

```bash
GROUP  FILE TABLESPACE        PAGES   SIZE_MB STATUS     PATH
----- ----- ------------ ---------- --------- ---------- -------------------------------------------
    0     0 SYSTEM             9472      74.0 OK         /dmdata/DAMENG/SYSTEM.DBF
    1     0 ROLL              16384     128.0 OK         /dmdata/DAMENG/ROLL.DBF
    3     0 TEMP               1280      10.0 OK         /dmdata/DAMENG/TEMP.DBF
    4     0 MAIN              16384     128.0 OK         /dmdata/DAMENG/MAIN.DBF
    5     0 TBS_BIN_TEST       4096      32.0 OK         /dmdata/DAMENG/TBS_BIN_TEST01.DBF
    7     0 TBS_TEST           4096      32.0 OK         /dmdata/DAMENG/TBS_TEST01.DBF
    9     0 TBS_DULV          16384     128.0 OK         /dmdata/DAMENG/TBS_DULV01.DBF
   10     0 TBS_MOCK         573440    4480.0 OK         /dmdata/DAMENG/TBS_MOCK01.DBF
   11     0 TBS_LOBBIG       204800    1600.0 OK         /dmdata/DAMENG/TBS_LOBBIG01.DBF
   12     0 TBS_CHK            8192      64.0 OK         /dmdata/DAMENG/TBS_CHK01.DBF
   13     0 TBS_MIG            8192      64.0 OK         /dmdata/DAMENG/TBS_MIG01.DBF
data files: 11 (readable & page-aligned: 11), page_size=8192
```

**这个命令不需要先 bootstrap**，它靠页头自识别定位文件。三件事要逐列看：

- `STATUS` 全是 `OK`。`UNREADABLE` 是权限或文件损坏；`SIZE?` 表示文件大小不是页大小的整数倍，
  意味着被截断，或者页大小判断错了。
- 表空间清单跟你预期的一致，目标表所在的 `TBS_MOCK` 在列。
- 末行 `readable & page-aligned` 的数量等于总数。

文件可疑时还可以接一句 `check pages;` 做页损坏诊断，坏页坐标格式与官方 dmdbchk 一致，
可交叉核对。本次文件干净，跳过。

---

## 三、bootstrap：重建数据字典

```bash
DMDUL> bootstrap;
```

这是整个流程的地基——dmdul 要从 `SYSTEM.DBF` 里把达梦的系统表（SYSOBJECTS、SYSCOLUMNS 等）
自己读出来，重建出"有哪些用户、哪些表、每张表什么结构、数据在哪几页"。输出很长，
挑关键的看：

```bash
[bootstrap] phase=metadata status=OK db_name="DAMENG" ... page_size=8192 ... charset="UTF-8 (UNICODE_FLAG=1)" ... case_sensitive=0 ...
[bootstrap] stage=1 phase=anchor name=SYSOBJECTS mode=root-chain status=OK root=0/16 storage=33554540 pages=57 rows=1822
[bootstrap] stage=1 phase=anchor name=SYSINDEXES mode=root-chain status=OK root=0/288 storage=33554434 pages=4 rows=427
[bootstrap] stage=1 phase=validate name=core-catalog mode=root-chain status=OK rows=2249
[bootstrap] stage=2 phase=extract name=SYSCOLUMNS mode=root-chain status=OK pages=66 rows=5045
[bootstrap] phase=output name=control.dul status=OK files=12 path="/dmdata/DAMENG/control.dul"
[bootstrap] phase=output name=dmdul_dict status=OK users=39 schemas=41 tables=177 columns=848 ... path="/dmdata/DAMENG/dmdul_dict"
[bootstrap] phase=complete status=SUCCESS mode=standard-two-stage objects=1822 elapsed_ms=6002
```

**三个必看点：**

1. **`phase=metadata status=OK`** —— 页大小、字符集、大小写敏感必须正确。这三个错一个，
   后面全是垃圾。
2. **`stage=1 phase=validate name=core-catalog status=OK`** —— 核心目录校验通过，说明
   SYSOBJECTS / SYSINDEXES 被正确读出来了。
3. **`phase=complete status=SUCCESS mode=standard-two-stage`** —— `mode` 必须是
   `standard-two-stage`。如果出现 fallback 模式（如 `stream-scan-fallback`），说明字典有损，
   dmdul 退到了扫描猜测，后续结果要加倍怀疑。

其中 `mode=root-chain` 表示字典表是顺着 B 树根指针一路读叶子链拿到的，这是最可信的路径。
`raw-window-fallback` 只出现在 ROUTINES/TRIGGERS 的源码补全上，属于正常的补充手段。

耗时 **6.0 秒**，1822 个对象。

字典落盘成纯文本 TSV：

```bash
dictionary dir: /dmdata/DAMENG/dmdul_dict (users=39 schemas=41 tables=177 columns=848 ...)
```

后续会话可以跳过 bootstrap 直接 `load dictionary;` 复用。

---

## 四、盘点：库里有什么、表在哪

### 4.1 有哪些用户

```bash
DMDUL> list user;
```

```bash
USER                SCHEMAS   TABLES    VIEWS   SYNONYMS  SEQUENCES  TRIGGERS  FUNCTIONS  PROCEDURES  PACKAGES
------------------ -------- -------- -------- ---------- ---------- --------- ---------- ----------- ---------
DMDUL_OWNER_IMPORT        1        3        0         11          0         0          3           1         0
DULTEST                   1        9        1          1          1         1          1           1         0
MOCK                      1        4        0          0          0         0          0           0         0
SCHTEST                   2        1        0          0          0         0          0           0         0
SYSDBA                    1       33        4          0          1         2          2           2         1
（此处省略其余用户）
```

注意 `SCHTEST` 的 `SCHEMAS` 是 **2**。达梦里**用户和模式是一对多关系**：建用户会自动创建同名
默认模式，但一个用户还可以拥有额外的附加模式（`CREATE SCHEMA x AUTHORIZATION user`）。
恢复多模式用户时这一列很重要。

### 4.2 目标用户下有哪些表

```bash
DMDUL> list table MOCK;
```

```bash
OWNER TABLE           TABLE_ID   COLUMNS    TABLESPACE STORAGE      PARTITION
----- --------------- ---------- ---------- ---------- ------------ ---------
MOCK  NUMS10          1380       1          TBS_DULV   CLUSTERBTR   NO
MOCK  NUMS100         1381       1          TBS_DULV   CLUSTERBTR   NO
MOCK  NUMS10000       1383       1          TBS_MOCK   CLUSTERBTR   NO
MOCK  T_CUSTOMER_MOCK 1384       13         TBS_MOCK   CLUSTERBTR   NO
4 table(s)
```

### 4.3 表的物理位置——恢复前的最后确认

```bash
DMDUL> desc MOCK.T_CUSTOMER_MOCK;
```

```bash
Table MOCK.T_CUSTOMER_MOCK
  table_id= 1384, tablespace= TBS_MOCK (group#= 10), columns= 13
  storage_id= 33555996, root= file#0/page#308432, segment= file#0/block#128, blocks= 283184, extents= 17699
  storage= CLUSTERBTR, bytes= 2319843328
  assist_ids= [33555996 33555997 33555816]
Column information:
  col# 00 ID                               BIGINT len=8 scale=0         NOT NULL
  col# 01 CUSTOMER_NO                      VARCHAR len=20 scale=0       NULL
  col# 02 CUSTOMER_NAME                    VARCHAR len=50 scale=0       NULL
  ...
  col# 12 CREATED_TIME                     DATETIME len=8 scale=6       NULL
```

`describe`（可简写 `desc`）借鉴自 Oracle DUL 的 `desc owner.table`，但多打印了**物理定位信息**。
恢复前用它确认两件事：**表被定位到了**，**它的数据在哪**。

- `storage_id` / `root` —— 表的存储段 ID 和 B 树根页坐标，unload 就是从这里出发遍历叶子链。
- `blocks= 283184` / `bytes= 2319843328` —— 段占了 28 万个块、约 2.2 GB。如果一张有数据的表
  这里显示 `blocks= 0`，说明段信息丢了，数据大概率取不出来。
- `assist_ids` —— 与该表相关的所有 storage_id。出现多个是正常的：分区、以及
  **TRUNCATE 之前的旧存储**都会留在这里。这也意味着 unload 有可能捞到历史残留行，
  行数比预期多时先查这里。

---

## 五、通道一：DMP（走官方 dimp）

DMP 是达梦的原生逻辑导出格式。dmdul 直接生成 `.dmp`，用官方 `dimp` 导入，
**元数据和数据一起走**，不需要单独执行 DDL。

### 5.1 切换格式并导出整个用户

```bash
DMDUL> set data_format dmp;
data_format = dmp
DMDUL> unload user MOCK;
```

```bash
ddl output: /dmdata/DAMENG/output/MOCK_ddl.sql
dmp output: /dmdata/DAMENG/output/MOCK.dmp
dmp mode: OWNER
schemas exported: 1
objects exported: 9
tables exported: 4
users exported: 1
role grants exported: 2
constraints exported: 1
rows exported: 10010110
rows failed: 0
planned pages: 223749
direct pages read: 223749
fallback pages scanned: 0
fallback reason: none
```

**输出里最该盯的四行：**

| 行 | 本次值 | 含义 |
| --- | --- | --- |
| `rows exported` | 10010110 | 10 + 100 + 10000 + 10000000，四张表全中 |
| `rows failed` | 0 | 没有解不出来的行 |
| `direct pages read` / `planned pages` | 223749 / 223749 | 计划要读的页全部读到了 |
| `fallback reason` | `none` | 走的是 page plan 直读，没有退化到扫描 |

`fallback reason` 不是 `none` 时要警惕：说明 page plan 没建全，dmdul 退到了扫描模式，
结果可能仍然正确，但覆盖范围没有保证。

产物：

```bash
ls -lh output/
```

```bash
-rw-r--r-- 1 dmdba dinstall 1.1K Jul 22 21:43 MOCK_ddl.sql
-rw-r--r-- 1 dmdba dinstall 1.7G Jul 22 21:45 MOCK.dmp
```

`MOCK_ddl.sql` 是给人看的参考（DMP 通道用不到它），内容包括建用户、授权、四张建表语句
和主键约束：

```sql
-- Users and roles
-- Password hashes are not exported. Change the placeholder password after import.
CREATE USER MOCK IDENTIFIED BY "Dmdul_2026#Reset" DEFAULT TABLESPACE "TBS_MOCK" TEMPORARY TABLESPACE "TEMP";

GRANT DBA TO MOCK;
GRANT PUBLIC TO MOCK;

CREATE TABLE MOCK.T_CUSTOMER_MOCK (
    ID BIGINT NOT NULL,
    ...
    CREATED_TIME DATETIME(6)
)
STORAGE(ON "TBS_MOCK", CLUSTERBTR);

ALTER TABLE MOCK.T_CUSTOMER_MOCK ADD CONSTRAINT PK_CUSTOMER_MOCK PRIMARY KEY (ID);
```

**注意那句注释：密码哈希不导出**，建用户语句里是占位密码，导入后必须改掉。

### 5.2 制造"灾难现场"

为了验证恢复真的有效，把源对象删掉：

```sql
SELECT COUNT(1) FROM MOCK.T_CUSTOMER_MOCK;   -- 10000000，确认删之前有数据
DROP USER MOCK CASCADE;
```

`DROP USER ... CASCADE` 会连同该用户的**所有模式和对象**一起删除——这正是达梦
用户/模式一对多关系的体现。

### 5.3 用 dimp 导回

```bash
dimp SYSDBA/<PASSWORD> FILE=MOCK.dmp LOG=dimplog2026_07_22.log
```

```bash
[0/9]import instance's USER objects : MOCK
[4/9]start importing schema[MOCK]...
[TABLE: NUMS10]import table MOCK.NUMS10 , has coped with 10 rows, size 0.030 KB
[TABLE: NUMS100]import table MOCK.NUMS100 , has coped with 100 rows, size 0.383 KB
[TABLE: NUMS10000]import table MOCK.NUMS10000 , has coped with 10000 rows, size 57.514 KB
[TABLE: T_CUSTOMER_MOCK]import table MOCK.T_CUSTOMER_MOCK , has coped with 10000000 rows, size 1.658 GB
[8/9][TABLE: T_CUSTOMER_MOCK]import constraint of table:
[8/9][TABLE: T_CUSTOMER_MOCK]PK_CUSTOMER_MOCK
[9/9]all the import process spent total   79.108 s

terminate import success without warning
```

**结尾必须是 `terminate import success without warning`。** 出现
`[WARNING]data abnormal, import fail...` 就说明数据段结构有问题，不要接受这次恢复结果。

用户、四张表、1000 万行数据、主键约束，一条命令全部还原，**79 秒**。

### 5.4 校验

```sql
SELECT COUNT(1) FROM MOCK.T_CUSTOMER_MOCK;
```

```bash
1          10000000
```

> **踩过的坑：`SHOW=Y` 验证不了数据。** `dimp ... SHOW=Y` 只解析元数据、不导数据，
> 哪怕数据段是坏的它也会把行数报得漂漂亮亮。v0.6.5 之前 dmdul 的 DMP 在数据段
> 8 MiB 边界处会把一行劈成两半跨 phase，`dimp` 读到半行就报 `data abnormal` 并放弃整张表，
> 而这个 bug 一直没被发现，正是因为大表只跑过 `SHOW=Y`。**大表必须跑真实导入 + 行数比对。**

---

## 六、通道二：dmfldr（走官方快速装载器）

dmfldr 是达梦的 Fast Loader，吃**分隔文本 + 控制文件**。dmdul 为每张表生成
`_data.txt`（纯数据行）和配套的 `_data.ctl`（控制文件），DDL 需要单独执行。

### 6.1 刷盘并重新 bootstrap

刚才 `dimp` 重建了对象，数据字典已经变了。先让脏页落盘：

```sql
SELECT CHECKPOINT(100);
```

然后重新 bootstrap：

```bash
DMDUL> bootstrap;
```

```bash
[bootstrap] phase=output name=dmdul_dict-backup status=OK path="/dmdata/DAMENG/dmdul_dict.backup-20260722-215707"
[bootstrap] phase=complete status=SUCCESS mode=standard-two-stage objects=1822 elapsed_ms=6995
```

注意多了一行 **`dmdul_dict-backup`**：dmdul 发现已有字典目录，先把旧的备份成
`dmdul_dict.backup-<时间戳>` 再覆盖，不会让你丢掉上一次的字典。

再看表：

```bash
DMDUL> list table MOCK;
```

```bash
OWNER TABLE           TABLE_ID   COLUMNS    TABLESPACE STORAGE      PARTITION
----- --------------- ---------- ---------- ---------- ------------ ---------
MOCK  T_CUSTOMER_MOCK 1389       13         TBS_MOCK   CLUSTERBTR   NO
```

`TABLE_ID` 从 **1384 变成了 1389**——`dimp` 重建的是全新对象，表 ID、storage_id、
B 树根页全都变了。**这就是为什么数据库结构一变就必须重新 bootstrap**：拿旧字典去 unload，
会按着旧的物理坐标去读，读到的是已经被回收或被别的对象占用的页。

### 6.2 导出为 dmfldr 格式

```bash
DMDUL> set data_format fldr;
data_format = fldr
DMDUL> unload table MOCK.T_CUSTOMER_MOCK;
```

```bash
ddl output: /dmdata/DAMENG/output/MOCK_T_CUSTOMER_MOCK_ddl.sql
data output: /dmdata/DAMENG/output/MOCK_T_CUSTOMER_MOCK_data.txt
tables exported: 1
rows exported: 10000000
rows failed: 0
planned pages: 223712
direct pages read: 223712
fallback pages scanned: 0
fallback reason: none
```

产物三件套：

```bash
-rw-r--r-- 1 dmdba dinstall 1.2K Jul 22 21:58 MOCK_T_CUSTOMER_MOCK_data.ctl
-rw-r--r-- 1 dmdba dinstall 1.7G Jul 22 21:59 MOCK_T_CUSTOMER_MOCK_data.txt
-rw-r--r-- 1 dmdba dinstall  588 Jul 22 21:57 MOCK_T_CUSTOMER_MOCK_ddl.sql
```

### 6.3 读懂控制文件

```bash
cat MOCK_T_CUSTOMER_MOCK_data.ctl
```

```sql
-- Generated by dmdul. Create the table first (companion _ddl.sql), then load with:
--   dmfldr USERID=SYSDBA/password@127.0.0.1:5236 CONTROL="'MOCK_T_CUSTOMER_MOCK_data.ctl'"
-- The single quotes must reach dmfldr itself, which is why they are wrapped in
-- double quotes: dmfldr rejects an unquoted value containing '.' ...
-- Fields are separated by SOH (0x01) and rows by STX+LF (0x02 0x0A): this table has
-- text columns, and dmfldr supports neither enclosure nor escaping, so a printable
-- separator could not be told apart from column content.
OPTIONS
(
        SKIP = 0
        ROWS = 50000
        ERRORS = 100
        DIRECT = FALSE
        BLOB_TYPE = 'HEX_CHAR'
        NULL_MODE = TRUE
        NULL_STR = '\\N'
        CHARACTER_CODE = 'UTF-8'
)
LOAD DATA
INFILE 'MOCK_T_CUSTOMER_MOCK_data.txt' STR X '020A'
BADFILE 'MOCK_T_CUSTOMER_MOCK_data.bad'
INTO TABLE MOCK.T_CUSTOMER_MOCK
FIELDS X '01'
(
        ID,
        CUSTOMER_NO,
        ...
        CREATED_TIME
)
```

逐项说明，这些取值都是在真实 DM8 实例上试出来的，与文档写法有出入的地方以实测为准：

| 选项 | 值 | 为什么 |
| --- | --- | --- |
| `FIELDS X '01'` | SOH 分隔 | 见下文"分隔符为什么这么怪" |
| `STR X '020A'` | STX+LF 换行 | 同上 |
| `BLOB_TYPE` | `'HEX_CHAR'` | 用 `'HEX'` 会把十六进制字符本身存进 BLOB，长度翻倍 |
| `NULL_MODE` + `NULL_STR` | `TRUE` / `\N` | NULL 写作 `\N`，空字段因此表示空字符串，两者可区分 |
| `DIRECT` | `FALSE` | `BLOB_TYPE` 只在 `DIRECT=FALSE` 时有效 |
| `ROWS` | 50000 | 每 5 万行提交一次 |
| `ERRORS` | 100 | 容错 100 行后中止 |

**分隔符为什么是 SOH 而不是竖线？** dmfldr 既不支持包围符（`ENCLOSED BY` 是语法错误），
也不做转义（实测 `ESCAPED BY` 不反转义）。这意味着任何**可打印字符**做分隔符，都无法与列
内容区分——数据里出现一个 `|` 就会把行切错。所以 dmdul 按列类型自动选：

- 所有列都不可能产生 `|`/CR/LF（数值、日期时间、二进制等）→ 用可读的 `|` + LF；
- **只要有一个字符类型列** → 改用 SOH（`0x01`）分隔、STX+LF（`0x02 0x0A`）换行。

dmdul 从不在字段值里写 C0 控制字符，所以这组分隔符不可能与数据冲突。本表有 VARCHAR 列，
因此走的是 SOH 方案。这也是为什么 `head` 看数据文件时字段是"粘"在一起的——分隔符不可见：

```bash
1639707C0001639707陈刚18898362027user1639707@example.com江苏省浦东新区...2024-08-17 07:27:21.000000
```

### 6.4 制造现场并建表

```sql
DROP TABLE MOCK.T_CUSTOMER_MOCK;
```

dmfldr 只装数据、不建表，所以先执行 DDL：

```bash
disql SYSDBA/<PASSWORD> < MOCK_T_CUSTOMER_MOCK_ddl.sql
```

```sql
SQL> 2   3   4  ... 19  executed successfully
SQL> 2   3   4   executed successfully
```

验证结构还原正确：

```sql
SELECT TABLEDEF('MOCK','T_CUSTOMER_MOCK');
```

```sql
CREATE TABLE "MOCK"."T_CUSTOMER_MOCK"
(
"ID" BIGINT NOT NULL,
"CUSTOMER_NO" VARCHAR(20),
...
"CREATED_TIME" DATETIME(6),
CONSTRAINT "PK_CUSTOMER_MOCK" NOT CLUSTER PRIMARY KEY("ID")) STORAGE(ON "TBS_MOCK", CLUSTERBTR) ;
```

列、类型、精度、主键、表空间、存储属性全部一致。此时表是空的：

```sql
SELECT COUNT(1) FROM MOCK.T_CUSTOMER_MOCK;   -- 0
```

### 6.5 装载

```bash
dmfldr USERID=SYSDBA/<PASSWORD>@127.0.0.1:5236 CONTROL="'MOCK_T_CUSTOMER_MOCK_data.ctl'"
```

> **那层双引号不是笔误。** dmfldr 拒绝解析含 `.` 的未加引号参数值，会报
> `parameters parse error[MOCK_T_CUSTOMER_MOCK_data]`（方括号里正是被点号截断后的前半截）。
> 而 POSIX shell 会把裸单引号吃掉，所以 `CONTROL='x.ctl'` 在 bash 里等价于不加引号、必然失败。
> 单引号必须用双引号包住才能活着送到 dmfldr。反过来，**不含点号的值不能加引号**——
> `DIRECT='TRUE'` 本身就是 parse error。

dmfldr 先回显它对控制文件的理解，这是一次免费的自检：

```bash
Rows per commit to server: 50000
Rows to skip: 0
Errors count allowed: 100
Whether to load direct: No
Character sets: UTF-8

Data file counts: 1
MOCK_T_CUSTOMER_MOCK_data.txt
Error file: MOCK_T_CUSTOMER_MOCK_data.bad
Dest table: MOCK.T_CUSTOMER_MOCK

Column Name          Packed data type     End
ID                   CHARACTER            X 01
CUSTOMER_NO          CHARACTER            X 01
...
CREATED_TIME         CHARACTER            X 01
```

**目标表、字符集、分隔符（`X 01`）、列顺序**都要跟你的预期对上再让它跑下去。然后：

```bash
50000 rows processed.
100000 rows processed.
...
10000000 rows processed.

Dest table: MOCK.T_CUSTOMER_MOCK
load success.
10000000 rows loaded success.
0 rows not loaded due to data error.
0 rows not loaded due to data format error.

Skip logic record counts: 0
Read logic record counts: 10000000
Refuse logic record counts: 0

107781.237(ms) time used.
```

三个零缺一不可：`data error`、`data format error`、`Refuse` 全是 0，
且 `loaded success` 等于 `Read logic record counts`。

装载完还要检查 `.bad` 文件——**不存在或大小为 0 才算干净**：

```bash
ls -l MOCK_T_CUSTOMER_MOCK_data.bad
```

### 6.6 校验

```sql
SELECT COUNT(1) FROM MOCK.T_CUSTOMER_MOCK;
```

```bash
1          10000000
```

---

## 七、两条通道的实测对照

| | DMP 通道 | dmfldr 通道 |
| --- | --- | --- |
| dmdul 卸载耗时 | 约 120 秒 | 约 92 秒 |
| 产物大小 | 1.66 GB（单个 `.dmp`） | 1.76 GB（`.txt`）+ 1.2 KB（`.ctl`） |
| 回灌耗时 | **79 秒** | 108 秒 |
| 是否需要先建表 | 否，`dimp` 一并重建 | 是，先跑 `_ddl.sql` |
| 是否恢复用户/权限/约束 | 是 | 否，只装数据 |
| 产物可读性 | 二进制，不可编辑 | 文本，可 `grep`/`awk`/切分 |
| 结果 | 10000000 行，0 告警 | 10000000 行，0 失败 |

### 怎么选

- **要连元数据一起搬** → DMP。用户、权限、约束、索引一条命令全带走。
- **超宽行（`STORAGE(USING LONG ROW)`）** → **必须** DMP。disql 从 stdin 读入时每行上限
  2499 字符，超宽行的 INSERT 语句根本喂不进去。
- **需要在装载前处理数据**（脱敏、筛选、改目标模式）→ dmfldr。文本文件可以随意加工，
  换目标模式只要改 `.ctl` 里的 `INTO TABLE` 一行。
- **`TIME` 类型带非零小数秒** → 不要走 DMP，DM 原生 DMP 通道不保存 `TIME` 的小数秒
  （dmdul 会打印告警）。走 SQL 或 dmfldr。
- **JSON/JSONB 走 DMP** → 必须加 `FAST_LOAD=N`。
- **几千行以内的小表** → 直接用 SQL 格式（`set data_format sql;`）最省事，
  产物可以人工审阅和修改。

---

## 八、这次实战暴露/验证的几个要点

1. **`detected:` 那一行是第一道关卡。** 库名、页大小、字符集、大小写敏感，四个里错一个，
   后面所有输出都不可信。
2. **`fallback reason: none` 才是干净的直读。** 一旦退化到扫描模式，覆盖范围就没有保证。
3. **数据库结构一变就要重新 bootstrap。** 本次 `dimp` 重建后 `TABLE_ID` 从 1384 变成 1389，
   拿旧字典去 unload 会按错误的物理坐标读页。
4. **`dimp SHOW=Y` 不能用来验证数据。** 它只解析元数据。大表必须跑真实导入加行数比对——
   dmdul 的 8 MiB phase 边界 bug 就是这么漏检的。
5. **dmfldr 的参数引号规则反直觉。** 含 `.` 的值必须带字面单引号（在 shell 里写成
   `CONTROL="'x.ctl'"`），不含 `.` 的值反而不能加引号。
6. **dmfldr 的分隔符只能用控制字符。** 它既无包围符也无转义，可打印分隔符无法与列内容区分。
7. **校验不能省。** `rows failed: 0` 只说明 dmdul 自己没报错，不代表数据对。
   源库还在就做双向 `MINUS`（**千万行级的表要按主键分块比，整表 MINUS 能把小内存实例
   OOM 掉**）；源库没了就核对行数、聚合值和主键连续性。

---

## 相关文档

- [离线恢复标准流程](recovery-workflow.md) —— 含一致性快照获取、预检、校验、常见坑对照表
- [使用示例](usage.md) —— 各命令的完整参数说明
- [DM8 数据类型支持矩阵](data-types.md) —— 每种类型在三条通道上的支持情况
- [DM8 DMP 逻辑导出格式实验记录](dmp-format-research.md) —— DMP 格式逆向细节
