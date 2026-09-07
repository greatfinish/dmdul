# v0.10.0 发布验证

日期：2026-09-07。主题：Recovery Hardening。

本版发布候选使用 Go 1.27.1 构建，在 DM8/DM9 隔离实验实例上重新执行以下验证。
原有主实例保持运行；每次读取实验 DBF 前，实验实例均已正常关闭。无字典救援另使用
先前为 Undo 差分保留的物理快照，该输出不表示提交视图。

## 自动化检查

| 检查 | 结果 |
| --- | --- |
| Windows Go 1.22.12 完整测试、vet | 通过 |
| Windows Go 1.27.1 完整测试、vet | 通过 |
| Linux Go 1.27.1 完整 race 测试、vet | 通过 |
| 五组 fuzz，各 30 秒、2 workers | 通过；页大小、slot、行 metadata、DMP、DMASM |
| Go 1.27.1 govulncheck | 0 个可达符号漏洞、0 个已导入包漏洞 |
| 提交内容检查 | 无测试密码、完整 DBF、DMP、虚拟磁盘和临时实验目录 |

govulncheck 仍列出 x/text v0.22.0 的模块级公告 GO-2026-5970，当前路径未调用涉及的
`unicode/norm`。这不是“所有依赖无漏洞”的承诺。详见 [开发说明](development.md)。

首次远端 CI 暴露 Windows 测试夹具问题：约 32 GiB 的 ASM 逻辑镜像未标记为稀疏文件，
`Truncate` 耗尽 runner 磁盘。测试已改为先设置 `FSCTL_SET_SPARSE`，并增加稀疏属性断言；
跨描述区读取用例继续完整执行。这项修复只涉及测试，不改变生产解析代码。

## DM9 参数矩阵

Build：`03151060506-20260417-322930-20218`。每组 3 行，覆盖 INT、VARCHAR、DECIMAL、DATE
及 NULL。各组均执行 SYSTEM 检查、全部 DBF 检查、SQL/fldr/DMP 导出及官方 dimp 导回；
导回后与源表双向 MINUS，缺失和多余记录均为 0。三字符集用例保留中文/韩文数据。
此小矩阵不代替 [DM9 完整对象矩阵](dm9-compatibility.md)。

| 页大小 | 字符集 | CASE_SENSITIVE | PAGE_CHECK | 结果 |
| --- | --- | --- | --- | --- |
| 4 KiB | UTF-8 | 0 | 0 | 通过 |
| 4 KiB | UTF-8 | 0 | 1 | 通过 |
| 4 KiB | UTF-8 | 0 | 3 | 通过 |
| 4 KiB | GB18030 | 0 | 3 | 通过 |
| 4 KiB | EUC-KR | 0 | 3 | 通过 |
| 8 KiB | UTF-8 | 0 | 2 / SM3 | 通过 |
| 16 KiB | UTF-8 | 0 | 2 / SM3 | 通过 |
| 32 KiB | UTF-8 | 0 | 2 / SM3 | 通过 |

每次干净文件检查均为 0 坏页，各格式导出 3 行、0 失败。
在单独复制的 8 KiB SM3 SYSTEM 文件中翻转 page 304 内的一个字节后，检查准确报告
唯一的 `page(0,0,304) CHECKSUM_FAIL`。源 DBF 没有修改。

4 KiB + SM3/SHA256 的初始化拒绝仍作为负样本保留，不声明支持这一组合。
同一 ISO 与已安装程序的 build 相同，第二个 DM9 build 尚未验证。

## DM8 内容与权限

Build：`03134284336-20250117-257733-20132`，独立 UNDOLAB，端口 5439。

- HUGE：2500 行，经 SQL 导入普通对照表后双向 MINUS 均为 0；总行数 2500，NI 非空 1667，
  NB 非空 1875。覆盖两个完整 HFS section 和 RAUX 尾部，含可空定长列。
- 列级权限：官方 dimp 导回后，3 项 UPDATE/REFERENCES 列权限与源 DBA_COL_PRIVS 双向比对为 0。
- 系统权限：OWNER DMP 重建实验用户后，CREATE SESSION/CREATE TABLE 为 NO，
  CREATE VIEW 的 ADMIN_OPTION 为 YES；原表中 ID=7 保留。dimp 报告无告警。
- 无字典救援：扫描 3 个文件、42240 页、358 个 storage 分组，异常页为 0；
  人工列定义恢复 4 行、0 失败，并生成逐行来源 TSV。不把其中未提交版本认作已提交数据。

## 日志与范围

完整 DBF、导出文件和实验日志不随源码发布。测试机保留：

- DM9：`/dmdata/dmdulmatrix0906/release0100-0907/`，逐例日志及 `results.tsv`。
- SM3 坏页复制件：`/dmdata/dmdulmatrix0906/release0100-sm3-corrupt/`。
- DM8：`/dmdata/dmdulundolab/release0100-0907c/`，导回和比对日志。
- 无字典救援：`/dmdata/dmdulundolab/release0100-rescue-0907/`。

正式包使用 `-trimpath -s -w`，注入 v0.10.0、提交号和 UTC 时间，并附带项目/第三方许可证。
完整 Undo、HUGE 压缩/多路径/校验和、ASM REDO/删除救援及扩展对象不计入本版支持范围，
见 [路线图](roadmap.md)。
