# DM8 数据类型支持矩阵

本文记录 dmdul 对 DM8 常规表字段的离线 DDL 恢复、物理行解码以及 SQL、dmfldr、DMP
输出能力。结论以达梦官方类型文档为基线，并在独立 DM8 实例中通过建表、插入、
checkpoint、离线 bootstrap、卸载和回灌验证。

DM9 新增的 VECTOR 类型、页大小/字符集矩阵及三种输出通道的实机证据，参见
[DM9 兼容性验证](dm9-compatibility.md)。

## 支持范围

| 类型族 | 类型 | 当前状态 |
| --- | --- | --- |
| 字符 | `CHAR`、`CHARACTER`、`VARCHAR`、`VARCHAR2` | 支持 |
| 国家字符 | `NCHAR`、`NVARCHAR`、`NVARCHAR2` | 支持 |
| 标准字符别名 | `CHARACTER VARYING`、`NATIONAL CHARACTER`、`NATIONAL CHARACTER VARYING`、`NCHAR VARYING` | 支持 |
| 精确数值 | `NUMERIC`、`DECIMAL`、`DEC`、`NUMBER` | 支持，精度最高按字典定义恢复 |
| 整数 | `INTEGER`、`INT`、`PLS_INTEGER`、`BIGINT`、`SMALLINT`、`TINYINT`、`BYTE` | 支持 |
| 位串 | `BIT` | 支持 |
| 二进制 | `BINARY`、`VARBINARY`、`RAW` | 支持；`BINARY(n)` 按定长字段读取 |
| 近似数值 | `REAL`、`FLOAT`、`DOUBLE`、`DOUBLE PRECISION` | 支持 |
| 浮点兼容别名 | `BINARY_FLOAT`、`BINARY_DOUBLE` | 支持；系统字典通常归一为 `REAL`、`DOUBLE` |
| 日期时间 | `DATE`、`TIME`、`TIMESTAMP`、`DATETIME` | 支持，包括 9 位 TIMESTAMP 小数秒 |
| 时区 | `TIME WITH TIME ZONE`、`TIMESTAMP/DATETIME WITH TIME ZONE`、`TIMESTAMP WITH LOCAL TIME ZONE` | 支持 |
| 年月间隔 | `INTERVAL YEAR`、`INTERVAL MONTH`、`INTERVAL YEAR TO MONTH` | 支持 |
| 日时间隔 | `INTERVAL DAY`、`HOUR`、`MINUTE`、`SECOND` 及六种 `TO` 组合 | 支持，共 10 种限定符 |
| 多媒体文本 | `TEXT`、`LONG`、`LONGVARCHAR`、`CLOB`、`NCLOB` | 支持行内值和行外 LOB 页链 |
| 多媒体二进制 | `IMAGE`、`LONGVARBINARY`、`BLOB` | 支持行内值和行外 LOB 页链 |
| 外部文件 | `BFILE` | 支持恢复 directory/file locator；不复制操作系统文件 |
| JSON | `JSON`、`JSONB` | 支持 JSONB 物理载荷、标量、数组、对象和嵌套值 |
| 行标识 | `ROWID` | 支持 12 字节物理值及 18 字符显示值 |
| XML | `XMLTYPE` | 支持内容恢复；当前构建在系统字典中规范化为 `TEXT` |

日时间隔的十种限定符为：

```text
INTERVAL DAY
INTERVAL HOUR
INTERVAL MINUTE
INTERVAL SECOND
INTERVAL DAY TO HOUR
INTERVAL DAY TO MINUTE
INTERVAL DAY TO SECOND
INTERVAL HOUR TO MINUTE
INTERVAL HOUR TO SECOND
INTERVAL MINUTE TO SECOND
```

加上三种年月间隔后，共验证 13 种 INTERVAL 类型。

## 字典归一化

DM8 会把部分声明别名转换成统一的系统字典类型。dmdul 只能依据离线字典中实际保存的
定义生成 DDL，因此不保证保留用户最初输入的别名拼写。

| 建表时声明 | 当前测试实例中的字典类型 |
| --- | --- |
| `RAW` | `VARBINARY` |
| `FLOAT(1..24)`、`BINARY_FLOAT` | `REAL` |
| `BINARY_DOUBLE` | `DOUBLE` |
| `LONG` | `CLOB` 或 `TEXT` |
| `NCLOB` | `TEXT` |
| `XMLTYPE` | `TEXT` |
| `TIME WITHOUT TIME ZONE` | `TIME` |
| `TIMESTAMP WITHOUT TIME ZONE` | `TIMESTAMP` |

这些变化不会丢失字段数据，但恢复后的 DDL 可能使用等价的规范类型。

## 当前实例不接受的类型

在本次 DM8 构建、`COMPATIBLE_MODE=0` 的普通基表测试中：

| 声明 | 结果 | 建议替代 |
| --- | --- | --- |
| `BOOLEAN` / `BOOL` | `Invalid data type` | `BIT` |
| `MONEY` | `Invalid base class name` | `DECIMAL(19,4)` |
| `LONG RAW` | 语法错误 | `LONGVARBINARY` 或 `BLOB` |
| `DATETIME WITH LOCAL TIME ZONE` | `Invalid data type` | `TIMESTAMP WITH LOCAL TIME ZONE` |

这类兼容语法可能受 DM 版本或 `COMPATIBLE_MODE` 影响。dmdul 对实际字典中出现的
`BOOLEAN`、`BOOL`、`BINARY_FLOAT`、`BINARY_DOUBLE`、`LONG RAW` 仍保留兼容解析路径。

## 输出格式差异

### SQL

SQL 格式保持完整时间精度，并使用 `DATE`、`TIME`、`TIMESTAMP`、`INTERVAL`、
`HEXTORAW`、`BFILENAME` 和 `CAST(... AS JSONB)` 等类型化字值。

### dmfldr（`.txt` + `.ctl`）

分隔文本统一输出为 UTF-8，并为每张表生成配套的 dmfldr 控制文件。控制文件固定声明
`CHARACTER_CODE = 'UTF-8'`，该参数描述输入文件编码，而不是目标数据库字符集；dmfldr
负责转换到 GB18030、UTF-8 或 EUC-KR 目标库。以下规则由 DM8 与 DM9 实例上的官方
dmfldr 实测确定：

- 二进制字段写为不带 `0x` 的大写十六进制，控制文件用 `BLOB_TYPE = 'HEX_CHAR'` 还原为
  字节。注意不能用 `'HEX'`：那会把十六进制字符本身存进 BLOB，长度翻倍。
- NULL 写为 `\N`，控制文件用 `NULL_MODE = TRUE` + `NULL_STR` 声明；空字段因此
  表示空字符串，NULL 与空字符串可以区分。字段值本身等于该标记时该行会被判为失败行。
- 分隔符按列类型选择：无字符类型列时用 `|` + LF，否则用 SOH（`0x01`）+ STX+LF
  （`0x02 0x0A`）。dmfldr 没有可用的包围符或转义机制（`ENCLOSED BY` 是语法错误，
  `ESCAPED BY` 不做反转义），只能靠分隔符本身不可能出现在数据里来保证分帧。
- 列清单不带 `FORMAT` 子句。所有恢复类型在 dmfldr 默认解析下都能正确装载，而写死的
  格式串会在列的小数秒精度不同时立刻失败。
- 时间类型的小数秒截到 6 位。dmdul 按列声明精度渲染，DM 最多存 6 位，dmfldr 会直接
  拒绝更长的小数秒。

### DMP

- 常规标量、13 种 INTERVAL、ROWID、BFILE、JSON/JSONB、文本和二进制 LOB 已通过
  官方 `dimp` 回灌。
- DM8 原生 DMP 通道不保存 `TIME` 的小数秒。dmdul 检测到非零小数秒时会打印告警；
  要无损恢复该字段应使用 SQL 或 dmfldr。
- 单表数据超过 8 MiB 时会跨多个数据 phase。phase 边界只在行与行之间切,行内不分片
  (与官方 `dexp` 一致);此前在精确 8 MiB 处硬切会让 `dimp` 报 `data abnormal` 并放弃
  整张表,v0.6.4 及更早版本导出的大表 DMP 需要用新版本重新导出。
- JSON/JSONB 必须使用 `FAST_LOAD=N`。本次测试中，`FAST_LOAD=Y` 对 dmdul 和官方
  `dexp` 生成的 DMP 都会写入不可查询的 JSONB 内容。
- BFILE 只恢复 locator；目标库必须预先创建同名 DIRECTORY，并准备对应外部文件。

JSON/JSONB 导入示例：

```bash
dimp SYSDBA/password \
  FILE=SYSDBA_DMDUL_T_JSON.dmp \
  FAST_LOAD=N
```

## 验证记录

2026-07-13 使用 DM8 `03134284336-20250117-257733-20132`、8 KiB 页面、UTF-8
实例完成以下验证：

1. 创建覆盖上述常规类型的测试表，并同时插入非 NULL 边界值和全 NULL 行。
2. 执行 checkpoint 后复制 `SYSTEM.DBF`、`MAIN.DBF` 和 `dm.ctl`。
3. 仅从离线文件执行标准两阶段 bootstrap 和逐表卸载。
4. SQL 路径恢复 13 张类型表，DDL、主键和数据脚本在隔离用户中全部执行成功。
5. 分隔文本路径逐文件校验字段数、行数和关键边界值。
6. DMP 路径由官方 `dimp` 回灌；JSON/JSONB 使用 `FAST_LOAD=N`，其他样例使用
   `FAST_LOAD=Y`。
7. 对 JSONB 额外验证 15 个标量、空容器、数组、对象及嵌套样本；对 ROWID 验证
   站点号、分区号和 48 位物理行号边界。

2026-07-22 补充 dmfldr 通道端到端验证：从离线快照卸载 DULTEST 的 9 张表共 53064 行，
用官方 `/dm8/bin/dmfldr` 装载进独立用户后逐表 `MINUS` 双向比对，差异为 0；CLOB
（152 KiB）、BLOB（128 KiB）、JSON、含换行与竖线的文本、13 种 INTERVAL、
TIMESTAMP(9) 与带时区时间戳全部一致，dmfldr 报告 0 行失败。

## 不属于常规标量的类型

`%TYPE`、`%ROWTYPE`、记录、数组、集合、类和用户自定义类型属于 DM SQL
过程语言或 `CREATE TYPE` 对象，不是普通内建标量列类型。当前版本可恢复过程源码，
但尚未承诺自定义类型对象及其表列数据的完整物理恢复；遇到这类对象应单独验证。
