package agent

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

type byteSpan struct {
	start int
	end   int
	repl  []byte
}

func formatTOMLKey(s string) string {
	if isBareTOMLKey(s) {
		return s
	}
	return strconv.Quote(s)
}

func isBareTOMLKey(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func detectEOL(data []byte) string {
	if bytes.Contains(data, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

func formatTOMLString(val string, origSpan []byte) string {
	if len(origSpan) >= 2 && origSpan[0] == '\'' && origSpan[len(origSpan)-1] == '\'' && !bytes.HasPrefix(origSpan, []byte("'''")) {
		if !strings.Contains(val, "'") && !strings.Contains(val, "\n") && !strings.Contains(val, "\r") {
			return "'" + val + "'"
		}
	}
	return strconv.Quote(val)
}

func orderKeys(keys map[string]string) []string {
	preferred := []string{"provider", "model", "effort"}
	var ordered []string
	seen := make(map[string]bool)
	for _, p := range preferred {
		if _, ok := keys[p]; ok {
			ordered = append(ordered, p)
			seen[p] = true
		}
	}
	var others []string
	for k := range keys {
		if !seen[k] {
			others = append(others, k)
		}
	}
	sort.Strings(others)
	return append(ordered, others...)
}

func findMatchingBrace(data []byte, start int) int {
	depth := 0
	i := start
	for i < len(data) {
		switch data[i] {
		case '"':
			if i+2 < len(data) && data[i+1] == '"' && data[i+2] == '"' {
				i += 3
				for i < len(data) {
					if data[i] == '\\' {
						i += 2
						continue
					}
					if i+2 < len(data) && data[i] == '"' && data[i+1] == '"' && data[i+2] == '"' {
						i += 3
						break
					}
					i++
				}
			} else {
				i++
				for i < len(data) {
					if data[i] == '\\' {
						i += 2
						continue
					}
					if data[i] == '"' {
						i++
						break
					}
					i++
				}
			}
		case '\'':
			if i+2 < len(data) && data[i+1] == '\'' && data[i+2] == '\'' {
				i += 3
				for i < len(data) {
					if i+2 < len(data) && data[i] == '\'' && data[i+1] == '\'' && data[i+2] == '\'' {
						i += 3
						break
					}
					i++
				}
			} else {
				i++
				for i < len(data) {
					if data[i] == '\'' {
						i++
						break
					}
					i++
				}
			}
		case '#':
			i++
			for i < len(data) && data[i] != '\n' {
				i++
			}
		case '{':
			depth++
			i++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
			i++
		default:
			i++
		}
	}
	return -1
}

func patchRootKeys(path string, keys map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	eol := detectEOL(data)

	if len(data) == 0 {
		var buf bytes.Buffer
		for _, k := range orderKeys(keys) {
			buf.WriteString(fmt.Sprintf("%s = %s%s", formatTOMLKey(k), strconv.Quote(keys[k]), eol))
		}
		var check map[string]any
		if err := toml.Unmarshal(buf.Bytes(), &check); err != nil {
			return fmt.Errorf("validating config: %w", err)
		}
		return writePrivate(path, buf.Bytes())
	}

	var p unstable.Parser
	p.Reset(data)

	type keySpan struct {
		start int
		end   int
	}
	existing := make(map[string]keySpan)
	inRoot := true
	lastRootKVLineEnd := -1
	firstTableLineStart := -1

	for p.NextExpression() {
		e := p.Expression()
		if e.Kind == unstable.Table || e.Kind == unstable.ArrayTable {
			if inRoot {
				inRoot = false
				it := e.Key()
				if it.Next() {
					kn := it.Node()
					lineStart := bytes.LastIndexByte(data[:kn.Raw.Offset], '\n')
					if lineStart < 0 {
						firstTableLineStart = 0
					} else {
						firstTableLineStart = lineStart + 1
					}
				}
			}
			continue
		}
		if inRoot && e.Kind == unstable.KeyValue {
			val := e.Value()
			valStart := int(val.Raw.Offset)
			valLen := int(val.Raw.Length)
			if valLen == 0 && len(val.Data) > 0 {
				r := p.Range(val.Data)
				valStart = int(r.Offset)
				valLen = int(r.Length)
			}
			valEnd := valStart + valLen

			idx := bytes.IndexByte(data[valEnd:], '\n')
			if idx >= 0 {
				lastRootKVLineEnd = valEnd + idx + 1
			} else {
				lastRootKVLineEnd = len(data)
			}

			it := e.Key()
			var parts []string
			for it.Next() {
				parts = append(parts, string(it.Node().Data))
			}
			if len(parts) == 1 {
				k := parts[0]
				if _, ok := keys[k]; ok {
					existing[k] = keySpan{start: valStart, end: valEnd}
				}
			}
		}
	}
	if err := p.Error(); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	var spans []byteSpan
	var missing []string
	for _, k := range orderKeys(keys) {
		if ex, ok := existing[k]; ok {
			origSpan := data[ex.start:ex.end]
			spans = append(spans, byteSpan{
				start: ex.start,
				end:   ex.end,
				repl:  []byte(formatTOMLString(keys[k], origSpan)),
			})
		} else {
			missing = append(missing, k)
		}
	}

	if len(missing) > 0 {
		var ins bytes.Buffer
		insertPos := 0
		if lastRootKVLineEnd >= 0 {
			insertPos = lastRootKVLineEnd
			if insertPos == len(data) && len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
				ins.WriteString(eol)
			}
		} else if firstTableLineStart >= 0 {
			insertPos = firstTableLineStart
			if idx := bytes.LastIndex(data[:firstTableLineStart], []byte("\n\n")); idx >= 0 {
				insertPos = idx + 2
			} else if idx := bytes.LastIndex(data[:firstTableLineStart], []byte("\r\n\r\n")); idx >= 0 {
				insertPos = idx + 4
			}
		} else {
			insertPos = len(data)
			if insertPos > 0 && !bytes.HasSuffix(data, []byte("\n")) {
				ins.WriteString(eol)
			}
		}

		for _, k := range missing {
			ins.WriteString(fmt.Sprintf("%s = %s%s", formatTOMLKey(k), strconv.Quote(keys[k]), eol))
		}
		if lastRootKVLineEnd < 0 && firstTableLineStart >= 0 && insertPos == firstTableLineStart {
			ins.WriteString(eol)
		}

		spans = append(spans, byteSpan{
			start: insertPos,
			end:   insertPos,
			repl:  ins.Bytes(),
		})
	}

	sort.Slice(spans, func(i, j int) bool {
		return spans[i].start > spans[j].start
	})

	patched := data
	for _, sp := range spans {
		out := make([]byte, 0, len(patched)-sp.end+sp.start+len(sp.repl))
		out = append(out, patched[:sp.start]...)
		out = append(out, sp.repl...)
		out = append(out, patched[sp.end:]...)
		patched = out
	}

	var check map[string]any
	if err := toml.Unmarshal(patched, &check); err != nil {
		return fmt.Errorf("validating patched config: %w", err)
	}

	return writePrivate(path, patched)
}

func patchProvider(path string, name string, provider Provider) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("provider name is required")
	}

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	eol := detectEOL(data)

	if len(data) == 0 {
		var buf bytes.Buffer
		buf.WriteString(fmt.Sprintf("[providers.%s]%s", formatTOMLKey(name), eol))
		buf.WriteString(fmt.Sprintf("base_url = %s%s", strconv.Quote(provider.BaseURL), eol))
		if provider.APIKey != "" {
			buf.WriteString(fmt.Sprintf("api_key = %s%s", strconv.Quote(provider.APIKey), eol))
		}
		if provider.APIKeyEnv != "" {
			buf.WriteString(fmt.Sprintf("api_key_env = %s%s", strconv.Quote(provider.APIKeyEnv), eol))
		}
		var check map[string]any
		if err := toml.Unmarshal(buf.Bytes(), &check); err != nil {
			return fmt.Errorf("validating config: %w", err)
		}
		return writePrivate(path, buf.Bytes())
	}

	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return err
	}
	if rawProv, ok := m["providers"]; ok {
		if provMap, ok := rawProv.(map[string]any); ok {
			if _, exists := provMap[name]; exists {
				return fmt.Errorf("provider %q already exists", name)
			}
		} else {
			return fmt.Errorf("invalid providers table in config")
		}
	}

	var p unstable.Parser
	p.Reset(data)
	inlineOpen := -1
	inRoot := true
	for p.NextExpression() {
		e := p.Expression()
		if e.Kind == unstable.Table || e.Kind == unstable.ArrayTable {
			inRoot = false
			break
		}
		if inRoot && e.Kind == unstable.KeyValue {
			it := e.Key()
			var parts []string
			for it.Next() {
				parts = append(parts, string(it.Node().Data))
			}
			if len(parts) == 1 && parts[0] == "providers" {
				val := e.Value()
				if val.Kind == unstable.InlineTable {
					inlineOpen = int(val.Raw.Offset)
				}
			}
		}
	}
	if err := p.Error(); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	var patched []byte
	if inlineOpen >= 0 {
		closeOffset := findMatchingBrace(data, inlineOpen)
		if closeOffset < 0 {
			return fmt.Errorf("unmatched brace in inline table")
		}
		inside := strings.TrimSpace(string(data[inlineOpen+1 : closeOffset]))
		insertAt := closeOffset
		for insertAt > inlineOpen+1 && (data[insertAt-1] == ' ' || data[insertAt-1] == '\t' || data[insertAt-1] == '\r' || data[insertAt-1] == '\n') {
			insertAt--
		}

		var b strings.Builder
		if len(inside) > 0 && !strings.HasSuffix(inside, ",") {
			b.WriteString(", ")
		} else if insertAt == closeOffset {
			b.WriteString(" ")
		}
		b.WriteString(formatTOMLKey(name))
		b.WriteString(" = { base_url = ")
		b.WriteString(strconv.Quote(provider.BaseURL))
		if provider.APIKey != "" {
			b.WriteString(", api_key = ")
			b.WriteString(strconv.Quote(provider.APIKey))
		}
		if provider.APIKeyEnv != "" {
			b.WriteString(", api_key_env = ")
			b.WriteString(strconv.Quote(provider.APIKeyEnv))
		}
		b.WriteString(" }")
		if len(inside) == 0 && insertAt == closeOffset {
			b.WriteString(" ")
		}

		out := make([]byte, 0, len(data)+b.Len())
		out = append(out, data[:insertAt]...)
		out = append(out, []byte(b.String())...)
		out = append(out, data[insertAt:]...)
		patched = out
	} else {
		var b bytes.Buffer
		if !bytes.HasSuffix(data, []byte("\n")) {
			b.WriteString(eol)
		}
		if !bytes.HasSuffix(data, []byte("\n\n")) && !bytes.HasSuffix(data, []byte("\r\n\r\n")) {
			b.WriteString(eol)
		}
		b.WriteString(fmt.Sprintf("[providers.%s]%s", formatTOMLKey(name), eol))
		b.WriteString(fmt.Sprintf("base_url = %s%s", strconv.Quote(provider.BaseURL), eol))
		if provider.APIKey != "" {
			b.WriteString(fmt.Sprintf("api_key = %s%s", strconv.Quote(provider.APIKey), eol))
		}
		if provider.APIKeyEnv != "" {
			b.WriteString(fmt.Sprintf("api_key_env = %s%s", strconv.Quote(provider.APIKeyEnv), eol))
		}
		patched = append(data, b.Bytes()...)
	}

	var check map[string]any
	if err := toml.Unmarshal(patched, &check); err != nil {
		return fmt.Errorf("validating patched config: %w", err)
	}

	return writePrivate(path, patched)
}
