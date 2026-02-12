package util

import (
	"strings"
)

type ContentParts struct {
	ContentBefore           string
	ContentAfter            string
	LastCharacter           string
	LastLine                string
	ContentImmediatelyAfter string
}

func GetContent(contents string, line, column int) ContentParts {
	lines := strings.Split(contents, "\n")

	if line < 0 {
		line = 0
	}

	if line >= len(lines) {
		line = len(lines) - 1
	}

	beforeLines := make([]string, line+1)
	copy(beforeLines, lines[:line+1])

	if column >= 0 && column < len(beforeLines[line]) {
		beforeLines[line] = beforeLines[line][:column]
	}

	lastLine := beforeLines[len(beforeLines)-1]
	contentBefore := strings.Join(beforeLines, "\n")
	var contentAfter string

	if line+1 < len(lines) {
		contentAfter = strings.Join(lines[line+1:], "\n")
	}

	var lastCharacter string

	if len(contentBefore) > 0 {
		lastCharacter = string(contentBefore[len(contentBefore)-1])
	}

	var contentImmediatelyAfter string

	if line < len(lines) && column < len(lines[line]) {
		contentImmediatelyAfter = lines[line][column:]
	}

	return ContentParts{
		ContentBefore:           contentBefore,
		ContentAfter:            contentAfter,
		LastCharacter:           lastCharacter,
		LastLine:                lastLine,
		ContentImmediatelyAfter: contentImmediatelyAfter,
	}
}

func GetContentPadding(text string) int {
	lines := strings.Split(text, "\n")
	minPadding := 99999

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}

		padding := len(line) - len(strings.TrimLeft(line, " \t"))
		if padding < minPadding {
			minPadding = padding
		}
	}

	if minPadding == 99999 {
		return 0
	}
	return minPadding
}

func PadContent(text string, padding int) string {
	if padding <= 0 {
		return text
	}

	lines := strings.Split(text, "\n")
	paddingStr := strings.Repeat(" ", padding)

	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			lines[i] = paddingStr + line
		}
	}

	return strings.Join(lines, "\n")
}

// GetContentIndent returns the actual whitespace prefix of the least-indented
// non-blank line. Unlike GetContentPadding (which returns a count),
// this preserves the original characters (tabs vs spaces).
func GetContentIndent(text string) string {
	lines := strings.Split(text, "\n")
	var minIndent string
	first := true

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if first || len(indent) < len(minIndent) {
			minIndent = indent
			first = false
		}
	}
	return minIndent
}

func DedentContent(text string) string {
	indent := GetContentIndent(text)
	if indent == "" {
		return text
	}

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, indent) {
			lines[i] = line[len(indent):]
		}
	}
	return strings.Join(lines, "\n")
}

// IndentContent prepends the given prefix string to each non-blank line.
func IndentContent(text, prefix string) string {
	if prefix == "" {
		return text
	}

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

func TrimBlankLines(text string) string {
	lines := strings.Split(text, "\n")

	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}

	end := len(lines)
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}

	if start >= end {
		return ""
	}
	return strings.Join(lines[start:end], "\n")
}

func UniqueStrings(items []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(items))

	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}

	return result
}
