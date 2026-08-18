# dmdul Roadmap

## Goal

Build an offline Dameng database extraction CLI that can recover table DDL and
table data from database files when the database instance cannot start normally.

## Suggested Milestones

1. File inspection
   - Read large database files safely.
   - Print file metadata and page samples.
   - Record known page sizes and file header patterns.

2. Page parser
   - Identify page size.
   - Decode common page headers.
   - Classify data pages, dictionary pages, and unsupported pages.

3. Dictionary recovery
   - Locate system catalog objects.
   - Recover table, column, type, and storage metadata.
   - Generate initial `CREATE TABLE` statements.
   - Prefer SYSTEM.DBF page 0 bootstrap-like roots for `SYSOBJECTS` and
     `SYSINDEXES`, then download dictionary tables through the standard
     `storage root -> internal -> leaf chain` path.
   - Keep full-file marker scans as explicit diagnostics/fallback paths only.

4. Row extraction
   - Decode row headers and column values.
   - Support NULL flags, variable-length fields, numeric types, text types, and
     date/time types.
   - Export data as SQL inserts and CSV.
   - Prefer page plans derived from storage roots and leaf chains; use segment
     range scan and full-file scan only as recovery fallback.
   - Replace heuristic NULL inference with explicit 2-bit row metadata decoding.
   - Add verified scalar decoders for `REAL`, `FLOAT`, `DOUBLE`, time with time
     zone, timestamp with time zone, interval, and rowid values.
   - Add out-of-line LOB and `STORAGE(USING LONG ROW)` locator/page-chain
     recovery from the current active row only.

5. Reliability
   - Add corruption-tolerant scanning.
   - Add progress reporting.
   - Add resumable exports.
   - Add explicit raw `storage_scan` recovery mode for cases where SYSTEM.DBF or
     key SYS dictionaries are unavailable.
   - Extend the bounded HUGE TABLE path beyond uncompressed `INT/VARCHAR/CHAR`:
     compression, more fixed-width types, multiple HFS paths, HFS checksums, and
     raw-DMASM HFS files. Unsupported layouts must keep failing before output.
