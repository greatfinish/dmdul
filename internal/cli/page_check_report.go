package cli

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"dmdul/internal/dm"
)

const (
	pageCheckSummaryName         = "check_summary.md"
	pageCheckBadPagesName        = "check_bad_pages.tsv"
	pageCheckAffectedObjectsName = "check_affected_objects.tsv"
)

type pageCheckReportContext struct {
	SystemPath         string
	DataDir            string
	FileFilter         []string
	FollowControlPaths bool
	GeneratedAt        time.Time
}

type pageCheckReportPaths struct {
	Summary         string
	BadPages        string
	AffectedObjects string
}

type pageCheckReportWriter struct {
	dir          string
	pageSize     uint32
	badTempPath  string
	badFile      *os.File
	badTSV       *csv.Writer
	badFinalized bool
}

func newPageCheckReportWriter(dir string, pageSize uint32) (*pageCheckReportWriter, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, ".check_bad_pages-*.tsv")
	if err != nil {
		return nil, err
	}
	w := &pageCheckReportWriter{
		dir:         dir,
		pageSize:    pageSize,
		badTempPath: file.Name(),
		badFile:     file,
		badTSV:      csv.NewWriter(file),
	}
	w.badTSV.Comma = '\t'
	if err := w.badTSV.Write([]string{
		"path", "tablespace", "group_id", "file_id", "page_no",
		"byte_offset", "byte_offset_hex", "storage_id", "corruption",
		"object_type", "owner", "table_name", "table_id", "object_storage_id",
		"object_group_id", "attribution", "confidence", "unattributed_reason", "detail",
	}); err != nil {
		w.abort()
		return nil, err
	}
	return w, nil
}

func (w *pageCheckReportWriter) writeBadPage(bad dm.BadPage) error {
	offset := uint64(bad.PageNo) * uint64(w.pageSize)
	return w.badTSV.Write([]string{
		bad.Path,
		bad.Tablespace,
		strconv.FormatUint(uint64(bad.GroupID), 10),
		strconv.FormatInt(int64(bad.FileID), 10),
		strconv.FormatUint(uint64(bad.PageNo), 10),
		strconv.FormatUint(offset, 10),
		fmt.Sprintf("0x%X", offset),
		strconv.FormatUint(uint64(bad.StorageID), 10),
		string(bad.Kind),
		string(bad.ObjectType),
		bad.Owner,
		bad.Table,
		strconv.FormatUint(uint64(bad.TableID), 10),
		strconv.FormatUint(uint64(bad.ObjectStorageID), 10),
		strconv.FormatUint(uint64(bad.ObjectGroupID), 10),
		string(bad.Attribution),
		string(bad.AttributionConfidence),
		bad.UnattributedReason,
		bad.Detail,
	})
}

func (w *pageCheckReportWriter) finalize(result *dm.PageCheckResult, context pageCheckReportContext) (pageCheckReportPaths, error) {
	paths := pageCheckReportPaths{
		Summary:         filepath.Join(w.dir, pageCheckSummaryName),
		BadPages:        filepath.Join(w.dir, pageCheckBadPagesName),
		AffectedObjects: filepath.Join(w.dir, pageCheckAffectedObjectsName),
	}
	if err := w.closeBadPages(); err != nil {
		w.abort()
		return pageCheckReportPaths{}, err
	}
	affectedTemp, err := writePageCheckTemp(w.dir, ".check_affected_objects-*.tsv", func(out io.Writer) error {
		return writePageCheckAffectedObjects(out, result)
	})
	if err != nil {
		w.abort()
		return pageCheckReportPaths{}, err
	}
	summaryTemp, err := writePageCheckTemp(w.dir, ".check_summary-*.md", func(out io.Writer) error {
		return writePageCheckSummary(out, result, context)
	})
	if err != nil {
		_ = os.Remove(affectedTemp)
		w.abort()
		return pageCheckReportPaths{}, err
	}

	temps := []struct{ temp, final string }{
		{w.badTempPath, paths.BadPages},
		{affectedTemp, paths.AffectedObjects},
		{summaryTemp, paths.Summary},
	}
	for i, item := range temps {
		if err := replacePageCheckReport(item.temp, item.final); err != nil {
			for _, remaining := range temps[i:] {
				_ = os.Remove(remaining.temp)
			}
			return pageCheckReportPaths{}, err
		}
	}
	w.badTempPath = ""
	return paths, nil
}

func (w *pageCheckReportWriter) closeBadPages() error {
	if w.badFinalized {
		return nil
	}
	w.badFinalized = true
	w.badTSV.Flush()
	if err := w.badTSV.Error(); err != nil {
		_ = w.badFile.Close()
		return err
	}
	if err := w.badFile.Sync(); err != nil {
		_ = w.badFile.Close()
		return err
	}
	return w.badFile.Close()
}

func (w *pageCheckReportWriter) abort() {
	if w == nil {
		return
	}
	if !w.badFinalized && w.badFile != nil {
		w.badTSV.Flush()
		_ = w.badFile.Close()
		w.badFinalized = true
	}
	if w.badTempPath != "" {
		_ = os.Remove(w.badTempPath)
	}
}

func writePageCheckTemp(dir, pattern string, write func(io.Writer) error) (string, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := file.Name()
	buffered := bufio.NewWriterSize(file, 64*1024)
	if err := write(buffered); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := buffered.Flush(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func replacePageCheckReport(tempPath, finalPath string) error {
	backupPath := finalPath + ".bak"
	_ = os.Remove(backupPath)
	hadFinal := false
	if _, err := os.Stat(finalPath); err == nil {
		if err := os.Rename(finalPath, backupPath); err != nil {
			return err
		}
		hadFinal = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		if hadFinal {
			_ = os.Rename(backupPath, finalPath)
		}
		return err
	}
	if hadFinal {
		_ = os.Remove(backupPath)
	}
	return nil
}

func writePageCheckAffectedObjects(out io.Writer, result *dm.PageCheckResult) error {
	w := csv.NewWriter(out)
	w.Comma = '\t'
	if err := w.Write([]string{
		"owner", "table_name", "table_id", "object_type", "storage_id", "group_id", "tablespace",
		"header_file", "header_block", "attribution", "confidence", "bad_pages",
		"segment_header_bad_pages", "header_invalid", "checksum_fail", "structure_invalid", "bad_bytes",
		"segment_bytes", "bad_pages_segment_pct",
	}); err != nil {
		return err
	}
	for _, object := range result.AffectedObjects {
		badBytes := uint64(object.BadPages) * uint64(result.PageSize)
		if err := w.Write([]string{
			object.Owner,
			object.Table,
			strconv.FormatUint(uint64(object.TableID), 10),
			string(object.ObjectType),
			strconv.FormatUint(uint64(object.StorageID), 10),
			strconv.FormatUint(uint64(object.GroupID), 10),
			object.Tablespace,
			strconv.FormatInt(int64(object.HeaderFile), 10),
			strconv.FormatUint(uint64(object.HeaderBlock), 10),
			object.Attribution,
			string(object.AttributionConfidence),
			strconv.Itoa(object.BadPages),
			strconv.Itoa(object.SegmentHeaderBadPages),
			strconv.Itoa(object.HeaderInvalid),
			strconv.Itoa(object.ChecksumFail),
			strconv.Itoa(object.StructureInvalid),
			strconv.FormatUint(badBytes, 10),
			strconv.FormatUint(object.SegmentBytes, 10),
			pageCheckPercent(badBytes, object.SegmentBytes),
		}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func writePageCheckSummary(out io.Writer, result *dm.PageCheckResult, context pageCheckReportContext) error {
	generatedAt := context.GeneratedAt
	if generatedAt.IsZero() {
		generatedAt = time.Now()
	}
	scannedBytes := uint64(result.PagesChecked) * uint64(result.PageSize)
	badBytes := uint64(result.BadPagesTotal) * uint64(result.PageSize)
	nonEmpty := result.PagesChecked - result.PagesEmpty
	if nonEmpty < 0 {
		nonEmpty = 0
	}

	fmt.Fprintln(out, "# DMDUL 离线坏页检查报告")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "- 生成时间：%s\n", generatedAt.Format("2006-01-02 15:04:05 -07:00"))
	fmt.Fprintf(out, "- SYSTEM 文件：`%s`\n", markdownInline(context.SystemPath))
	fmt.Fprintf(out, "- 数据目录：`%s`\n", markdownInline(context.DataDir))
	if result.DictionaryUsed {
		fmt.Fprintf(out, "- 字典状态：LOADED（`%s`）\n", markdownInline(defaultIfBlank(result.DictionarySource, "current session")))
	} else {
		fmt.Fprintln(out, "- 字典状态：UNAVAILABLE（physical-only）")
	}
	if len(context.FileFilter) > 0 {
		fmt.Fprintf(out, "- 文件过滤：`%s`\n", markdownInline(strings.Join(context.FileFilter, ",")))
	} else {
		fmt.Fprintln(out, "- 文件过滤：全部已识别 DBF")
	}
	fmt.Fprintf(out, "- dm.ctl 绝对路径：%s\n", map[bool]string{true: "允许跟随（control 模式）", false: "不跟随（离线目录优先）"}[context.FollowControlPaths])
	fmt.Fprintln(out)

	fmt.Fprintln(out, "## 扫描结论")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "| 指标 | 数值 |")
	fmt.Fprintln(out, "|---|---:|")
	fmt.Fprintf(out, "| 数据文件 | %d |\n", result.FilesChecked)
	fmt.Fprintf(out, "| 扫描页 | %d |\n", result.PagesChecked)
	fmt.Fprintf(out, "| 空页 | %d |\n", result.PagesEmpty)
	fmt.Fprintf(out, "| 不适用行页校验的文件/引导元数据页 | %d |\n", result.ChecksumNotApplicable)
	fmt.Fprintf(out, "| 非空页 | %d |\n", nonEmpty)
	fmt.Fprintf(out, "| 坏页 | %d |\n", result.BadPagesTotal)
	fmt.Fprintf(out, "| 坏页字节 | %s (%d bytes) |\n", pageCheckHumanBytes(badBytes), badBytes)
	fmt.Fprintf(out, "| 扫描字节 | %s (%d bytes) |\n", pageCheckHumanBytes(scannedBytes), scannedBytes)
	fmt.Fprintf(out, "| 坏页占扫描页比例 | %s |\n", pageCheckPercent(uint64(result.BadPagesTotal), uint64(result.PagesChecked)))
	fmt.Fprintf(out, "| 页头无效 | %d |\n", result.Corruption[dm.PageCorruptionHeader])
	fmt.Fprintf(out, "| 校验失败 | %d |\n", result.Corruption[dm.PageCorruptionChecksum])
	fmt.Fprintf(out, "| 结构无效 | %d |\n", result.Corruption[dm.PageCorruptionStructure])
	fmt.Fprintf(out, "| B-tree 叶链问题 | %d |\n", len(result.ChainIssues))
	fmt.Fprintf(out, "| 字典一致性问题 | %d |\n", len(result.DictIssues))
	fmt.Fprintln(out)

	typeCounts := make(map[dm.PageObjectType]int)
	typePages := make(map[dm.PageObjectType]int)
	segmentHeaderBadPages := 0
	for _, object := range result.AffectedObjects {
		typeCounts[object.ObjectType]++
		typePages[object.ObjectType] += object.BadPages
		segmentHeaderBadPages += object.SegmentHeaderBadPages
	}

	fmt.Fprintln(out, "## 对象影响")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "| 指标 | 数值 |")
	fmt.Fprintln(out, "|---|---:|")
	fmt.Fprintf(out, "| 已归属坏页 | %d (%s) |\n", result.AttributedBadPages, pageCheckPercent(uint64(result.AttributedBadPages), uint64(result.BadPagesTotal)))
	fmt.Fprintf(out, "| 未归属坏页 | %d (%s) |\n", result.UnattributedBadPages, pageCheckPercent(uint64(result.UnattributedBadPages), uint64(result.BadPagesTotal)))
	fmt.Fprintf(out, "| 受影响物理对象 | %d |\n", len(result.AffectedObjects))
	fmt.Fprintf(out, "| 受影响表 | %d / %d (%s) |\n", result.AffectedTables, result.TotalTables, pageCheckPercent(uint64(result.AffectedTables), uint64(result.TotalTables)))
	fmt.Fprintf(out, "| 已知受影响表段大小 | %s / %s (%s) |\n",
		pageCheckHumanBytes(result.AffectedTableBytes), pageCheckHumanBytes(result.TotalTableBytes),
		pageCheckPercent(result.AffectedTableBytes, result.TotalTableBytes))
	fmt.Fprintf(out, "| 命中已知表段头的坏页 | %d |\n", segmentHeaderBadPages)
	fmt.Fprintln(out)
	if len(typeCounts) > 0 {
		types := make([]string, 0, len(typeCounts))
		for objectType := range typeCounts {
			types = append(types, string(objectType))
		}
		sort.Strings(types)
		fmt.Fprintln(out, "### 对象类型")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "| 类型 | 对象数 | 坏页数 |")
		fmt.Fprintln(out, "|---|---:|---:|")
		for _, name := range types {
			objectType := dm.PageObjectType(name)
			fmt.Fprintf(out, "| %s | %d | %d |\n", name, typeCounts[objectType], typePages[objectType])
		}
		fmt.Fprintln(out)
	}

	if len(result.UnattributedReasons) > 0 {
		reasons := make([]string, 0, len(result.UnattributedReasons))
		for reason := range result.UnattributedReasons {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		fmt.Fprintln(out, "### 未归属原因")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "| 原因 | 页数 |")
		fmt.Fprintln(out, "|---|---:|")
		for _, reason := range reasons {
			fmt.Fprintf(out, "| %s | %d |\n", markdownCell(reason), result.UnattributedReasons[reason])
		}
		fmt.Fprintln(out)
	}

	fmt.Fprintln(out, "## 文件结果")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "| 状态 | 文件 | 表空间 | Group | File | 扫描页 | 空页 | 坏页 |")
	fmt.Fprintln(out, "|---|---|---|---:|---:|---:|---:|---:|")
	for _, file := range result.Files {
		status := "OK"
		if file.SizeInvalid {
			status = "FILE_INVALID"
		} else if file.BadPages > 0 {
			status = "BAD"
		}
		fmt.Fprintf(out, "| %s | %s | %s | %d | %d | %d | %d | %d |\n",
			status, markdownCell(file.Path), markdownCell(file.Tablespace), file.GroupID, file.FileID,
			file.PagesChecked, file.PagesEmpty, file.BadPages)
	}
	fmt.Fprintln(out)

	if len(result.AffectedObjects) > 0 {
		limit := len(result.AffectedObjects)
		if limit > 20 {
			limit = 20
		}
		objects := append([]dm.PageAffectedObject(nil), result.AffectedObjects...)
		sort.Slice(objects, func(i, j int) bool {
			if objects[i].BadPages != objects[j].BadPages {
				return objects[i].BadPages > objects[j].BadPages
			}
			if objects[i].Owner != objects[j].Owner {
				return objects[i].Owner < objects[j].Owner
			}
			return objects[i].Table < objects[j].Table
		})
		fmt.Fprintln(out, "## 受影响对象（坏页数前 20）")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "| Owner | 表 | 类型 | Storage ID | 归属依据 | 置信度 | 坏页 |")
		fmt.Fprintln(out, "|---|---|---|---:|---|---|---:|")
		for _, object := range objects[:limit] {
			fmt.Fprintf(out, "| %s | %s | %s | %d | %s | %s | %d |\n",
				markdownCell(object.Owner), markdownCell(object.Table), object.ObjectType, object.StorageID,
				markdownCell(object.Attribution), object.AttributionConfidence, object.BadPages)
		}
		fmt.Fprintln(out)
	}

	fmt.Fprintln(out, "## 证据边界")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "- `TABLE`：由主 storage_id 或唯一表段范围归属。storage_id 为 HIGH，segment_range 为 MEDIUM。")
	fmt.Fprintln(out, "- `TABLE_ASSIST`：字典能够证明该 storage_id 隶属于父表，但当前持久化字典不足以继续区分 INDEX、LOB、分区或其他辅助存储，因此不会猜测具体类型。")
	fmt.Fprintln(out, "- 未归属页不等同于空闲页。它只表示当前字典证据不足，可能是未知内部对象、缺失字典、重叠段范围或无法映射的 storage_id。")
	fmt.Fprintln(out, "- `check pages` 是离线只读物理诊断，不替代官方 `dmdbchk`；建议在条件允许时交叉验证。")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "## 明细文件")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "- 全量坏页：`%s`\n", pageCheckBadPagesName)
	fmt.Fprintf(out, "- 受影响对象：`%s`\n", pageCheckAffectedObjectsName)
	return nil
}

func pageCheckPercent(numerator, denominator uint64) string {
	if denominator == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.6f%%", float64(numerator)*100/float64(denominator))
}

func pageCheckHumanBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := uint64(unit), 0
	for n := value / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(value)/float64(div), "KMGTPE"[exp])
}

func markdownCell(value string) string {
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func markdownInline(value string) string {
	return strings.ReplaceAll(value, "`", "'")
}
