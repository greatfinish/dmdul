package dm

import (
	"strings"
)

type tableNameMatcher struct {
	all       bool
	hasRules  bool
	names     map[string]bool
	qualified map[string]bool
}

func newTableNameMatcher(filter string) tableNameMatcher {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return tableNameMatcher{}
	}
	if strings.EqualFold(filter, "all") || strings.EqualFold(filter, "*") {
		return tableNameMatcher{all: true, hasRules: true}
	}
	matcher := tableNameMatcher{
		hasRules:  true,
		names:     make(map[string]bool),
		qualified: make(map[string]bool),
	}
	for _, part := range strings.Split(filter, ",") {
		token := normalizeTableFilterToken(part)
		if token == "" {
			continue
		}
		if strings.Contains(token, ".") {
			matcher.qualified[token] = true
			continue
		}
		matcher.names[token] = true
	}
	if len(matcher.names) == 0 && len(matcher.qualified) == 0 {
		return tableNameMatcher{}
	}
	return matcher
}

func (m tableNameMatcher) allowed(owner string, table string) bool {
	if !m.hasRules {
		return false
	}
	if m.all {
		return true
	}
	owner = normalizeNameFilter(owner)
	table = normalizeNameFilter(table)
	return m.names[table] || m.qualified[owner+"."+table]
}

func normalizeNameFilter(value string) string {
	parts := splitQualifiedNameFilter(value)
	if len(parts) > 1 {
		return normalizeTableFilterToken(value)
	}
	return normalizeNameFilterPart(value)
}

func normalizeTableFilterToken(value string) string {
	parts := splitQualifiedNameFilter(value)
	if len(parts) == 0 {
		return ""
	}
	for i := range parts {
		parts[i] = normalizeNameFilterPart(parts[i])
	}
	return strings.Join(parts, ".")
}

func normalizeNameFilterPart(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	} else {
		value = strings.Trim(value, `"`)
	}
	value = strings.ReplaceAll(value, `""`, `"`)
	return strings.ToUpper(value)
}

func splitQualifiedNameFilter(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var parts []string
	var current strings.Builder
	inQuote := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch == '"' {
			current.WriteByte(ch)
			if inQuote && i+1 < len(value) && value[i+1] == '"' {
				i++
				current.WriteByte(value[i])
				continue
			}
			inQuote = !inQuote
			continue
		}
		if ch == '.' && !inQuote {
			parts = append(parts, current.String())
			current.Reset()
			continue
		}
		current.WriteByte(ch)
	}
	parts = append(parts, current.String())
	out := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
