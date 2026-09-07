# 无完整系统字典的存储扫描与人工恢复

当 `SYSOBJECTS`、`SYSCOLUMNS` 损坏到无法完成 bootstrap 时，`scan storage;` 可以先列出
数据页的物理归属，再用人工列定义尝试解码。该路径不加载 `SYSTEM.DBF` 字典，不读取
`dm.ctl`、`control.dul` 或残留的 `dmdul_dict`。输出的表名由操作者指定，不是恢复出的原名。

本节描述 v0.10.0 新增能力。只支持文件系统中的普通 DBF；ASM 文件应先用 `cp` 复制为
DBF。HUGE/HFS 不进入此救援路径。

## 1. 扫描实际文件

把同一离线快照的 DBF 放在一个目录中。不要混放不同数据库、不同时间点或同一文件的
多个副本。`ROLL.DBF` 可选，用于行尾 Undo 指针的诊断，不用于判断提交状态。

```text
DMDUL> set data_dir D:\recovery\dbf;
DMDUL> set output_dir D:\recovery\rescue;
DMDUL> scan storage;
```

`storage_scan;` 是同一命令的别名。无需先执行 `bootstrap;` 或 `load dictionary;`。

扫描按页读取，使用文件头及多页证据确定页大小，用实际页头投票确定 group/file。
身份不唯一、重复 group/file 或页大小证据冲突会停止扫描。符号链接被拒绝，避免读取恢复目录
之外的文件。截断尾页、页身份和行结构异常写入错误清单。

| 文件 | 内容 |
| --- | --- |
| `storage_scan.tsv` | 按实际文件、group/file、storage_id、page_kind 分组的页数、首尾页、slot 行数 |
| `storage_samples.tsv` | 每组至多 3 条样例，含页号、slot、偏移、删除位、最多 256 字节原始前缀、事务尾与 Undo 诊断 |
| `storage_errors.tsv` | 截断尾页、页身份冲突、行结构异常及坐标 |

`attribution` 固定为 `UNATTRIBUTED`，不会推测 owner/table。`slot_rows` 是非删除 slot
记录数，不代表已提交行数。首尾页只是范围统计，不保证其中每页都属于该对象。
扫描包含 `0x14/0x15/0x16/0x20/0x22` 类页；branch、LOB、Long Row 页不作为普通行样例。
19 字节事务尾仅适用于已经验证的普通行布局，无字典样例中的事务字段仍须核对。

此清单不是完整坏块报告。需要校验 checksum 时执行 `check pages;`；结构合理的页也可能
存在列值损坏。重新扫描会更新这三个报告，重要实验报告应使用不同 `output_dir` 保存。

## 2. 提供列定义

先从建表备份、应用代码或人工分析确定列结构。文件必须是 UTF-8、TAB 分隔，表头严格为：

```tsv
col_id	name	data_type	length	scale	nullable
0	ID	INT	4	0	N
1	VALUE_TX	VARCHAR	100	0	Y
```

- `col_id` 从 0 连续编号，列名不重复；最多 1024 列，文件最多 1 MiB。
- `data_type` 使用解析器支持的基本类型名；长度和精度分别填写 `length`、`scale`。
- `nullable` 填 `Y` 或 `N`。列的可空属性会影响物理解码，不能凭当前样例是否有 NULL 推断。
- 定义应匹配该 storage 的实际行版本。此路径不猜测历史增删列，也不尝试不同 metadata 起点。

可以使用编辑器手动维护文件。不要把 TSV 中的 TAB 换成对齐用的空格。

## 3. 按 storage 恢复

假设扫描得到 `group_id=4`、`storage_id=33555471`：

```text
DMDUL> set charset utf-8;
DMDUL> set data_format sql;
DMDUL> recover storage 4.33555471 using D:\recovery\columns.tsv as RECOVERED.T;
```

字符集必须明确指定为 `utf-8/gb18030/gbk/euc-kr`，不能用 `auto`。支持 `sql/fldr/dmp`；
DMP 还要求从 DBF 头取得有效的簇大小，并明确 `set case_sensitive 0;` 或
`set case_sensitive 1;`，不从残留字典猜测该标志。路径含空格时给列定义路径加引号。

以上命令在 `output_dir` 生成：

```text
RECOVERED_T_storage_4_33555471.sql
RECOVERED_T_storage_4_33555471.sql.evidence.tsv
```

切换 fldr 时生成 `.txt` 和 `.ctl`；DMP 时生成 `.dmp`。空表不生成 fldr/DMP 数据文件。
该命令只恢复数据，不生成或执行建表 DDL，需先在验证库人工创建目标表。
所选 group 存在截断文件或待恢复页结构不可靠时会停止，不静默跳过后报告成功；扫描报告仍可
用于进一步人工分析。

证据 TSV 逐行记录源文件、group/file、storage_id、page、slot、行偏移、删除位、解码结果
和失败原因。归属标记为 `OPERATOR_SUPPLIED`。完成前数据写入临时目录，结束时以硬链接发布；
已有同名输出会报错，不覆盖。输出目录所在文件系统须支持硬链接（如 NTFS、ext4）。

默认只取 slot 指向的非删除行。确需读取删除或无 slot 残留时显式添加：

```text
DMDUL> recover storage 4.33555471 using D:\recovery\columns.tsv as RECOVERED.T residual;
```

残留模式可能同时读出旧版本、已回滚值及重用前的数据，必须人工去重核对。不要将 residual
输出当作某个时间点的完整表。

## 事务边界

两种模式都不是 committed-only。未提交 INSERT 可以出现在 slot 中，UPDATE 的当前值可能
尚未提交，DELETE 的旧值可能只存在于 Undo。当前只追踪经过验证的 `0x1D` Undo 公共记录头；
缺页、事务号不匹配、页复用、未知 opcode、指针环和深度超限都会明确停止追踪。

`END (visibility unknown)` 只表示指针追踪抵达空地址，不表示已恢复提交前镜像。
详见 [事务与 Undo 差分证据](transaction-undo-evidence.md)。
