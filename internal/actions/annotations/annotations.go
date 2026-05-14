// SPDX-License-Identifier: AGPL-3.0-or-later

// Package annotations parses GitHub Actions workflow command annotation
// records from runner logs. Runner log lines are untrusted user input, so this
// package normalizes, caps, and redacts before callers persist anything.
package annotations

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	LevelNotice  = "notice"
	LevelWarning = "warning"
	LevelError   = "error"

	MaxPerChunk   = 50
	MaxLineBytes  = 8192
	MaxTitleBytes = 256
	MaxMsgBytes   = 4096
	MaxPathBytes  = 1024
)

type Annotation struct {
	Level       string
	Title       string
	Message     string
	Path        string
	StartLine   int32
	EndLine     int32
	StartColumn int32
	EndColumn   int32
	LogLine     int32
	LogChunkSeq int32
	Fingerprint string
}

// ParseChunk returns annotations from complete workflow command lines in a log
// chunk. It intentionally ignores a trailing non-newline-terminated line so a
// secret split across chunks cannot be persisted before the next chunk arrives
// and the server-side scrubber has a chance to repair the boundary.
func ParseChunk(chunk []byte, seq int32, maskValues []string) []Annotation {
	if len(chunk) == 0 {
		return nil
	}
	text := strings.ToValidUTF8(string(chunk), "\uFFFD")
	if !strings.HasSuffix(text, "\n") {
		if idx := strings.LastIndexByte(text, '\n'); idx >= 0 {
			text = text[:idx+1]
		} else {
			return nil
		}
	}
	lines := strings.SplitAfter(text, "\n")
	out := make([]Annotation, 0, 1)
	for i, raw := range lines {
		if len(out) >= MaxPerChunk {
			break
		}
		line := strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
		if line == "" || len(line) > MaxLineBytes {
			continue
		}
		ann, ok := parseLine(line, seq, int32(i+1), maskValues)
		if ok {
			out = append(out, ann)
		}
	}
	return out
}

func parseLine(line string, seq, logLine int32, maskValues []string) (Annotation, bool) {
	line = strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(line, "::") {
		return Annotation{}, false
	}
	rest := line[2:]
	idx := strings.Index(rest, "::")
	if idx < 0 {
		return Annotation{}, false
	}
	header := rest[:idx]
	message := rest[idx+2:]
	name, propsRaw, _ := strings.Cut(header, " ")
	level := normalizeLevel(name)
	if level == "" {
		return Annotation{}, false
	}
	props := parseProps(propsRaw)
	title := scrubField(decodeCommandValue(props["title"]), MaxTitleBytes, maskValues)
	message = scrubField(decodeCommandValue(message), MaxMsgBytes, maskValues)
	if message == "" {
		return Annotation{}, false
	}
	filePath := scrubPath(decodeCommandValue(props["file"]), maskValues)
	ann := Annotation{
		Level:       level,
		Title:       title,
		Message:     message,
		Path:        filePath,
		StartLine:   parsePositiveInt32(props["line"]),
		EndLine:     parsePositiveInt32(props["endline"]),
		StartColumn: parsePositiveInt32(firstNonEmpty(props["col"], props["column"])),
		EndColumn:   parsePositiveInt32(firstNonEmpty(props["endcol"], props["endcolumn"])),
		LogLine:     logLine,
		LogChunkSeq: seq,
	}
	if ann.EndLine == 0 && ann.StartLine > 0 {
		ann.EndLine = ann.StartLine
	}
	ann.Fingerprint = fingerprint(ann)
	return ann, true
}

func normalizeLevel(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case LevelNotice:
		return LevelNotice
	case LevelWarning:
		return LevelWarning
	case LevelError:
		return LevelError
	default:
		return ""
	}
}

func parseProps(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	props := make(map[string]string)
	for _, part := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		key = strings.ReplaceAll(key, "-", "")
		key = strings.ReplaceAll(key, "_", "")
		if key == "" {
			continue
		}
		props[key] = strings.TrimSpace(value)
	}
	return props
}

func decodeCommandValue(s string) string {
	r := strings.NewReplacer(
		"%0D", "\r", "%0d", "\r",
		"%0A", "\n", "%0a", "\n",
		"%2C", ",", "%2c", ",",
		"%3A", ":", "%3a", ":",
		"%25", "%",
	)
	return r.Replace(s)
}

func scrubField(s string, limit int, maskValues []string) string {
	s = redact(s, maskValues)
	s = stripANSI(s)
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
		if b.Len() >= limit {
			break
		}
	}
	return strings.TrimSpace(truncateUTF8(b.String(), limit))
}

func scrubPath(s string, maskValues []string) string {
	s = scrubField(s, MaxPathBytes, maskValues)
	s = strings.ReplaceAll(s, "\\", "/")
	return strings.TrimSpace(s)
}

func redact(s string, maskValues []string) string {
	if s == "" || len(maskValues) == 0 {
		return s
	}
	for _, value := range maskValues {
		if value == "" {
			continue
		}
		s = strings.ReplaceAll(s, value, "***")
	}
	return s
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			r, size := utf8.DecodeRuneInString(s[i:])
			b.WriteRune(r)
			i += size
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		if s[i] == ']' {
			i++
			for i < len(s) {
				if s[i] == 0x07 {
					i++
					break
				}
				if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
					i += 2
					break
				}
				i++
			}
			continue
		}
		if s[i] == '[' {
			i++
		}
		for i < len(s) {
			c := s[i]
			i++
			if c >= 0x40 && c <= 0x7e {
				break
			}
		}
	}
	return b.String()
}

func parsePositiveInt32(s string) int32 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32)
	if err != nil || n <= 0 {
		return 0
	}
	return int32(n)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

func fingerprint(ann Annotation) string {
	h := sha256.New()
	writeField := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	writeField(ann.Level)
	writeField(ann.Title)
	writeField(ann.Message)
	writeField(ann.Path)
	writeField(strconv.FormatInt(int64(ann.StartLine), 10))
	writeField(strconv.FormatInt(int64(ann.EndLine), 10))
	writeField(strconv.FormatInt(int64(ann.StartColumn), 10))
	writeField(strconv.FormatInt(int64(ann.EndColumn), 10))
	writeField(strconv.FormatInt(int64(ann.LogLine), 10))
	writeField(strconv.FormatInt(int64(ann.LogChunkSeq), 10))
	return hex.EncodeToString(h.Sum(nil))
}
