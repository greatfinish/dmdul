package dm

import (
	"bytes"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
	"io"
	"strings"
	"unicode/utf8"
)

func (d textDecoder) decode(raw []byte) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	var candidates []string
	if d.preferred != "" && d.preferred != "auto" {
		candidates = append(candidates, d.preferred)
	}
	candidates = append(candidates, "utf-8", "gb18030", "gbk", "euc-kr")
	seen := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		value, ok := decodeWithCharset(raw, name)
		if ok && !containsBadControl(value) {
			return value, true
		}
	}
	return "", false
}

func decodeWithCharset(raw []byte, charset string) (string, bool) {
	switch strings.ReplaceAll(strings.ToLower(charset), "_", "-") {
	case "utf8", "utf-8":
		if !utf8.Valid(raw) {
			return "", false
		}
		return string(raw), true
	case "gb18030":
		return decodeWithEncoding(raw, simplifiedchinese.GB18030)
	case "gbk":
		return decodeWithEncoding(raw, simplifiedchinese.GBK)
	case "euc-kr", "euckr", "kr":
		return decodeWithEncoding(raw, korean.EUCKR)
	default:
		return "", false
	}
}

func decodeWithEncoding(raw []byte, enc encoding.Encoding) (string, bool) {
	reader := transform.NewReader(bytes.NewReader(raw), enc.NewDecoder())
	out, err := io.ReadAll(reader)
	if err != nil {
		return "", false
	}
	return string(out), true
}

func containsBadControl(value string) bool {
	for _, ch := range value {
		if ch == utf8.RuneError || (ch < 32 && ch != '\t' && ch != '\n' && ch != '\r') {
			return true
		}
	}
	return false
}

func isSafeShortText(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	return !containsBadControl(value)
}
