# 本地开发、测试、构建说明

## 代码结构

```text
cmd/dmdul/          CLI 入口
internal/cli/       命令行解析、参数默认值、控制台输出
internal/dm/        达梦文件解析、DDL 导出、数据导出
internal/version/   版本字符串
docs/               用户文档和逆向研究笔记
research/           临时实验脚本和研究材料
```

## 常用开发命令

运行测试：

```powershell
go test ./...
```

构建 Windows 可执行文件：

```powershell
go build -o .\bin\dmdul.exe .\cmd\dmdul
```

查看帮助：

```powershell
.\bin\dmdul.exe help
```

启动交互式界面：

```powershell
.\bin\dmdul.exe
```

## 本地验证建议

建议准备一个不提交到 Git 的样例目录，例如：

```text
oldpro/
  SYSTEM.DBF
  dm.ctl
  MAIN.DBF
  TBS_BIN_TEST01.DBF
```

执行交互式验证：

```text
DMDUL> set system oldpro\SYSTEM.DBF;
DMDUL> set data_dir oldpro;
DMDUL> bootstrap;
DMDUL> list user;
DMDUL> list table SYSDBA;
DMDUL> unload table SYSDBA.T;
```

如果样例表名不同，可以先用 `list table <owner>;` 找到实际表名，再执行 `unload table`。

功能验证统一通过交互式命令完成。底层解析器应直接由 Go 单元测试或集成测试覆盖，
不再通过一次性功能子命令暴露。

## 版本信息

`internal/version/version.go` 里的 `Version` 是没有 `-ldflags` 注入时的兜底值，发版时
同步成当前 tag。正式发布构建通过 `-ldflags` 注入版本号、提交号和构建日期。

### 发布构建

**`-s -w` 不能漏。** 它去掉符号表和 DWARF 调试信息，二进制从约 6.2 MB 降到约 4.5 MB，
压缩包从约 3.4 MB 降到约 1.9 MB（v0.6.4 实测）。历史发布包都是带这两个 flag 构建的，
漏掉会让新版本看起来凭空"胖"了一大圈。`-s -w` 与 `-X` 注入不冲突，`dmdul version`
照常打印完整版本串。

先在干净工作区完成版本提交并创建 tag。以下命令从 `HEAD` 上的精确 tag 读取版本号，
如果尚未打 tag 会立即停止，避免二进制、压缩包名称和源码版本不一致。三段命令要在
**同一个 PowerShell 会话**里依次执行，后两段复用第一段定义的 `$ldflags`：

```powershell
$ver = (git describe --tags --exact-match HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or !$ver) { throw "HEAD must have an exact release tag" }
$commit = git rev-parse --short HEAD
$buildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$ldflags = "-s -w -X dmdul/internal/version.Version=$ver -X dmdul/internal/version.Commit=$commit -X dmdul/internal/version.BuildTime=$buildTime"
go build -trimpath -ldflags $ldflags -o bin\dmdul.exe .\cmd\dmdul
if ($?) { .\bin\dmdul.exe version }
```

```powershell
$env:CGO_ENABLED="0"
$env:GOOS="linux"
$env:GOARCH="amd64"
go build -trimpath -ldflags $ldflags -o bin\dmdul .\cmd\dmdul
Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH
```

```powershell
Compress-Archive -Path .\bin\dmdul.exe -DestinationPath .\bin\dmdul_windows_amd64_$ver.zip -Force; tar -czf bin\dmdul_linux_amd64_$ver.tar.gz -C bin dmdul; Get-FileHash .\bin\dmdul_windows_amd64_$ver.zip, .\bin\dmdul_linux_amd64_$ver.tar.gz -Algorithm SHA256 | Format-List Hash, Path
```

Windows PowerShell 5.1 没有 `&&` 和三元运算符，串联用 `;`，条件串联用 `if ($?) { }`。

`bin/` 在 `.gitignore` 里，构建产物不会进仓库。

### 本地开发构建

日常构建不需要注入，直接：

```powershell
go build -o bin\dmdul.exe .\cmd\dmdul
```

## 测试覆盖方向

当前重点测试方向：

- `SYSTEM.DBF` 页大小、字符集、页保护字节。
- `dm.ctl` 控制文件解析。
- 字典表对象、字段、索引、约束解析。
- DDL 类型格式化。
- 数据页 slot array、树表、堆表、行长、NULL、长 varchar、NUMBER、DATE/DATETIME 解码。
- CLI 默认参数和错误提示。

新增一种行格式或数据类型时，建议先在 `internal/dm/data_test.go` 或相关测试文件中添加最小样本，再实现解析逻辑。

## 逆向研究约定

- 临时脚本放在 `research/`。
- 已验证并进入主流程的规则记录到 `docs/offline-system-scan.md`。
- 系统字典字段含义记录到 `docs/system-dictionary-fields.md`。
- 不要把生产库文件、导出 SQL、含密码的配置、VM 配置或虚拟磁盘提交到仓库。

## 发布前检查清单

```powershell
go test ./...
go build -o .\bin\dmdul.exe .\cmd\dmdul
.\bin\dmdul.exe help
.\bin\dmdul.exe version
```

如果有样例文件，再执行：

```text
DMDUL> set system oldpro\SYSTEM.DBF;
DMDUL> set data_dir oldpro;
DMDUL> bootstrap;
DMDUL> list user;
DMDUL> unload user SYSDBA;
```
