# 使用示例

以下示例假设程序位于 `.\bin\dmdul.exe`。数据库扫描、字典加载、DDL 导出、
数据恢复全部通过交互式入口完成，不再提供功能性命令行子命令。

## 准备目录

建议把可执行文件、参数文件和离线数据库文件放在同一个工作目录，或者在交互界面中用
`set` 命令指定路径：

```text
dmdul.exe
init.dul
control.dul
dmdul_dict/
output/             # 首次 unload/recover 时自动创建
dul.log
SYSTEM.DBF
MAIN.DBF
TBS_*.DBF
```

`dm.ctl` 是可选增强文件。有它时可以补充数据库名、表空间名和数据文件路径；没有它时，
数据抽取会根据 DBF 页头识别 `(表空间号, 文件号)`。
`bootstrap` 会优先从 SYSTEM.DBF 的 `SYS.SYSOPENHISTORY` 恢复 `INSTANCE_NAME`；
数据库从未成功打开或历史页不可用时，再读取同目录 `dm.ini`，最后使用默认值
`DMSERVER`。这不影响离线字典和数据恢复。

## 启动 DMDUL

```powershell
.\bin\dmdul.exe
```

进入交互界面后会看到提示符：

```text
DMDUL>
```

查看帮助：

```text
DMDUL> help;
```

## 设置参数

如果当前目录下就有 `SYSTEM.DBF`，可以直接执行 `bootstrap;`。否则建议先确定工作目录，
再指定 SYSTEM 和可选控制文件：

```text
DMDUL> set data_dir D:\temp\oldpro;
DMDUL> set system D:\temp\oldpro\SYSTEM.DBF;
DMDUL> set control D:\temp\oldpro\dm.ctl;
DMDUL> set output_dir D:\temp\oldpro\out;
DMDUL> set data_format sql;
DMDUL> set case_sensitive auto;
DMDUL> set charset auto;
```

### DMASM 裸盘模式

数据库文件位于离线 DMASM 成员盘时，使用 ASM 逻辑路径代替普通 `SYSTEM.DBF` 路径：

```text
DMDUL> set asm_disk /dev/dmasm/ext4a,/dev/dmasm/ext4b,/dev/dmasm/norm4a,/dev/dmasm/norm4b,/dev/dmasm/ext32a;
DMDUL> set system +NORM4/data/MIRRORDB/SYSTEM.DBF;
DMDUL> list asmfile;
DMDUL> list datafile;
DMDUL> bootstrap;
```

`list asmfile` 从 INODE 列出逻辑文件；`list datafile` 读取各 DBF 的第 0 页，恢复
tablespace group、file id、页数和大小。后续 `unload` 仍使用同一套 storage root、
page refs 和 leaf chain，不会复制中间 DBF。多成员盘和多磁盘组成员统一用逗号分隔，
解析器按磁盘头 group id 分组；镜像组会结合 AU 1 generation 和 AU 2 当前成员表排除
OFFLINE、RECONNECT、DELETED 及替换后的旧盘，再按 `+GROUP/...` 路由逻辑文件。

VMware `monolithicFlat` 冷副本可以直接使用 `*-flat.vmdk`，不要传只包含描述信息的
`.vmdk` descriptor 文件。当前已验证 DMASM 非镜像环境以及镜像环境的
1/4/32 MiB AU、EXTERNAL/NORMAL/HIGH 和 0/32 KiB 条带组合。

裸设备必须来自同一时点的一致性副本。对 DMDSC 共享盘，先停止所有节点的数据库和 ASM
写入，或使用存储快照；不要把运行中读取成功当成一致性保证。当前支持范围和物理格式见
[DMASM 裸盘离线读取与恢复](dmasm-raw-recovery.md)。

查看当前参数：

```text
DMDUL> show parameter;
```

## 加载字典

```text
DMDUL> bootstrap;
```

`bootstrap` 会读取 `SYSTEM.DBF`，识别页大小、簇大小、页数、字符集、大小写敏感标志，
从可选 `dm.ctl` 获取数据库名，并从 `SYS.SYSOPENHISTORY` 最新有效记录获取实例名，
然后加载用户、表、字段等字典信息。
同时会扫描 `data_dir` 下的 DBF 页头，生成 `control.dul` 数据文件清单，并把字典摘要写入
`dmdul_dict` 文本目录。成功后会把所有基础数据库参数及来源回写到 `init.dul`。
成功后才能继续执行 `list` 和 `unload`。

若工作目录中已有 `dmdul_dict`，bootstrap 不会读取它参与本次扫描。新字典先写入同级
临时目录并完整校验，旧目录随后备份为 `dmdul_dict.backup-YYYYMMDD-HHMMSS`。只有新目录
切换成功后才会更新会话中的内存字典；任何 bootstrap 失败都会保持字典未加载状态。
备份目录不会被 `load dictionary` 自动读取，确认新字典可用后可自行归档或删除。

`dmdul_dict` 目录当前包含：

```text
meta.tsv
users.tsv
schemas.tsv
tables.tsv
columns.tsv
partitions.tsv
partition_keys.tsv
views.tsv
sequences.tsv
routines.tsv
triggers.tsv
synonyms.tsv
tab_privs.tsv
```

这些文件可以人工审查和修正。再次进入 DMDUL 后，可以不重扫 `SYSTEM.DBF`，直接加载文本字典：

```text
DMDUL> load dictionary;
```

加载后的 `dmdul_dict` 会真正参与后续恢复：DDL 和数据导出时，用户、表名、字段名、字段类型、
默认值、表空间、存储组织、分区键、分区边界、视图、序列、存储过程/函数/包、触发器、同义词和对象授权会优先使用文本字典中的内容；
`SYSTEM.DBF` 仍用于辅助定位索引、约束和数据页等物理信息。Windows 下手工保存为带
UTF-8 BOM 的 TSV 文件也可以正常读取。

`tables.tsv` 还预留了 `header_file`、`header_block`、`bytes`、`blocks`、`extents`
字段，对应在线 `DBA_SEGMENTS` 的段信息。补齐这些字段后，数据抽取会按段页范围过滤候选页，
能减少同名表或相似行格式造成的误匹配。

`schemas.tsv` 保存模式及其所属用户。它使 DMP 的 OWNER 与 SCHEMAS 两种逻辑级别可以
正确区分；旧字典没有该文件时，会按“一个用户对应一个同名默认模式”兼容加载。

如果内存中还没有字典，执行 `list user`、`list schema`、`list table`、`unload table`、
`unload object`、`unload user`、`unload schema`、`unload database` 前也会尝试自动从
`dmdul_dict` 加载。

因此推荐二选一：

1. **重新扫描 DBF**：`set data_dir` → `set system/control` → `show parameter` →
   `bootstrap` → 检查 TSV → 必要时 `load dictionary` → `unload`。
2. **复用已审核字典**：`set data_dir` → `load dictionary` → 核对 `show parameter`、
   `list user` 和 `list table` → `unload`，不要再执行 bootstrap 覆盖人工修订版。

## 查看用户、模式和表

列出用户/owner：

```text
DMDUL> list user;
```

`list user` 会先显示当前字典来源、字典目录和字典行数统计，再按用户汇总其模式、表、
视图、同义词、序列、触发器、函数、过程和包数量。

列出全部模式，或只列出某个用户拥有的模式：

```text
DMDUL> list schema;
DMDUL> list schema HR_TEST;
```

列出某个用户下的表：

```text
DMDUL> list table HR_TEST;
```

输出中会显示表名、table id、字段数、表空间、存储组织和是否分区。

## 恢复单表

```text
DMDUL> unload table HR_TEST.EMP_INFO;
```

默认会在启动 DMDUL 时的当前目录创建 `output` 子目录并生成文件，不会因为设置了
`data_dir` 而改到数据库文件目录。`control.dul`、`dul.log`、`init.dul` 和 `dmdul_dict`
仍按 `data_dir` 确定位置。显式执行 `set output_dir <directory>;` 时，卸载结果直接写入
指定目录，其他工作文件不移动。

```text
output/HR_TEST_EMP_INFO_ddl.sql
output/HR_TEST_EMP_INFO_data.sql
```

命令末尾会显示本次数据页定位和物理读取统计，例如：

```text
planned pages: 12
direct pages read: 12
fallback pages scanned: 0
fallback reason: none
```

正常路径只用 `ReadAt` 读取 page plan 中的页。计划不完整或计划页校验失败时，依次回退到
同 group 的 `storage_id` 扫描和字典段范围；只有 `recover table` 会启用全数据文件残留页扫描。
相同统计及具体回退原因也会写入 `dul.log`。

这里的 `storage_id` 回退只扩大“候选页面”的定位范围，普通 `unload` 在每页内仍严格只读取
slot 目录指向且未带 DELETE 标志的行。无 slot 物理空洞、删除行和旧版本残留只由显式
`recover table` 扫描，二者不会因为 page-plan fallback 而混在一起。slot-only 仍不等于事务一致性读：
在离线事务状态和 Undo PRE IMAGE 尚未完整解析前，普通卸载无法可靠判定未提交 INSERT/DELETE 的
最终可见性。

也可以指定输出前缀：

```text
DMDUL> unload table HR_TEST.EMP_INFO to emp_info;
```

生成：

```text
output/emp_info_ddl.sql
output/emp_info_data.sql
```

导出 dmfldr 数据（分隔文本 + 控制文件）：

```text
DMDUL> set data_format fldr;
DMDUL> unload table HR_TEST.EMP_INFO;
```

生成：

```text
output/HR_TEST_EMP_INFO_ddl.sql
output/HR_TEST_EMP_INFO_data.txt
output/HR_TEST_EMP_INFO_data.ctl
```

回灌时先用 `_ddl.sql` 建表，再用官方 dmfldr 装载数据。控制文件里的字段/行分隔符、
NULL 标记、字符集和 BLOB 编码都已按数据文件写死，通常不需要改动：

```bash
dmfldr USERID=SYSDBA/password@127.0.0.1:5236 CONTROL="'HR_TEST_EMP_INFO_data.ctl'"
```

那层双引号不是笔误：dmfldr 拒绝解析含 `.` 的未加引号参数值（报
`parameters parse error[...]`，方括号里是被点号截断后的前半截），而 shell 会把裸单引号
吃掉，单引号必须用双引号包住才能活着送到 dmfldr。不含点号的值反而不能加引号——
`DIRECT='TRUE'` 本身就是 parse error。

分隔符由表的列类型决定，控制文件与数据文件永远一致：

- 所有列都不可能产生 `|`、CR、LF（数值、日期时间、INTERVAL、十六进制二进制）时，
  使用可读的 `|` 分隔、LF 换行。
- 只要有一个字符类型列，就改用 SOH（`0x01`）分隔、STX+LF（`0x02 0x0A`）换行。
  dmfldr 既不支持包围符也不支持转义（`ENCLOSED BY` 是语法错误，`ESCAPED BY` 不做
  反转义），可打印分隔符无法与列内容区分，因此文本表只能用控制字符分隔。dmdul 从不
  在字段值中写入 C0 控制字符，所以这组分隔符不会与数据冲突。

导出达梦原生逻辑 DMP：

```text
DMDUL> set data_format dmp;
DMDUL> unload table HR_TEST.EMP_INFO;
```

生成：

```text
output/HR_TEST_EMP_INFO_ddl.sql
output/HR_TEST_EMP_INFO.dmp
```

DMP 使用 `bootstrap` 识别出的数据库字符集，支持 UTF-8、GB18030 和 EUC-KR。
文件同时保存表定义、索引、约束、注释、触发器、授权和数据，可以直接通过 `dimp`
恢复。`_ddl.sql` 是审计和人工修订副本，正常导入前不需要先执行，避免对象已存在冲突。

```text
dimp SYSDBA/password FILE=HR_TEST_EMP_INFO.dmp SHOW=Y NOLOGFILE=Y
dimp SYSDBA/password FILE=HR_TEST_EMP_INFO.dmp CTRL_INFO=4 NOLOGFILE=Y
dimp SYSDBA/password FILE=HR_TEST_EMP_INFO.dmp FAST_LOAD=Y
```

JSON/JSONB 表是例外，必须使用 `FAST_LOAD=N`；当前测试中官方 `dexp` 文件也会在
`FAST_LOAD=Y` 后产生不可查询的 JSONB 内容。
DM 当前 DMP 行格式不能保存 `TIME` 的小数秒；遇到非零小数秒时 DMDUL 会打印明确告警。
`case_sensitive=auto` 会读取 `SYSTEM.DBF` 第 4 页偏移 `0x2C` 的建库大小写敏感标志，
并写入 DMP 文件头，不依赖 `dm.ini`。若文件损坏导致该控制字节无法识别，可执行
`set case_sensitive 0|1;` 显式覆盖，避免 `dimp` 等待参数确认。

## DROP / TRUNCATE 残留页恢复

`recover table` 用于在表被 `TRUNCATE`、`DROP` 或 DELETE 后，数据块尚未被新写入覆盖时，扫描
数据文件中的残留行。它会使用字典中的字段定义解码行，按页头里的 storage/assist id 过滤候选页，
并遍历活动 slot 之外的物理行区。恢复结果可能包含删除、旧版本或未提交残留，不等价于数据库一致性读。

TRUNCATE 场景中，表结构通常仍在当前 `SYSTEM.DBF` 中：

```text
DMDUL> bootstrap;
DMDUL> recover table USERS1.T_TEST;
```

DROP 场景中，当前字典里可能已经没有表定义，需要先加载 DROP 前保存的 `dmdul_dict`，或人工在 `tables.tsv`、`columns.tsv` 中补齐表结构和 `storage_id/assist_ids`：

```text
DMDUL> load dictionary;
DMDUL> recover table USERS1.T_TEST to users1_t_test_drop_recover;
```

## 导出对象字典

`unload object` 只生成对象定义，不扫描和导出用户表数据：

```text
DMDUL> unload object HR_TEST;
```

默认生成 `output/HR_TEST_objects.sql`，其中包含该 owner 可恢复的用户定义、角色授权、表、
索引、约束、注释、视图、序列、存储过程、函数、包、触发器、同义词和对象权限。
导出全部 owner 的对象字典：

```text
DMDUL> unload object all;
```

默认生成 `output/DATABASE_objects.sql`。也可以使用 `to` 指定输出前缀：

```text
DMDUL> unload object HR_TEST to hr_test_dictionary;
```

## 恢复一个用户的表

```text
DMDUL> unload user HR_TEST;
```

`unload user` 只负责该用户拥有的表。每张表按表名生成自己的建表 DDL 和数据文件；
表 DDL 包含表、索引、约束、注释、表触发器和表权限，不再重复输出视图、序列、函数、
包、同义词或用户定义。例如 SQL 格式会生成：

```text
output/HR_TEST_EMP_INFO_ddl.sql
output/HR_TEST_EMP_INFO_data.sql
output/HR_TEST_T_LOG_HEAP_ddl.sql
output/HR_TEST_T_LOG_HEAP_data.sql
```

如果 `data_format=fldr`，每张表仍会生成自己的 DDL，数据写入对应 `.txt` 并附带同名
`.ctl`；没有数据的表只保留 DDL，不生成空数据文件。例如：

```text
output/HR_TEST_EMP_INFO_ddl.sql
output/HR_TEST_EMP_INFO_data.txt
output/HR_TEST_EMP_INFO_data.ctl
output/HR_TEST_T_LOG_HEAP_ddl.sql
```

如果 `data_format=dmp`，`unload user HR_TEST;` 对应官方 OWNER 级别，生成：

```text
output/HR_TEST_ddl.sql
output/HR_TEST.dmp
```

一个 DMP 包含该用户、其拥有的全部模式、当前可恢复的模式对象、表对象和数据；空表也会
保留建表元数据。可用逗号同时选择多个用户，例如 `unload user U1,U2;`。超大 LOB 从当前
活动行的 locator 出发逐页流式写入，不会先把整个 LOB 读入内存；超过 4 GiB 的整表数据
通过多个 DMP phase 持续输出。

整个用户只读取一次 SYSTEM.DBF 字典，并为所有选中表统一生成 page plan。计划完整时仅直接读取
计划页并按表分流写入；需要回退时，同一 group 数据文件也只扫描一次，不会因表数量增加而
为每张表重复扫描完整 DBF 文件集。

也可以指定输出前缀：

```text
DMDUL> unload user HR_TEST to hr_test_all;
```

SQL/fldr 格式下文件前缀会变为 `hr_test_all_<table_name>`；DMP 格式下生成
`hr_test_all_ddl.sql` 和 `hr_test_all.dmp`。

`unload user all;` 已移除。整库导出统一使用 `unload database;`，避免两个命令表达同一操作。

## 恢复一个或多个模式

模式级对应官方 SCHEMAS 逻辑级别，当前仅用于 DMP：

```text
DMDUL> set data_format dmp;
DMDUL> unload schema HR_TEST;
DMDUL> unload schema HR_TEST,ARCHIVE to business_schemas;
```

默认生成一个 `<prefix>_ddl.sql` 和一个 `<prefix>.dmp`。SCHEMAS 只包含明确选择的模式；
OWNER 则包含选中用户及该用户拥有的全部模式，两者不是同义命令。

## 恢复整库

```text
DMDUL> unload database;
```

默认生成：

```text
output/DATABASE_ddl.sql
output/DATABASE_data.sql
```

`DATABASE_ddl.sql` 包含可识别用户、用户表、视图、序列、存储过程/函数/包、触发器、同义词和表/视图/序列授权 DDL；`DATABASE_data.sql`
包含所有可识别用户表的 INSERT 数据。

如果 `data_format=fldr`，`unload database` 会生成一个全库 DDL 文件，并按 owner/table
分别生成 `.txt` 数据文件和 `.ctl` 控制文件；没有数据的表不会生成空数据文件。例如：

```text
output/DATABASE_ddl.sql
output/DATABASE_HR_TEST_EMP_INFO_data.txt
output/DATABASE_HR_TEST_EMP_INFO_data.ctl
output/DATABASE_SYSDBA_T_data.txt
output/DATABASE_SYSDBA_T_data.ctl
```

如果 `data_format=dmp`，对应官方 FULL 级别，生成一个同时包含全部可恢复元数据和数据的
全库逻辑 DMP：

```text
output/DATABASE_ddl.sql
output/DATABASE.dmp
```

空用户和空表也会保留必要元数据。若需要逐表重试，应使用 TABLES 级命令分别生成文件。

## DMP 四种逻辑级别

| 级别 | DMDUL 命令 | 一个文件包含的范围 |
| --- | --- | --- |
| `TABLES` | `unload table S.T1,S.T2;` | 一个或多个完整表及表相关对象和数据 |
| `OWNER` | `unload user U1,U2;` | 一个或多个用户、其拥有的全部模式及对象 |
| `SCHEMAS` | `unload schema S1,S2;` | 明确选择的一个或多个模式及对象 |
| `FULL` | `unload database;` | 全部可恢复用户、模式、对象和数据 |

四种级别由命令互斥选择。当前 TABLES 以完整父表为最小单位，分区表数据由 DM 在导入时
按分区键路由，暂不支持只导出一个叶子分区。

也可以指定输出前缀：

```text
DMDUL> unload database to dmdb_all;
```

## init.dul 示例

如果工作目录下存在 `init.dul`，DMDUL 启动时会自动读取；每次执行 `set`
命令后会同步写入：

```text
db_name=DAMENG
db_name_source=dm.ctl
instance_name=DMSERVER
instance_name_source=DM default
extent_size=16
extent_size_source=u32 @ 0x80
page_size=8192
page_size_source=u32 @ 0x84
page_count=9472
page_count_source=u32 @ 0x8C
database_charset=UTF-8 (UNICODE_FLAG=1)
unicode_flag=1
charset_source=SYSTEM.DBF page 4 + 0x2D
case_sensitive_value=0
case_sensitive_source=SYSTEM.DBF page 4 + 0x2C
system=D:\temp\oldpro\SYSTEM.DBF
control=D:\temp\oldpro\dm.ctl
data_dir=D:\temp\oldpro
output_dir=
data_format=sql
case_sensitive=auto
charset=auto
log=
```

如果运行过程中手工修改了 `init.dul`，可以重新加载：

```text
DMDUL> load parameter;
DMDUL> show parameter;
```

`load init;` 作为兼容别名仍然可用。

## 退出

```text
DMDUL> exit;
```

## 命令行边界

直接运行 `.\bin\dmdul.exe` 进入交互界面。当前只保留两个非恢复子命令：

```powershell
.\bin\dmdul.exe help
.\bin\dmdul.exe version
```

`inspect`、`inspect-ctl`、`scan-system`、`scan-partitions`、`export-ddl`、
`export-data` 均已移除。对应的解析和恢复能力由 `bootstrap`、`list`、`unload`
和 `recover` 等交互命令统一提供。

## 输出结果建议

导出的 SQL 建议按顺序使用：

1. 先审查并执行 DDL 文件。
2. 再审查并执行 INSERT 文件。
3. 对关键表做行数和抽样内容校验。
