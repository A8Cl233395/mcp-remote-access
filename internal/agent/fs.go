package agent

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	MaxReadLines       = 2000
	MaxReadBytes       = 50 * 1024
	MaxLineChars       = 2000
	DefaultSearchLimit = 100
)

func ReadFileOrDir(rawPath, workdir string, offsetPtr, limitPtr *int) (string, map[string]any, error) {
	absPath := ResolvePath(rawPath, workdir)
	st, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, fmt.Errorf("File not found: %s", rawPath)
		}
		return "", nil, fmt.Errorf("Unable to read %s: %v", rawPath, err)
	}

	offset := 1
	if offsetPtr != nil && *offsetPtr > 0 {
		offset = *offsetPtr
	}
	limit := MaxReadLines
	if limitPtr != nil && *limitPtr > 0 && *limitPtr < MaxReadLines {
		limit = *limitPtr
	}

	if st.IsDir() {
		entries, err := os.ReadDir(absPath)
		if err != nil {
			return "", nil, fmt.Errorf("Unable to read directory %s: %v", rawPath, err)
		}
		type item struct {
			name  string
			isDir bool
		}
		items := make([]item, 0, len(entries))
		sep := string(filepath.Separator)
		for _, e := range entries {
			name := e.Name()
			isDir := e.IsDir()
			if isDir {
				name += sep
			}
			items = append(items, item{name: name, isDir: isDir})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].isDir != items[j].isDir {
				return items[i].isDir
			}
			return items[i].name < items[j].name
		})

		startIdx := offset - 1
		if startIdx > len(items) {
			startIdx = len(items)
		}
		endIdx := startIdx + limit
		truncated := false
		if endIdx < len(items) {
			truncated = true
		} else {
			endIdx = len(items)
		}
		selected := items[startIdx:endIdx]

		var out []string
		if len(selected) == 0 {
			out = append(out, fmt.Sprintf("Read directory %s, 0 entries", rawPath))
		} else {
			out = append(out, fmt.Sprintf("Read directory %s, entries %d-%d", rawPath, offset, offset+len(selected)-1))
			for _, it := range selected {
				out = append(out, it.name)
			}
		}
		if truncated {
			nextOffset := offset + len(selected)
			out = append(out, fmt.Sprintf("[Output truncated. Continue reading with offset: %d]", nextOffset))
		}
		return strings.Join(out, "\n"), map[string]any{"truncated": truncated}, nil
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", nil, fmt.Errorf("Unable to read %s: %v", rawPath, err)
	}

	checkLen := len(data)
	if checkLen > 8192 {
		checkLen = 8192
	}
	if bytes.IndexByte(data[:checkLen], 0) != -1 {
		return "", nil, fmt.Errorf("Cannot read binary file: %s", rawPath)
	}

	if len(data) == 0 {
		return fmt.Sprintf("Read file %s, 0 lines", rawPath), map[string]any{"truncated": false}, nil
	}

	text := string(data)
	rawLines := strings.Split(text, "\n")
	if len(rawLines) > 0 && rawLines[len(rawLines)-1] == "" {
		rawLines = rawLines[:len(rawLines)-1]
	}
	for i := range rawLines {
		rawLines[i] = strings.TrimSuffix(rawLines[i], "\r")
	}

	totalLines := len(rawLines)
	if offset > totalLines && offset != 1 {
		word := "lines"
		if totalLines == 1 {
			word = "line"
		}
		return "", nil, fmt.Errorf("Offset %d is out of range for this file (%d %s)", offset, totalLines, word)
	}

	var selected []string
	var nextOffset *int
	bytesUsed := 0
	for idx := offset - 1; idx < totalLines; idx++ {
		lineNum := idx + 1
		if len(selected) >= limit || bytesUsed >= MaxReadBytes {
			n := lineNum
			nextOffset = &n
			break
		}
		lineText := clampLine(rawLines[idx])
		lineBytes := len(lineText)
		if len(selected) > 0 {
			lineBytes++
		}
		if bytesUsed+lineBytes > MaxReadBytes && len(selected) > 0 {
			n := lineNum
			nextOffset = &n
			break
		}
		selected = append(selected, fmt.Sprintf("%d: %s", lineNum, lineText))
		bytesUsed += lineBytes
	}

	truncated := nextOffset != nil
	var out []string
	if len(selected) == 0 {
		out = append(out, fmt.Sprintf("Read file %s, 0 lines", rawPath))
	} else {
		out = append(out, fmt.Sprintf("Read file %s, lines %d-%d", rawPath, offset, offset+len(selected)-1))
		out = append(out, selected...)
	}
	if truncated && nextOffset != nil {
		out = append(out, fmt.Sprintf("[Output truncated. Continue reading with offset: %d]", *nextOffset))
	}

	return strings.Join(out, "\n"), map[string]any{"truncated": truncated}, nil
}

func clampLine(s string) string {
	if utf8.RuneCountInString(s) <= MaxLineChars {
		return s
	}
	runes := []rune(s)
	return string(runes[:MaxLineChars]) + fmt.Sprintf("... (line truncated to %d chars)", MaxLineChars)
}

func WriteFile(rawPath, content, workdir string) (string, map[string]any, error) {
	absPath := ResolvePath(rawPath, workdir)
	_, statErr := os.Stat(absPath)
	existed := statErr == nil

	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return "", nil, fmt.Errorf("Unable to create parent directories for %s: %v", rawPath, err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return "", nil, fmt.Errorf("Unable to write %s: %v", rawPath, err)
	}

	verb := "Created"
	if existed {
		verb = "Wrote"
	}
	return fmt.Sprintf("%s file successfully: %s", verb, rawPath), map[string]any{
		"operation": "write",
		"target":    absPath,
		"resource":  rawPath,
		"existed":   existed,
	}, nil
}

var unicodeReplacer = strings.NewReplacer(
	"‘", "'", "’", "'", "‚", "'", "‛", "'",
	"“", `"`, "”", `"`, "„", `"`, "‟", `"`,
	"‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "―", "-", "−", "-",
	"\u00A0", " ", "\u2002", " ", "\u2003", " ", "\u2004", " ", "\u2005", " ",
	"\u2006", " ", "\u2007", " ", "\u2008", " ", "\u2009", " ", "\u200A", " ",
	"\u202F", " ", "\u205F", " ", "\u3000", " ",
)

func normalizeForMatch(s string) string {
	return unicodeReplacer.Replace(s)
}

type byteSpan struct {
	start int
	end   int
}

func findOccurrences(content, search string) []byteSpan {
	if search == "" {
		return nil
	}
	var out []byteSpan
	offset := 0
	for {
		idx := strings.Index(content[offset:], search)
		if idx == -1 {
			break
		}
		realStart := offset + idx
		out = append(out, byteSpan{start: realStart, end: realStart + len(search)})
		offset = realStart + len(search)
	}
	return out
}

func findLineTrimmedOccurrences(view, search string) []byteSpan {
	trailingNL := strings.HasSuffix(search, "\n")
	expected := strings.Split(search, "\n")
	if trailingNL && len(expected) > 0 {
		expected = expected[:len(expected)-1]
	}
	if len(expected) == 0 {
		return nil
	}

	type lineInfo struct {
		start      int
		end        int
		contentEnd int
		text       string
		hasNL      bool
	}
	var lines []lineInfo
	pos := 0
	for pos < len(view) {
		nlIdx := strings.IndexByte(view[pos:], '\n')
		if nlIdx == -1 {
			txt := view[pos:]
			lines = append(lines, lineInfo{
				start:      pos,
				end:        len(view),
				contentEnd: len(view),
				text:       txt,
				hasNL:      false,
			})
			break
		}
		endPos := pos + nlIdx + 1
		txt := view[pos : pos+nlIdx]
		lines = append(lines, lineInfo{
			start:      pos,
			end:        endPos,
			contentEnd: pos + nlIdx,
			text:       txt,
			hasNL:      true,
		})
		pos = endPos
	}

	var matches []byteSpan
	for i := 0; i+len(expected) <= len(lines); i++ {
		ok := true
		for j := 0; j < len(expected); j++ {
			a := normalizeForMatch(strings.TrimRight(lines[i+j].text, " \t"))
			b := normalizeForMatch(strings.TrimRight(expected[j], " \t"))
			if a != b {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		last := lines[i+len(expected)-1]
		if trailingNL && !last.hasNL {
			continue
		}
		end := last.contentEnd
		if trailingNL {
			end = last.end
		}
		cand := byteSpan{start: lines[i].start, end: end}
		overlap := false
		for _, m := range matches {
			if m.end > cand.start && m.start < cand.end {
				overlap = true
				break
			}
		}
		if !overlap {
			matches = append(matches, cand)
		}
	}
	return matches
}

func dominantLineEnding(s string) string {
	crlf := strings.Count(s, "\r\n")
	lf := strings.Count(s, "\n") - crlf
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

func EditFile(rawPath, oldString, newString string, replaceAll bool, workdir string) (string, map[string]any, error) {
	if oldString == newString {
		return "", nil, fmt.Errorf("No changes to apply: oldString and newString are identical.")
	}
	if oldString == "" {
		return "", nil, fmt.Errorf("oldString must not be empty. Use write to create or overwrite a file.")
	}

	absPath := ResolvePath(rawPath, workdir)
	st, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, fmt.Errorf("File not found: %s", rawPath)
		}
		return "", nil, fmt.Errorf("Unable to edit %s: %v", rawPath, err)
	}
	if st.IsDir() {
		return "", nil, fmt.Errorf("Path is a directory, not a file: %s", rawPath)
	}

	rawBytes, err := os.ReadFile(absPath)
	if err != nil {
		return "", nil, fmt.Errorf("Unable to edit %s: %v", rawPath, err)
	}
	source := string(rawBytes)

	endingsOnly := strings.ReplaceAll(oldString, "\r", "") == strings.ReplaceAll(newString, "\r", "")
	if endingsOnly {
		matches := findOccurrences(source, oldString)
		if len(matches) == 0 {
			return "", nil, fmt.Errorf("Could not find oldString in %s.", rawPath)
		}
		if len(matches) > 1 && !replaceAll {
			return "", nil, fmt.Errorf("Found %d matches for oldString, but expected exactly one. Add more surrounding context to make oldString unique, or set replaceAll to true to replace every occurrence.", len(matches))
		}
		var replaced string
		if replaceAll {
			replaced = strings.ReplaceAll(source, oldString, newString)
		} else {
			replaced = strings.Replace(source, oldString, newString, 1)
		}
		if err := os.WriteFile(absPath, []byte(replaced), st.Mode().Perm()); err != nil {
			return "", nil, err
		}
		plural := "s"
		if len(matches) == 1 {
			plural = ""
		}
		return fmt.Sprintf("Edited %s (%d replacement%s)", rawPath, len(matches), plural), map[string]any{
			"replacements": len(matches),
		}, nil
	}

	fileEnding := dominantLineEnding(source)
	view := strings.ReplaceAll(source, "\r\n", "\n")
	search := strings.ReplaceAll(oldString, "\r\n", "\n")
	replacement := strings.ReplaceAll(newString, "\r\n", "\n")

	matches := findOccurrences(view, search)
	if len(matches) == 0 {
		// Tier 2: Unicode normalized match (preserving line indices by mapping via lines if lengths differ, or exact index if equal length)
		normView := normalizeForMatch(view)
		normSearch := normalizeForMatch(search)
		if len(normView) == len(view) && len(normSearch) == len(search) {
			matches = findOccurrences(normView, normSearch)
		}
	}
	if len(matches) == 0 {
		// Tier 3: Line-by-line trimmed match
		matches = findLineTrimmedOccurrences(view, search)
	}

	if len(matches) == 0 {
		hint := buildMissHint(view, search, replacement)
		if hint != "" {
			return "", nil, fmt.Errorf("Could not find oldString in %s. It must match exactly, including whitespace and indentation.\n\n%s", rawPath, hint)
		}
		return "", nil, fmt.Errorf("Could not find oldString in %s. It must match exactly, including whitespace and indentation.", rawPath)
	}

	if len(matches) > 1 && !replaceAll {
		return "", nil, fmt.Errorf("Found %d matches for oldString, but expected exactly one. Add more surrounding context to make oldString unique, or set replaceAll to true to replace every occurrence.", len(matches))
	}

	toApply := matches
	if !replaceAll {
		toApply = matches[:1]
	}

	var buf strings.Builder
	lastPos := 0
	for _, m := range toApply {
		buf.WriteString(view[lastPos:m.start])
		buf.WriteString(replacement)
		lastPos = m.end
	}
	buf.WriteString(view[lastPos:])
	newView := buf.String()

	var finalContent string
	if fileEnding == "\r\n" {
		finalContent = strings.ReplaceAll(newView, "\n", "\r\n")
	} else {
		finalContent = newView
	}

	if finalContent == source {
		return "", nil, fmt.Errorf("No changes to apply: the edit leaves %s unchanged.", rawPath)
	}

	if err := os.WriteFile(absPath, []byte(finalContent), st.Mode().Perm()); err != nil {
		return "", nil, fmt.Errorf("Unable to write edited file %s: %v", rawPath, err)
	}

	count := len(toApply)
	plural := "s"
	if count == 1 {
		plural = ""
	}
	return fmt.Sprintf("Edited %s (%d replacement%s)", rawPath, count, plural), map[string]any{
		"replacements": count,
	}, nil
}

func buildMissHint(view, search, replacement string) string {
	if len(strings.TrimSpace(replacement)) >= 20 && strings.Contains(normalizeForMatch(view), normalizeForMatch(replacement)) {
		return "newString is already in the file, so this edit may already be applied."
	}
	searchLines := strings.Split(search, "\n")
	var firstNonEmpty string
	for _, l := range searchLines {
		trimmed := strings.TrimSpace(l)
		if len(trimmed) >= 4 {
			firstNonEmpty = trimmed
			break
		}
	}
	if firstNonEmpty == "" {
		return ""
	}
	lines := strings.Split(view, "\n")
	for idx, l := range lines {
		if strings.Contains(strings.TrimSpace(l), firstNonEmpty) {
			from := idx
			if from > 2 {
				from -= 2
			} else {
				from = 0
			}
			to := idx + len(searchLines) + 2
			if to > len(lines) {
				to = len(lines)
			}
			if to-from > 12 {
				to = from + 12
			}
			out := []string{"The closest lines in the file:"}
			for k := from; k < to; k++ {
				out = append(out, fmt.Sprintf("%d: %s", k+1, clampLine(lines[k])))
			}
			return strings.Join(out, "\n")
		}
	}
	return ""
}

func GlobFiles(workdir, pattern, searchPath string, hidden bool, limit int) (string, map[string]any, error) {
	if strings.TrimSpace(pattern) == "" {
		return "", nil, fmt.Errorf("pattern must not be empty")
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	targetDir := workdir
	if strings.TrimSpace(searchPath) != "" && searchPath != "undefined" && searchPath != "null" {
		targetDir = ResolvePath(searchPath, workdir)
	}
	st, err := os.Stat(targetDir)
	if err != nil {
		return "", nil, fmt.Errorf("Search path does not exist: %s", targetDir)
	}
	if !st.IsDir() {
		return "", nil, fmt.Errorf("Search path is not a directory: %s", targetDir)
	}

	matcher, err := compileGlobRegex(pattern)
	if err != nil {
		return "", nil, fmt.Errorf("Invalid glob pattern %q: %v", pattern, err)
	}

	var matched []string
	truncated := false

	_ = filepath.WalkDir(targetDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if p == targetDir {
			return nil
		}
		name := d.Name()
		if !hidden && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(targetDir, p)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if matcher.MatchString(relSlash) || matcher.MatchString(name) {
			if len(matched) >= limit {
				truncated = true
				return fs.SkipAll
			}
			matched = append(matched, p)
		}
		return nil
	})

	if len(matched) == 0 {
		return "No files found", map[string]any{"count": 0, "truncated": false}, nil
	}
	lines := append([]string{}, matched...)
	if truncated {
		lines = append(lines, "", fmt.Sprintf("(Results are truncated: showing first %d results. Consider using a more specific path or pattern.)", len(matched)))
	}
	return strings.Join(lines, "\n"), map[string]any{"count": len(matched), "truncated": truncated}, nil
}

func GrepFiles(workdir, pattern, searchPath, include string, literal, caseSensitive bool, limit int) (string, map[string]any, error) {
	if pattern == "" {
		return "", nil, fmt.Errorf("Pattern must not be empty")
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}

	expr := pattern
	if literal {
		expr = regexp.QuoteMeta(pattern)
	}
	if !caseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", nil, fmt.Errorf("Invalid regex pattern: %v", err)
	}

	var includeRe *regexp.Regexp
	if strings.TrimSpace(include) != "" {
		includeRe, err = compileGlobRegex(include)
		if err != nil {
			return "", nil, fmt.Errorf("Invalid include glob %q: %v", include, err)
		}
	}

	target := workdir
	if strings.TrimSpace(searchPath) != "" && searchPath != "undefined" && searchPath != "null" {
		target = ResolvePath(searchPath, workdir)
	}
	st, err := os.Stat(target)
	if err != nil {
		return "", nil, fmt.Errorf("Search path does not exist: %s", target)
	}

	type grepMatch struct {
		file string
		line int
		text string
	}
	var matches []grepMatch
	truncated := false

	searchSingleFile := func(filePath string) {
		data, err := os.ReadFile(filePath)
		if err != nil || len(data) == 0 {
			return
		}
		checkLen := len(data)
		if checkLen > 8192 {
			checkLen = 8192
		}
		if bytes.IndexByte(data[:checkLen], 0) != -1 {
			return
		}
		lines := strings.Split(string(data), "\n")
		for idx, rawLine := range lines {
			line := strings.TrimSuffix(rawLine, "\r")
			if re.MatchString(line) {
				if len(matches) >= limit {
					truncated = true
					return
				}
				matches = append(matches, grepMatch{
					file: filePath,
					line: idx + 1,
					text: clampLine(line),
				})
			}
		}
	}

	if !st.IsDir() {
		searchSingleFile(target)
	} else {
		_ = filepath.WalkDir(target, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if p == target {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if name == ".git" || name == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if includeRe != nil {
				rel, _ := filepath.Rel(target, p)
				relSlash := filepath.ToSlash(rel)
				if !includeRe.MatchString(relSlash) && !includeRe.MatchString(name) {
					return nil
				}
			}
			searchSingleFile(p)
			if truncated {
				return fs.SkipAll
			}
			return nil
		})
	}

	if len(matches) == 0 {
		return "No matches found", map[string]any{"matches": 0, "truncated": false}, nil
	}

	lines := []string{fmt.Sprintf("Found %d matches", len(matches))}
	currentFile := ""
	for _, m := range matches {
		if currentFile != m.file {
			if currentFile != "" {
				lines = append(lines, "")
			}
			currentFile = m.file
			lines = append(lines, fmt.Sprintf("%s:", m.file))
		}
		lines = append(lines, fmt.Sprintf("  Line %d: %s", m.line, m.text))
	}
	if truncated {
		lines = append(lines, "", fmt.Sprintf("(Results are truncated: showing first %d results. Consider using a more specific path or pattern.)", len(matches)))
	}
	return strings.Join(lines, "\n"), map[string]any{"matches": len(matches), "truncated": truncated}, nil
}

// compileGlobRegex converts a glob pattern supporting **, *, ?, and {a,b} into a *regexp.Regexp.
func compileGlobRegex(pattern string) (*regexp.Regexp, error) {
	p := filepath.ToSlash(strings.TrimSpace(pattern))
	p = strings.TrimPrefix(p, "./")

	var sb strings.Builder
	sb.WriteString("^")
	i := 0
	for i < len(p) {
		ch := p[i]
		switch ch {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				if i+2 < len(p) && p[i+2] == '/' {
					sb.WriteString("(?:.*/)?")
					i += 3
					continue
				}
				sb.WriteString(".*")
				i += 2
				continue
			}
			sb.WriteString("[^/]*")
		case '?':
			sb.WriteString("[^/]")
		case '{':
			end := strings.IndexByte(p[i:], '}')
			if end != -1 {
				inner := p[i+1 : i+end]
				parts := strings.Split(inner, ",")
				for idx, part := range parts {
					parts[idx] = regexp.QuoteMeta(part)
				}
				sb.WriteString("(?:")
				sb.WriteString(strings.Join(parts, "|"))
				sb.WriteString(")")
				i += end + 1
				continue
			}
			sb.WriteString(`\{`)
		case '.', '+', '^', '$', '(', ')', '[', ']', '|', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(ch)
		default:
			sb.WriteByte(ch)
		}
		i++
	}
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}
