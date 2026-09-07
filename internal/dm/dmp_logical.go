package dm

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DMPLogicalOptions describes one native logical dump. Table data is supplied
// as the proven table-level DMP streams produced by ExportData; the assembler
// copies their data phases without materializing rows or LOB values in memory.
type DMPLogicalOptions struct {
	OutputPath     string
	Catalog        *DMPMetadataCatalog
	TableDataPaths map[uint32]string
	LogicalVersion uint32
	RowFormatFlag  uint32
	Charset        string
	CaseSensitive  *bool
	ExtentSize     uint32
	PageSize       uint32
	PageCheck      *uint32
}

type dmpLogicalTableIndex struct {
	metadataOffset uint64
	objectID       uint32
	name           []byte
	phaseOffsets   []uint64
}

type dmpLogicalSchemaIndex struct {
	recordOffset uint64
	name         []byte
	owner        []byte
	tables       []dmpLogicalTableIndex
}

// WriteLogicalDMP writes one self-describing FULL, OWNER, SCHEMAS, or TABLES
// file. Empty tables remain in the metadata directory even when no data spool
// exists.
func WriteLogicalDMP(opts DMPLogicalOptions) (*DMPInfo, error) {
	if strings.TrimSpace(opts.OutputPath) == "" {
		return nil, fmt.Errorf("logical dmp output path is required")
	}
	if opts.Catalog == nil {
		return nil, fmt.Errorf("logical dmp metadata catalog is required")
	}
	if opts.Catalog.Mode < DMPModeFull || opts.Catalog.Mode > DMPModeOwner {
		return nil, fmt.Errorf("unsupported logical dmp mode %s", opts.Catalog.Mode)
	}
	if len(opts.Catalog.Schemas) == 0 {
		return nil, fmt.Errorf("logical dmp requires at least one schema")
	}
	if opts.LogicalVersion == 0 {
		opts.LogicalVersion = dmpCurrentLogicalVersion
	}
	if opts.LogicalVersion < 9 || opts.LogicalVersion > dmpCurrentLogicalVersion {
		return nil, fmt.Errorf("unsupported dmp logical version %d", opts.LogicalVersion)
	}
	if opts.RowFormatFlag == 0 {
		opts.RowFormatFlag = 1
	}
	if opts.RowFormatFlag != 1 {
		return nil, fmt.Errorf("unsupported dmp row-format flag %d", opts.RowFormatFlag)
	}
	if opts.ExtentSize == 0 {
		opts.ExtentSize = 16
	}
	if opts.PageSize == 0 {
		opts.PageSize = 8192
	}
	charset, err := dmpCharsetFromName(opts.Charset)
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(opts.OutputPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create logical dmp output directory: %w", err)
		}
	}

	file, err := os.Create(opts.OutputPath)
	if err != nil {
		return nil, fmt.Errorf("create logical dmp: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
			_ = os.Remove(opts.OutputPath)
		}
	}()
	if _, err := file.Write(make([]byte, DMPHeaderSize)); err != nil {
		return nil, fmt.Errorf("write logical dmp header placeholder: %w", err)
	}

	objectCount := uint32(0)
	for _, record := range opts.Catalog.GlobalRecords {
		if err := writeDMPMetadataRecord(file, charset, record); err != nil {
			return nil, err
		}
		objectCount++
	}

	schemaIndexes := make([]dmpLogicalSchemaIndex, 0, len(opts.Catalog.Schemas))
	for _, schema := range opts.Catalog.Schemas {
		schemaName, err := encodeDMPText(schema.Name, charset)
		if err != nil {
			return nil, fmt.Errorf("encode dmp schema %q: %w", schema.Name, err)
		}
		ownerName, err := encodeDMPText(defaultIfEmpty(schema.Owner, schema.Name), charset)
		if err != nil {
			return nil, fmt.Errorf("encode dmp schema owner %q: %w", schema.Owner, err)
		}
		recordOffset, err := dmpCurrentOffset(file)
		if err != nil {
			return nil, err
		}
		if err := writeDMPSchemaRecord(file, schemaName); err != nil {
			return nil, err
		}
		index := dmpLogicalSchemaIndex{recordOffset: recordOffset, name: schemaName, owner: ownerName}
		for _, record := range schema.Records {
			if err := writeDMPMetadataRecord(file, charset, record); err != nil {
				return nil, err
			}
			objectCount++
		}
		for _, table := range schema.Tables {
			tableIndex, count, err := writeDMPLogicalTable(file, charset, table, opts.TableDataPaths[table.ID])
			if err != nil {
				return nil, err
			}
			objectCount += count
			index.tables = append(index.tables, tableIndex)
		}
		schemaIndexes = append(schemaIndexes, index)
	}

	payloadEnd, err := dmpCurrentOffset(file)
	if err != nil {
		return nil, err
	}
	if err := writeDMPLogicalFooter(file, schemaIndexes); err != nil {
		return nil, fmt.Errorf("write logical dmp footer: %w", err)
	}
	payloadHash, err := hashDMPPayload(file)
	if err != nil {
		return nil, fmt.Errorf("hash logical dmp: %w", err)
	}
	header, err := buildDMPLogicalHeader(opts, charset, payloadEnd, objectCount, payloadHash.Sum(nil))
	if err != nil {
		return nil, err
	}
	if _, err := file.WriteAt(header, 0); err != nil {
		return nil, fmt.Errorf("write logical dmp header: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync logical dmp: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close logical dmp: %w", err)
	}
	ok = true
	return InspectDMP(opts.OutputPath)
}

func writeDMPLogicalTable(file *os.File, charset dmpCharsetHeader, table DMPTableMetadata, dataPath string) (dmpLogicalTableIndex, uint32, error) {
	tableName, err := encodeDMPText(table.Name, charset)
	if err != nil {
		return dmpLogicalTableIndex{}, 0, fmt.Errorf("encode dmp table %s.%s: %w", table.Schema, table.Name, err)
	}
	phase1Offset, err := dmpCurrentOffset(file)
	if err != nil {
		return dmpLogicalTableIndex{}, 0, err
	}
	dataSource, err := openDMPDataSpool(dataPath, table)
	if err != nil {
		return dmpLogicalTableIndex{}, 0, err
	}
	if dataSource != nil {
		defer dataSource.file.Close()
	}
	noRows := byte(1)
	if dataSource != nil && len(dataSource.phaseOffsets) > 0 {
		noRows = 0
	}
	if err := writeDMPTablePhaseHeader(file, table.ID, 1, noRows, tableName); err != nil {
		return dmpLogicalTableIndex{}, 0, err
	}
	count := uint32(0)
	for _, record := range table.Records {
		if err := writeDMPMetadataRecord(file, charset, record); err != nil {
			return dmpLogicalTableIndex{}, 0, err
		}
		count++
	}
	if dataSource != nil && len(dataSource.phaseOffsets) > 0 {
		if err := writeDMPUint16(file, dmpRecordRowMarker); err != nil {
			return dmpLogicalTableIndex{}, 0, err
		}
		if err := writeDMPUint16(file, 0xFFFF); err != nil {
			return dmpLogicalTableIndex{}, 0, err
		}
		if err := writeDMPUint16(file, table.ColumnCount); err != nil {
			return dmpLogicalTableIndex{}, 0, err
		}
	}
	phase1End, err := dmpCurrentOffset(file)
	if err != nil {
		return dmpLogicalTableIndex{}, 0, err
	}
	phase1Length := phase1End - phase1Offset
	if phase1Length > uint64(^uint32(0)) {
		return dmpLogicalTableIndex{}, 0, fmt.Errorf("dmp metadata phase for %s.%s exceeds uint32", table.Schema, table.Name)
	}
	if err := patchDMPUint32(file, int64(phase1Offset+12), uint32(phase1Length)); err != nil {
		return dmpLogicalTableIndex{}, 0, err
	}

	index := dmpLogicalTableIndex{
		metadataOffset: phase1Offset, objectID: table.ID, name: tableName,
		phaseOffsets: []uint64{phase1Offset},
	}
	if dataSource != nil {
		for _, sourceOffset := range dataSource.phaseOffsets {
			targetOffset, err := dmpCurrentOffset(file)
			if err != nil {
				return dmpLogicalTableIndex{}, 0, err
			}
			length, err := dmpDataPhaseLength(dataSource.file, sourceOffset, dataSource.payloadEnd)
			if err != nil {
				return dmpLogicalTableIndex{}, 0, fmt.Errorf("read dmp data phase for %s.%s: %w", table.Schema, table.Name, err)
			}
			if _, err := io.CopyN(file, io.NewSectionReader(dataSource.file, int64(sourceOffset), int64(length)), int64(length)); err != nil {
				return dmpLogicalTableIndex{}, 0, fmt.Errorf("copy dmp data phase for %s.%s: %w", table.Schema, table.Name, err)
			}
			index.phaseOffsets = append(index.phaseOffsets, targetOffset)
		}
	}
	return index, count, nil
}

type dmpDataSpool struct {
	file         *os.File
	payloadEnd   uint64
	phaseOffsets []uint64
}

func openDMPDataSpool(path string, table DMPTableMetadata) (*dmpDataSpool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	info, err := InspectDMP(path)
	if err != nil {
		return nil, fmt.Errorf("inspect dmp data spool %s: %w", path, err)
	}
	if !info.PayloadMD5Valid || !info.HeaderChecksumValid || !info.FooterMagicValid || info.FooterParseError != "" {
		return nil, fmt.Errorf("invalid dmp data spool %s", path)
	}
	if info.Mode != DMPModeTables || len(info.Tables) != 1 {
		return nil, fmt.Errorf("dmp data spool %s is not a single TABLES export", path)
	}
	index := info.Tables[0]
	if index.ObjectID != table.ID || !strings.EqualFold(index.Name, table.Name) {
		return nil, fmt.Errorf("dmp data spool %s identifies table %d/%s, want %d/%s", path, index.ObjectID, index.Name, table.ID, table.Name)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open dmp data spool %s: %w", path, err)
	}
	phases := append([]uint64(nil), index.PhaseOffsets...)
	if len(phases) > 0 {
		phases = phases[1:]
	}
	return &dmpDataSpool{file: file, payloadEnd: info.PayloadEnd, phaseOffsets: phases}, nil
}

func dmpDataPhaseLength(file *os.File, offset uint64, payloadEnd uint64) (uint32, error) {
	if offset+20 > payloadEnd {
		return 0, fmt.Errorf("phase offset 0x%X is outside payload", offset)
	}
	var header [20]byte
	if _, err := file.ReadAt(header[:], int64(offset)); err != nil {
		return 0, err
	}
	if binary.LittleEndian.Uint16(header[0:2]) != 2 || binary.LittleEndian.Uint16(header[2:4]) != 0xFFFF {
		return 0, fmt.Errorf("unexpected phase marker at 0x%X", offset)
	}
	length := binary.LittleEndian.Uint32(header[12:16])
	if length < 20 || offset+uint64(length) > payloadEnd {
		return 0, fmt.Errorf("invalid phase length %d at 0x%X", length, offset)
	}
	return length, nil
}

func writeDMPTablePhaseHeader(file *os.File, tableID uint32, phase uint32, noRows byte, tableName []byte) error {
	if err := writeDMPUint16(file, 2); err != nil {
		return err
	}
	if err := writeDMPUint16(file, 0xFFFF); err != nil {
		return err
	}
	for _, value := range []uint32{tableID, phase, 0, 0} {
		if err := writeDMPUint32(file, value); err != nil {
			return err
		}
	}
	if _, err := file.Write([]byte{noRows}); err != nil {
		return err
	}
	return writeDMPString32(file, tableName)
}

func writeDMPSchemaRecord(file *os.File, schemaName []byte) error {
	if err := writeDMPUint16(file, dmpRecordSchema); err != nil {
		return err
	}
	if err := writeDMPUint16(file, 0xFFFF); err != nil {
		return err
	}
	return writeDMPString32(file, schemaName)
}

func writeDMPMetadataRecord(file *os.File, charset dmpCharsetHeader, record DMPMetadataRecord) error {
	if record.RecordType == dmpRecordSchema || record.RecordType == dmpRecordRowMarker {
		return fmt.Errorf("record type %d requires a dedicated encoder", record.RecordType)
	}
	if err := writeDMPUint16(file, record.RecordType); err != nil {
		return err
	}
	if err := writeDMPUint16(file, 0xFFFF); err != nil {
		return err
	}
	if record.RecordType == dmpRecordColumnGrant {
		if record.Grant == nil || record.Grant.ColumnName == "" {
			return fmt.Errorf("column grant metadata has no resolved column name")
		}
		return writeDMPObjectGrant(file, charset, record.Grant, false)
	}
	if record.RecordType == dmpRecordObjectGrant || record.RecordType == dmpRecordSchemaGrant {
		return writeDMPObjectGrant(file, charset, record.Grant, record.RecordType == dmpRecordSchemaGrant)
	}
	name, err := encodeDMPText(record.Name, charset)
	if err != nil {
		return fmt.Errorf("encode dmp metadata name %q: %w", record.Name, err)
	}
	sql, err := encodeDMPText(record.SQL, charset)
	if err != nil {
		return fmt.Errorf("encode dmp metadata SQL for %q: %w", record.Name, err)
	}
	if err := writeDMPString32(file, name); err != nil {
		return err
	}
	if err := writeDMPString32(file, sql); err != nil {
		return err
	}
	switch record.RecordType {
	case dmpRecordSystemPrivilege, dmpRecordRoleGrant, dmpRecordBuiltinGrant:
		return nil
	case dmpRecordTrigger:
		tableName, err := encodeDMPText(record.TableName, charset)
		if err != nil {
			return err
		}
		if err := writeDMPString32(file, tableName); err != nil {
			return err
		}
		return writeDMPUint32(file, record.ExtraValue)
	default:
		return writeDMPUint32(file, record.ExtraValue)
	}
}

func writeDMPObjectGrant(file *os.File, charset dmpCharsetHeader, grant *DMPObjectGrant, includeObjectType bool) error {
	if grant == nil {
		return fmt.Errorf("dmp object grant metadata is missing")
	}
	for _, value := range []string{grant.Grantor, grant.Grantee, grant.Privilege, grant.Owner, grant.ObjectName} {
		encoded, err := encodeDMPText(value, charset)
		if err != nil {
			return err
		}
		if err := writeDMPString32(file, encoded); err != nil {
			return err
		}
	}
	if grant.ColumnName != "" {
		encoded, err := encodeDMPText(grant.ColumnName, charset)
		if err != nil {
			return err
		}
		if err := writeDMPString32(file, encoded); err != nil {
			return err
		}
	}
	if err := writeDMPUint32(file, 1); err != nil {
		return err
	}
	grantable := byte('N')
	if strings.EqualFold(grant.Grantable, "Y") || strings.EqualFold(grant.Grantable, "YES") {
		grantable = 'Y'
	}
	if _, err := file.Write([]byte{grantable}); err != nil {
		return err
	}
	if !includeObjectType {
		return nil
	}
	objectType, err := encodeDMPText(grant.ObjectType, charset)
	if err != nil {
		return err
	}
	return writeDMPString32(file, objectType)
}

func writeDMPLogicalFooter(file *os.File, schemas []dmpLogicalSchemaIndex) error {
	if _, err := file.Write(dmpFooterMagic[:]); err != nil {
		return err
	}
	for schemaIndex, schema := range schemas {
		if schemaIndex > 0 {
			if err := writeDMPUint32(file, dmpSchemaIndexMarker); err != nil {
				return err
			}
		}
		if err := writeDMPUint16(file, 0); err != nil {
			return err
		}
		if err := writeDMPUint64(file, schema.recordOffset); err != nil {
			return err
		}
		if err := writeDMPString16(file, schema.name); err != nil {
			return err
		}
		if err := writeDMPString16(file, schema.owner); err != nil {
			return err
		}
		for _, table := range schema.tables {
			if err := writeDMPUint32(file, dmpTableIndexMarker); err != nil {
				return err
			}
			if err := writeDMPUint16(file, 0); err != nil {
				return err
			}
			if err := writeDMPUint64(file, table.metadataOffset); err != nil {
				return err
			}
			if err := writeDMPUint32(file, table.objectID); err != nil {
				return err
			}
			if err := writeDMPString16(file, table.name); err != nil {
				return err
			}
			if err := writeDMPUint32(file, uint32(len(table.phaseOffsets))); err != nil {
				return err
			}
			for _, offset := range table.phaseOffsets {
				if err := writeDMPUint16(file, 0); err != nil {
					return err
				}
				if err := writeDMPUint64(file, offset); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func buildDMPLogicalHeader(opts DMPLogicalOptions, charset dmpCharsetHeader, payloadEnd uint64, objectCount uint32, payloadMD5 []byte) ([]byte, error) {
	header := make([]byte, DMPHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], opts.LogicalVersion+8)
	binary.LittleEndian.PutUint32(header[4:8], 1)
	binary.LittleEndian.PutUint32(header[8:12], uint32(opts.Catalog.Mode))
	binary.LittleEndian.PutUint64(header[dmpPayloadEndOffset:dmpPayloadEndOffset+8], payloadEnd)
	header[dmpEncodingCodeOffset] = charset.EncodingCode
	description, err := encodeDMPText("dmdul native logical export", charset)
	if err != nil {
		return nil, err
	}
	if len(description) > dmpCompressionOffset-dmpDescriptionOffset {
		description = description[:dmpCompressionOffset-dmpDescriptionOffset]
	}
	binary.LittleEndian.PutUint16(header[dmpDescriptionLenOffset:dmpDescriptionLenOffset+2], uint16(len(description)))
	copy(header[dmpDescriptionOffset:], description)
	binary.LittleEndian.PutUint32(header[dmpRowFormatFlagOffset:dmpRowFormatFlagOffset+4], opts.RowFormatFlag)
	caseSensitive := true
	if opts.CaseSensitive != nil {
		caseSensitive = *opts.CaseSensitive
	}
	if caseSensitive {
		header[dmpCaseSensitiveOffset] = 1
	}
	binary.LittleEndian.PutUint32(header[dmpExtentSizeOffset:dmpExtentSizeOffset+4], opts.ExtentSize)
	binary.LittleEndian.PutUint32(header[dmpPageSizeOffset:dmpPageSizeOffset+4], opts.PageSize)
	binary.LittleEndian.PutUint16(header[dmpCharsetFlagOffset:dmpCharsetFlagOffset+2], charset.Flag)
	pageCheck := uint32(3)
	if opts.PageCheck != nil {
		pageCheck = *opts.PageCheck
	}
	binary.LittleEndian.PutUint32(header[dmpPageCheckOffset:dmpPageCheckOffset+4], pageCheck)
	binary.LittleEndian.PutUint32(header[dmpObjectCountOffset:dmpObjectCountOffset+4], objectCount)
	copy(header[dmpPayloadMD5Offset:dmpPayloadMD5Offset+md5.Size], payloadMD5)
	copy(header[dmpBuildStringOffset:], []byte("dmdul"))
	header[dmpHeaderChecksumOffset] = dmpXOR(header)
	return header, nil
}

func dmpCurrentOffset(file *os.File) (uint64, error) {
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	return uint64(offset), nil
}
