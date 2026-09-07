# Parser Fixtures

`dm9_sm3_system_page304.bin` is an 8 KiB SYS dictionary page from a disposable
DM9 instance initialized on 2026-09-06 with `PAGE_CHECK=2 PAGE_HASH_NAME=SM3`.
Build: `03151060506-20260417-322930-20218`. It contains built-in catalog rows,
not a production backup. The row at `0xFE6` crosses a sector hash boundary.
The fixture verifies raw per-sector checksums and restoration from the backup
area at `0x1FB8` before interpreting row headers and slots.

`dm8_hfs_nullable_int.bin` contains only the 4 KiB file header and first 8 KiB
section of a synthetic nullable INT column from the disposable DM8 UNDOLAB.
Build: `03134284336-20250117-257733-20132`. Values are 1..1024, every third
value NULL. There are no business records. Its final 128-byte MSB-first
presence bitmap has exactly 341 zero bits.

SHA256:

```text
C1933B003554E651FE06B7B18B94A958E234D33B499270F2D32EFBA53516413A  dm9_sm3_system_page304.bin
7B374F658319714BA412C1CB313064744DD971A053CC57D699A9345461F9A2C2  dm8_hfs_nullable_int.bin
```
