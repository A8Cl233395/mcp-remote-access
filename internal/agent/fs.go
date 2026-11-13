package agent

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxReadLines       = 2000
	MaxReadBytes       = 50 * 1024
	FirstReadChunk     = 256 * 1024
	MaxLineChars       = 2000
	DefaultSearchLimit = 100
	HintLines          = 12
	HintLineLength     = 200
)

type textLeaf struct {
	bytes []byte
	lines int
}

func makeTextLeaf(b []byte) textLeaf {
	return textLeaf{
		bytes: b,
		lines: bytes.Count(b, []byte{'\n'}),
	}
}

func lineCountFromLeaves(totalNewlines int, lastByte byte, hasBytes bool) int {
	if !hasBytes {
		return 0
	}
	if lastByte == '\n' {
		return totalNewlines
	}
	return totalNewlines + 1
}

func nthNewlineOffset(b []byte, count int) (int, bool) {
	if count <= 0 {
		return 0, true
	}
	found := 0
	for i, c := range b {
		if c == '\n' {
			found++
			if found == count {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func findStartByteOffset(leaves []textLeaf, targetNewline int) int {
	if targetNewline <= 0 {
		return 0
	}
	remaining := targetNewline
	offset := 0
	for _, leaf := range leaves {
		if remaining <= leaf.lines {
			if pos, ok := nthNewlineOffset(leaf.bytes, remaining); ok {
				return offset + pos
			}
			return offset + len(leaf.bytes)
		}
		remaining -= leaf.lines
		offset += len(leaf.bytes)
	}
	return offset
}

type pageResult struct {
	entries  []string
	offset   int
	next     *int
	consumed int
}

// computeTextPage mirrors OpenCode's textPage in read-filesystem.ts.
func computeTextPage(data []byte, eof bool, offset, limit int) (*pageResult, bool) {
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 || limit > MaxReadLines {
		limit = MaxReadLines
	}

	decoded := string(data)
	split := strings.Split(decoded, "\n")
	var complete []string
	if eof {
		if len(split) > 0 && split[len(split)-1] == "" {
			complete = split[:len(split)-1]
		} else {
			complete = split
		}
	} else {
		if len(split) > 0 {
			complete = split[:len(split)-1]
		}
	}

	available := make([]string, len(complete))
	for i, line := range complete {
		available[i] = strings.TrimSuffix(line, "\r")
	}

	var entries []string
	size := 0
	var next *int

	startIdx := offset - 1
	if startIdx < 0 {
		startIdx = 0
	}
	if startIdx < len(available) {
		for index, value := range available[startIdx:] {
			line := offset + index
			if len(entries) >= limit || size >= MaxReadBytes {
				n := line
				next = &n
				break
			}
			text := clampLine(value)
			lineSize := len(text)
			if len(entries) > 0 {
				lineSize++
			}
			if size+lineSize > MaxReadBytes && len(entries) > 0 {
				n := line
				next = &n
				break
			}
			entries = append(entries, text)
			size += lineSize
		}
	}

	if next == nil && len(entries) >= limit && (!eof || startIdx+len(entries) < len(available)) {
		n := offset + len(entries)
		next = &n
	}
	if !eof && next == nil {
		return nil, false
	}

	consumedLines := len(available)
	if next != nil {
		consumedLines = *next - 1
	}
	consumed := 0
	if consumedLines > 0 {
		if pos, ok := nthNewlineOffset(data, consumedLines); ok {
			consumed = pos
		} else {
			consumed = len(data)
		}
	}

	return &pageResult{
		entries:  entries,
		offset:   offset,
		next:     next,
		consumed: consumed,
	}, true
}

func canonicalFileName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch r {
		case '\u00a0', '\u202f':
			sb.WriteByte(' ')
		case '\u2018', '\u2019':
			sb.WriteByte('\'')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// findAlternateFilePath mirrors OpenCode's ReadTool.alternatePath:
// when a file is not found, check if the parent directory has a single file whose canonicalized name matches.
func findAlternateFilePath(absPath string) string {
	dir := filepath.Dir(absPath)
	base := canonicalFileName(filepath.Base(absPath))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var matches []string
	for _, e := range entries {
		if !e.IsDir() && canonicalFileName(e.Name()) == base {
			matches = append(matches, filepath.Join(dir, e.Name()))
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func similarNames(names []string, target string) []string {
	base := strings.ToLower(target)
	if base == "" {
		return nil
	}
	var out []string
	for _, entry := range names {
		cand := strings.ToLower(entry)
		if strings.Contains(cand, base) || strings.Contains(base, cand) {
			out = append(out, entry)
			if len(out) >= 3 {
				break
			}
		}
	}
	return out
}

// formatMissingError mirrors OpenCode's ReadTool.missing and missingFolder diagnostics.
func formatMissingError(rawInput, absPath string) error {
	folder := filepath.Dir(absPath)
	entries, err := os.ReadDir(folder)
	if err == nil {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		suggestions := similarNames(names, filepath.Base(rawInput))
		if len(suggestions) > 0 {
			rawDir := filepath.Dir(rawInput)
			var fullSuggestions []string
			for _, s := range suggestions {
				if rawDir == "." || rawDir == "" {
					fullSuggestions = append(fullSuggestions, s)
				} else {
					fullSuggestions = append(fullSuggestions, filepath.Join(rawDir, s))
				}
			}
			return fmt.Errorf("File not found: %s\n\nDid you mean one of these?\n%s", rawInput, strings.Join(fullSuggestions, "\n"))
		}
		return fmt.Errorf("File not found: %s", rawInput)
	}

	// Folder itself is missing: walk up to find nearest existing parent
	child := folder
	parent := filepath.Dir(folder)
	for parent != child {
		pEntries, pErr := os.ReadDir(parent)
		if pErr == nil {
			var names []string
			for _, e := range pEntries {
				names = append(names, e.Name())
			}
			suggestions := similarNames(names, filepath.Base(child))
			var fullSuggestions []string
			for _, s := range suggestions {
				fullSuggestions = append(fullSuggestions, filepath.Join(parent, s))
			}
			parts := []string{
				fmt.Sprintf("File not found: %s", rawInput),
				fmt.Sprintf("The folder %s does not exist. The nearest folder that does is %s.", child, parent),
			}
			if len(fullSuggestions) > 0 {
				parts = append(parts, fmt.Sprintf("Did you mean one of these?\n%s", strings.Join(fullSuggestions, "\n")))
			}
			return fmt.Errorf("%s", strings.Join(parts, "\n\n"))
		}
		child = parent
		parent = filepath.Dir(parent)
	}
	return fmt.Errorf("File not found: %s", rawInput)
}

func ReadFileOrDir(rawPath, workdir string, offsetPtr, limitPtr *int) (string, map[string]any, error) {
	return ReadFileOrDirWithEncoding(rawPath, workdir, offsetPtr, limitPtr, "")
}

func ReadFileOrDirWithEncoding(rawPath, workdir string, offsetPtr, limitPtr *int, encodingSpec string) (string, map[string]any, error) {
	absPath := ResolvePath(rawPath, workdir)
	displayPath := rawPath

	st, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			if alt := findAlternateFilePath(absPath); alt != "" {
				if altSt, altErr := os.Stat(alt); altErr == nil && !altSt.IsDir() {
					absPath = alt
					st = altSt
					err = nil
					rawDir := filepath.Dir(rawPath)
					if rawDir == "." || rawDir == "" {
						displayPath = filepath.Base(alt)
					} else {
						displayPath = filepath.Join(rawDir, filepath.Base(alt))
					}
				}
			}
			if err != nil {
				return "", nil, formatMissingError(rawPath, absPath)
			}
		} else {
			return "", nil, fmt.Errorf("Unable to read %s: %v", rawPath, err)
		}
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
			return "", nil, fmt.Errorf("Unable to read directory %s: %v", displayPath, err)
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
			out = append(out, fmt.Sprintf("Read directory %s, 0 entries", displayPath))
		} else {
			out = append(out, fmt.Sprintf("Read directory %s, entries %d-%d", displayPath, offset, offset+len(selected)-1))
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

	f, err := os.Open(absPath)
	if err != nil {
		return "", nil, fmt.Errorf("Unable to read %s: %v", displayPath, err)
	}
	defer f.Close()

	fileSize := st.Size()
	firstBuf := make([]byte, FirstReadChunk)
	nFirst, readErr := io.ReadFull(f, firstBuf)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return "", nil, fmt.Errorf("Unable to read %s: %v", displayPath, readErr)
	}
	firstBytes := firstBuf[:nFirst]
	fullFileInFirst := int64(len(firstBytes)) >= fileSize

	var encInfo EncodingInfo
	if strings.TrimSpace(encodingSpec) == "" || strings.EqualFold(strings.TrimSpace(encodingSpec), "auto") {
		encInfo, err = DetectTextEncoding(firstBytes, fullFileInFirst)
		if err != nil {
			if errors.Is(err, errBinaryDetected) {
				return "", nil, fmt.Errorf("Cannot read binary file: %s", displayPath)
			}
			return "", nil, fmt.Errorf("Unable to detect encoding for %s: %v", displayPath, err)
		}
	} else {
		encInfo, err = ResolveExplicitReadEncoding(encodingSpec, firstBytes)
		if err != nil {
			return "", nil, err
		}
	}

	// If the first chunk already contains the entire file
	if fullFileInFirst {
		decodedStr, decErr := DecodeToUTF8(firstBytes, encInfo)
		if decErr != nil {
			return "", nil, fmt.Errorf("Unable to decode %s (%s): %v", displayPath, encInfo.Name, decErr)
		}
		decodedBytes := []byte(decodedStr)
		lineCount := bytes.Count(decodedBytes, []byte{'\n'})
		if len(decodedBytes) > 0 && decodedBytes[len(decodedBytes)-1] != '\n' {
			lineCount++
		}
		paged := len(decodedBytes) > MaxReadBytes || lineCount > MaxReadLines || offsetPtr != nil || limitPtr != nil
		if !paged {
			if len(decodedBytes) == 0 {
				header := fmt.Sprintf("Read file %s, 0 lines", displayPath)
				if encInfo.Name != "utf-8" {
					header += fmt.Sprintf(" (encoding: %s)", encInfo.Name)
				}
				return header, map[string]any{"truncated": false, "encoding": encInfo.Name}, nil
			}
			text := strings.TrimSuffix(decodedStr, "\n")
			rawLines := strings.Split(text, "\n")
			out := make([]string, 0, len(rawLines)+1)
			header := fmt.Sprintf("Read file %s, lines 1-%d", displayPath, len(rawLines))
			if encInfo.Name != "utf-8" {
				header += fmt.Sprintf(" (encoding: %s)", encInfo.Name)
			}
			out = append(out, header)
			for i, l := range rawLines {
				l = strings.TrimSuffix(l, "\r")
				out = append(out, fmt.Sprintf("%d: %s", i+1, clampLine(l)))
			}
			return strings.Join(out, "\n"), map[string]any{"truncated": false, "encoding": encInfo.Name}, nil
		}

		res, ok := computeTextPage(decodedBytes, true, offset, limit)
		if !ok {
			return "", nil, fmt.Errorf("Unable to read %s", displayPath)
		}
		leaf := makeTextLeaf(decodedBytes)
		var lastByte byte
		if len(decodedBytes) > 0 {
			lastByte = decodedBytes[len(decodedBytes)-1]
		}
		totalLines := lineCountFromLeaves(leaf.lines, lastByte, len(decodedBytes) > 0)
		return formatTextPageOutput(displayPath, res, totalLines, encInfo.Name)
	}

	// Multi-chunk streaming read for large files (> FirstReadChunk)
	if encInfo.Enc == nil {
		// Fast path for UTF-8 / UTF-8-BOM
		initial := firstBytes
		if encInfo.HasBOM && len(encInfo.BOM) > 0 && bytes.HasPrefix(initial, encInfo.BOM) {
			initial = initial[len(encInfo.BOM):]
		}
		leaves := []textLeaf{makeTextLeaf(initial)}
		totalBytes := int64(len(firstBytes))
		totalNewlines := leaves[0].lines
		ended := false

		for {
			eof := ended || totalBytes >= fileSize
			if totalNewlines >= offset-1 || eof {
				startByte := findStartByteOffset(leaves, offset-1)
				var selectedBuf bytes.Buffer
				pos := 0
				for _, leaf := range leaves {
					leafStart := pos
					pos += len(leaf.bytes)
					if pos <= startByte {
						continue
					}
					subStart := startByte - leafStart
					if subStart < 0 {
						subStart = 0
					}
					selectedBuf.Write(leaf.bytes[subStart:])
				}

				res, ok := computeTextPage(selectedBuf.Bytes(), eof, 1, limit)
				if ok {
					res.offset = offset
					if res.next != nil {
						nextTranslated := offset + *res.next - 1
						res.next = &nextTranslated
					}
					consumedTotal := startByte + res.consumed
					checked := 0
					for _, leaf := range leaves {
						checkLen := len(leaf.bytes)
						if consumedTotal-checked < checkLen {
							checkLen = consumedTotal - checked
						}
						checked += len(leaf.bytes)
						if checkLen > 0 && bytes.IndexByte(leaf.bytes[:checkLen], 0) != -1 {
							return "", nil, fmt.Errorf("Cannot read binary file: %s", displayPath)
						}
						if checked >= consumedTotal {
							break
						}
					}
					var lastByte byte
					hasBytes := false
					if len(leaves) > 0 {
						lastLeaf := leaves[len(leaves)-1].bytes
						if len(lastLeaf) > 0 {
							lastByte = lastLeaf[len(lastLeaf)-1]
							hasBytes = true
						}
					}
					totalLines := lineCountFromLeaves(totalNewlines, lastByte, hasBytes)
					return formatTextPageOutput(displayPath, res, totalLines, encInfo.Name)
				}
			}

			nextChunk := make([]byte, FirstReadChunk)
			nNext, nextErr := io.ReadFull(f, nextChunk)
			if nNext == 0 {
				ended = true
				continue
			}
			leaf := makeTextLeaf(nextChunk[:nNext])
			leaves = append(leaves, leaf)
			totalBytes += int64(nNext)
			totalNewlines += leaf.lines
			if nextErr == io.EOF || nextErr == io.ErrUnexpectedEOF {
				ended = true
			}
		}
	}

	// Streaming decode path for large non-UTF-8 files (GBK, GB18030, UTF-16LE/BE, etc.)
	rawTail := firstBytes
	if encInfo.HasBOM && len(encInfo.BOM) > 0 && bytes.HasPrefix(rawTail, encInfo.BOM) {
		rawTail = rawTail[len(encInfo.BOM):]
	}
	utf8Stream := NewUTF8DecodeReader(io.MultiReader(bytes.NewReader(rawTail), f), encInfo)
	var leaves []textLeaf
	totalNewlines := 0
	ended := false

	for {
		nextChunk := make([]byte, FirstReadChunk)
		nNext, nextErr := io.ReadFull(utf8Stream, nextChunk)
		if nextErr != nil && nextErr != io.EOF && nextErr != io.ErrUnexpectedEOF {
			if errors.Is(nextErr, errBinaryDetected) {
				return "", nil, fmt.Errorf("Cannot read binary file: %s", displayPath)
			}
			return "", nil, fmt.Errorf("Unable to decode %s (%s): %v", displayPath, encInfo.Name, nextErr)
		}
		if nNext > 0 {
			leaf := makeTextLeaf(nextChunk[:nNext])
			leaves = append(leaves, leaf)
			totalNewlines += leaf.lines
		}
		if nNext == 0 || nextErr == io.EOF || nextErr == io.ErrUnexpectedEOF {
			ended = true
		}

		if totalNewlines >= offset-1 || ended {
			startByte := findStartByteOffset(leaves, offset-1)
			var selectedBuf bytes.Buffer
			pos := 0
			for _, leaf := range leaves {
				leafStart := pos
				pos += len(leaf.bytes)
				if pos <= startByte {
					continue
				}
				subStart := startByte - leafStart
				if subStart < 0 {
					subStart = 0
				}
				selectedBuf.Write(leaf.bytes[subStart:])
			}

			res, ok := computeTextPage(selectedBuf.Bytes(), ended, 1, limit)
			if ok {
				res.offset = offset
				if res.next != nil {
					nextTranslated := offset + *res.next - 1
					res.next = &nextTranslated
				}
				var lastByte byte
				hasBytes := false
				if len(leaves) > 0 {
					lastLeaf := leaves[len(leaves)-1].bytes
					if len(lastLeaf) > 0 {
						lastByte = lastLeaf[len(lastLeaf)-1]
						hasBytes = true
					}
				}
				totalLines := lineCountFromLeaves(totalNewlines, lastByte, hasBytes)
				return formatTextPageOutput(displayPath, res, totalLines, encInfo.Name)
			}
		}
		if ended {
			return "", nil, fmt.Errorf("Unable to read %s", displayPath)
		}
	}
}

func formatTextPageOutput(displayPath string, res *pageResult, totalLines int, encName string) (string, map[string]any, error) {
	if len(res.entries) == 0 && res.offset != 1 {
		word := "lines"
		if totalLines == 1 {
			word = "line"
		}
		return "", nil, fmt.Errorf("Offset %d is out of range for this file (%d %s)", res.offset, totalLines, word)
	}
	truncated := res.next != nil
	var out []string
	encSuffix := ""
	if encName != "" && encName != "utf-8" {
		encSuffix = fmt.Sprintf(" (encoding: %s)", encName)
	}
	if len(res.entries) == 0 {
		out = append(out, fmt.Sprintf("Read file %s, 0 lines%s", displayPath, encSuffix))
	} else {
		out = append(out, fmt.Sprintf("Read file %s, lines %d-%d%s", displayPath, res.offset, res.offset+len(res.entries)-1, encSuffix))
		for i, line := range res.entries {
			out = append(out, fmt.Sprintf("%d: %s", res.offset+i, line))
		}
	}
	if truncated && res.next != nil {
		out = append(out, fmt.Sprintf("[Output truncated. Continue reading with offset: %d]", *res.next))
	}
	meta := map[string]any{"truncated": truncated}
	if encName != "" {
		meta["encoding"] = encName
	}
	return strings.Join(out, "\n"), meta, nil
}

func clampLine(s string) string {
	if utf8.RuneCountInString(s) <= MaxLineChars {
		return s
	}
	runes := []rune(s)
	return string(runes[:MaxLineChars]) + fmt.Sprintf("... (line truncated to %d chars)", MaxLineChars)
}

func WriteFile(rawPath, content, workdir string) (string, map[string]any, error) {
	return WriteFileWithEncoding(rawPath, content, workdir, "")
}

func WriteFileWithEncoding(rawPath, content, workdir, encodingSpec string) (string, map[string]any, error) {
	if strings.EqualFold(strings.TrimSpace(encodingSpec), "auto") {
		return "", nil, fmt.Errorf("write does not support \"auto\" encoding; omit encoding to default to \"utf-8\", or specify an explicit encoding such as \"gbk\", \"gb18030\", \"utf-8-bom\", or \"utf-16le\"")
	}
	encInfo, err := LookupEncoding(encodingSpec)
	if err != nil {
		return "", nil, err
	}
	outBytes, err := EncodeFromUTF8(content, encInfo)
	if err != nil {
		return "", nil, fmt.Errorf("Unable to encode %s as %s: %v", rawPath, encInfo.Name, err)
	}

	absPath := ResolvePath(rawPath, workdir)
	_, statErr := os.Stat(absPath)
	existed := statErr == nil

	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return "", nil, fmt.Errorf("Unable to create parent directories for %s: %v", rawPath, err)
	}
	if err := os.WriteFile(absPath, outBytes, 0644); err != nil {
		return "", nil, fmt.Errorf("Unable to write %s: %v", rawPath, err)
	}

	verb := "Created"
	if existed {
		verb = "Wrote"
	}
	msg := fmt.Sprintf("%s file successfully: %s", verb, rawPath)
	if encInfo.Name != "utf-8" {
		msg = fmt.Sprintf("%s file successfully: %s (encoding: %s)", verb, rawPath, encInfo.Name)
	}
	return msg, map[string]any{
		"operation": "write",
		"target":    absPath,
		"resource":  rawPath,
		"existed":   existed,
		"encoding":  encInfo.Name,
	}, nil
}

func normalizeRuneForMatch(r rune) rune {
	switch r {
	case '‘', '’', '‚', '‛':
		return '\''
	case '“', '”', '„', '‟':
		return '"'
	case '‐', '‑', '‒', '–', '—', '―', '−':
		return '-'
	case '\u00A0', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200A', '\u202F', '\u205F', '\u3000':
		return ' '
	default:
		return r
	}
}

func normalizeForMatch(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		sb.WriteRune(normalizeRuneForMatch(r))
	}
	return sb.String()
}

// normalizeWithByteMap normalizes typographic quotes, dashes, and spaces in s,
// and returns both the normalized string and a slice mapping each byte index
// in the normalized string back to its original byte index in s (including sentinel at len).
func normalizeWithByteMap(s string) (string, []int) {
	var sb strings.Builder
	sb.Grow(len(s))
	normToOrig := make([]int, 0, len(s)+1)
	for origIdx, r := range s {
		nr := normalizeRuneForMatch(r)
		var buf [utf8.UTFMax]byte
		n := utf8.EncodeRune(buf[:], nr)
		for k := 0; k < n; k++ {
			normToOrig = append(normToOrig, origIdx)
		}
		sb.Write(buf[:n])
	}
	normToOrig = append(normToOrig, len(s))
	return sb.String(), normToOrig
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

// findUnicodeOccurrences performs Tier-2 Unicode-normalized matching while preserving exact UTF-8 byte offsets in view.
func findUnicodeOccurrences(view, search string) []byteSpan {
	normView, normToOrig := normalizeWithByteMap(view)
	normSearch := normalizeForMatch(search)
	rawMatches := findOccurrences(normView, normSearch)
	if len(rawMatches) == 0 {
		return nil
	}
	out := make([]byteSpan, 0, len(rawMatches))
	for _, m := range rawMatches {
		out = append(out, byteSpan{
			start: normToOrig[m.start],
			end:   normToOrig[m.end],
		})
	}
	return out
}

func trimRightWhitespace(s string) string {
	return strings.TrimRightFunc(s, unicode.IsSpace)
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
			cEnd := len(view)
			if strings.HasSuffix(txt, "\r") {
				cEnd--
			}
			lines = append(lines, lineInfo{
				start:      pos,
				end:        len(view),
				contentEnd: cEnd,
				text:       txt,
				hasNL:      false,
			})
			break
		}
		endPos := pos + nlIdx + 1
		txt := view[pos : pos+nlIdx]
		cEnd := pos + nlIdx
		if strings.HasSuffix(txt, "\r") {
			cEnd--
		}
		lines = append(lines, lineInfo{
			start:      pos,
			end:        endPos,
			contentEnd: cEnd,
			text:       txt,
			hasNL:      true,
		})
		pos = endPos
	}

	var matches []byteSpan
	for i := 0; i+len(expected) <= len(lines); i++ {
		ok := true
		for j := 0; j < len(expected); j++ {
			a := normalizeForMatch(trimRightWhitespace(lines[i+j].text))
			b := normalizeForMatch(trimRightWhitespace(expected[j]))
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

// lineEnding returns the dominant line ending in text, or fallback when tied/none (matching OpenCode's lineEnding).
func lineEnding(text, fallback string) string {
	crlfCount := strings.Count(text, "\r\n")
	lfCount := strings.Count(text, "\n") - crlfCount
	if crlfCount == lfCount {
		return fallback
	}
	if crlfCount > lfCount {
		return "\r\n"
	}
	return "\n"
}

func splitLinesPreserve(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			line = strings.TrimSuffix(line, "\r")
			out = append(out, line)
			start = i + 1
		}
	}
	last := s[start:]
	last = strings.TrimSuffix(last, "\r")
	out = append(out, last)
	return out
}

func extractLineEndings(s string) []string {
	var endings []string
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > 0 && s[i-1] == '\r' {
				endings = append(endings, "\r\n")
			} else {
				endings = append(endings, "\n")
			}
		}
	}
	return endings
}

// alignLines mirrors OpenCode's alignLines in edit.ts (LCS alignment between before and after lines).
func alignLines(before, after []string) []*int {
	width := len(after) + 1
	table := make([]int, (len(before)+1)*width)
	for i := len(before) - 1; i >= 0; i-- {
		for j := len(after) - 1; j >= 0; j-- {
			if before[i] == after[j] {
				table[i*width+j] = table[(i+1)*width+j+1] + 1
			} else {
				a := table[(i+1)*width+j]
				b := table[i*width+j+1]
				if a >= b {
					table[i*width+j] = a
				} else {
					table[i*width+j] = b
				}
			}
		}
	}

	result := make([]*int, len(after))
	var gapBefore []int
	var gapAfter []int
	flush := func() {
		for k, j := range gapAfter {
			if k < len(gapBefore) {
				v := gapBefore[k]
				result[j] = &v
			}
		}
		gapBefore = gapBefore[:0]
		gapAfter = gapAfter[:0]
	}

	i, j := 0, 0
	for i < len(before) && j < len(after) {
		if before[i] == after[j] {
			flush()
			v := i
			result[j] = &v
			i++
			j++
			continue
		}
		if table[(i+1)*width+j] >= table[i*width+j+1] {
			gapBefore = append(gapBefore, i)
			i++
		} else {
			gapAfter = append(gapAfter, j)
			j++
		}
	}
	for i < len(before) {
		gapBefore = append(gapBefore, i)
		i++
	}
	for j < len(after) {
		gapAfter = append(gapAfter, j)
		j++
	}
	flush()
	return result
}

// withRegionEndings mirrors OpenCode's withRegionEndings in edit.ts.
func withRegionEndings(region, replacement, fallback string) string {
	before := splitLinesPreserve(region)
	endings := extractLineEndings(region)
	after := strings.Split(replacement, "\n")

	beforeKeys := make([]string, len(before))
	for i, l := range before {
		beforeKeys[i] = normalizeForMatch(trimRightWhitespace(l))
	}
	afterKeys := make([]string, len(after))
	for i, l := range after {
		afterKeys[i] = normalizeForMatch(trimRightWhitespace(l))
	}
	source := alignLines(beforeKeys, afterKeys)

	var sb strings.Builder
	for idx, line := range after {
		sb.WriteString(line)
		if idx < len(after)-1 {
			ending := fallback
			if srcIdx := source[idx]; srcIdx != nil && *srcIdx >= 0 && *srcIdx < len(endings) {
				ending = endings[*srcIdx]
			}
			sb.WriteString(ending)
		}
	}
	return sb.String()
}

func countBefore(positions []int, offset int) int {
	low := 0
	high := len(positions)
	for low < high {
		mid := (low + high) >> 1
		if positions[mid] < offset {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return low
}

// matchView mirrors OpenCode's matchView in edit.ts:
// matches against the LF view, then maps offsets back to the original source bytes.
func matchView(source, oldString, newString string) ([]byteSpan, func(byteSpan) string) {
	view := strings.ReplaceAll(source, "\r\n", "\n")
	var removed []int
	idx := 0
	for i := 0; i+1 < len(source); i++ {
		if source[i] == '\r' && source[i+1] == '\n' {
			removed = append(removed, i-idx)
			idx++
		}
	}

	fileEnding := lineEnding(source, "\n")
	search := strings.ReplaceAll(oldString, "\r\n", "\n")
	replacement := strings.ReplaceAll(newString, "\r\n", "\n")

	matches := findOccurrences(view, search)
	if len(matches) == 0 {
		matches = findUnicodeOccurrences(view, search)
	}
	if len(matches) == 0 {
		matches = findLineTrimmedOccurrences(view, search)
	}

	mapped := make([]byteSpan, len(matches))
	for i, m := range matches {
		mapped[i] = byteSpan{
			start: m.start + countBefore(removed, m.start),
			end:   m.end + countBefore(removed, m.end),
		}
	}

	replaceFn := func(m byteSpan) string {
		region := source[m.start:m.end]
		return withRegionEndings(region, replacement, lineEnding(region, fileEnding))
	}
	return mapped, replaceFn
}

func EditFile(rawPath, oldString, newString string, replaceAll bool, workdir string) (string, map[string]any, error) {
	return EditFileWithEncoding(rawPath, oldString, newString, replaceAll, workdir, "")
}

func EditFileWithEncoding(rawPath, oldString, newString string, replaceAll bool, workdir, encodingSpec string) (string, map[string]any, error) {
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

	var encInfo EncodingInfo
	if strings.TrimSpace(encodingSpec) == "" || strings.EqualFold(strings.TrimSpace(encodingSpec), "auto") {
		encInfo, err = DetectTextEncoding(rawBytes, true)
		if err != nil {
			if errors.Is(err, errBinaryDetected) {
				return "", nil, fmt.Errorf("Cannot edit binary file: %s", rawPath)
			}
			return "", nil, fmt.Errorf("Unable to detect encoding for %s: %v", rawPath, err)
		}
	} else {
		encInfo, err = ResolveExplicitReadEncoding(encodingSpec, rawBytes)
		if err != nil {
			return "", nil, err
		}
	}

	source, err := DecodeToUTF8(rawBytes, encInfo)
	if err != nil {
		return "", nil, fmt.Errorf("Unable to decode %s (%s): %v", rawPath, encInfo.Name, err)
	}

	endingsOnly := strings.ReplaceAll(oldString, "\r", "") == strings.ReplaceAll(newString, "\r", "")
	var matches []byteSpan
	var replaceFn func(byteSpan) string

	if endingsOnly {
		matches = findOccurrences(source, oldString)
		replaceFn = func(_ byteSpan) string { return newString }
	} else {
		matches, replaceFn = matchView(source, oldString, newString)
	}

	replacements := len(matches)
	if replacements == 0 {
		hint := buildMissHint(source, oldString, newString)
		if hint != "" {
			return "", nil, fmt.Errorf("Could not find oldString in %s. It must match exactly, including whitespace and indentation.\n\n%s", rawPath, hint)
		}
		return "", nil, fmt.Errorf("Could not find oldString in %s. It must match exactly, including whitespace and indentation.", rawPath)
	}

	if replacements > 1 && !replaceAll {
		return "", nil, fmt.Errorf("Found %d matches for oldString, but expected exactly one. Add more surrounding context to make oldString unique, or set replaceAll to true to replace every occurrence.", replacements)
	}

	toApply := matches
	if !replaceAll {
		toApply = matches[:1]
	}

	var buf strings.Builder
	lastPos := 0
	for _, m := range toApply {
		buf.WriteString(source[lastPos:m.start])
		buf.WriteString(replaceFn(m))
		lastPos = m.end
	}
	buf.WriteString(source[lastPos:])
	replaced := buf.String()

	if replaced == source {
		if endingsOnly {
			return "", nil, fmt.Errorf("No changes to apply: %s already has those line endings.", rawPath)
		}
		return "", nil, fmt.Errorf("No changes to apply: the edit leaves %s unchanged. Line endings follow the file; to change them, write \\r explicitly in oldString or newString.", rawPath)
	}

	outBytes, err := EncodeFromUTF8(replaced, encInfo)
	if err != nil {
		return "", nil, fmt.Errorf("Unable to save %s in original encoding (%s): %v", rawPath, encInfo.Name, err)
	}

	if err := os.WriteFile(absPath, outBytes, st.Mode().Perm()); err != nil {
		return "", nil, fmt.Errorf("Unable to write edited file %s: %v", rawPath, err)
	}

	count := len(toApply)
	plural := "s"
	if count == 1 {
		plural = ""
	}
	msg := fmt.Sprintf("Edited %s (%d replacement%s)", rawPath, count, plural)
	if encInfo.Name != "utf-8" {
		msg = fmt.Sprintf("Edited %s (%d replacement%s, encoding: %s)", rawPath, count, plural, encInfo.Name)
	}
	return msg, map[string]any{
		"replacements": count,
		"encoding":     encInfo.Name,
	}, nil
}

func collapseWhitespaceKey(line string) string {
	norm := normalizeForMatch(line)
	return strings.Join(strings.Fields(norm), " ")
}

func isDistinctLine(line string) bool {
	for _, r := range line {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}

// buildMissHint mirrors OpenCode's missHint in edit.ts:
// lines vote for where oldString starts when they match ignoring all whitespace; at least half must agree.
func buildMissHint(source, oldString, newString string) string {
	view := strings.ReplaceAll(source, "\r\n", "\n")
	replacement := strings.ReplaceAll(newString, "\r\n", "\n")
	if len(strings.TrimSpace(replacement)) >= 20 && strings.Contains(normalizeForMatch(view), normalizeForMatch(replacement)) {
		return "newString is already in the file, so this edit may already be applied."
	}

	lines := strings.Split(view, "\n")
	search := strings.Split(strings.ReplaceAll(oldString, "\r\n", "\n"), "\n")

	positions := make(map[string][]int)
	for index, line := range lines {
		val := collapseWhitespaceKey(line)
		if !isDistinctLine(val) {
			continue
		}
		positions[val] = append(positions[val], index)
	}

	votes := make(map[int]int)
	var voteOrder []int
	wanted := 0
	for offset, line := range search {
		val := collapseWhitespaceKey(line)
		if !isDistinctLine(val) {
			continue
		}
		wanted++
		for _, idx := range positions[val] {
			candStart := idx - offset
			if _, exists := votes[candStart]; !exists {
				voteOrder = append(voteOrder, candStart)
			}
			votes[candStart]++
		}
	}

	bestStart := 0
	bestCount := 0
	for _, st := range voteOrder {
		if c := votes[st]; c > bestCount {
			bestStart = st
			bestCount = c
		}
	}
	if bestCount == 0 || bestCount*2 < wanted {
		return ""
	}

	var matched []int
	for offset, line := range search {
		val := collapseWhitespaceKey(line)
		idx := bestStart + offset
		if val != "" && idx >= 0 && idx < len(lines) && collapseWhitespaceKey(lines[idx]) == val {
			matched = append(matched, idx)
		}
	}
	if len(matched) == 0 {
		return ""
	}

	from := matched[0]
	to := matched[len(matched)-1] + 1
	if to > from+HintLines {
		to = from + HintLines
	}
	if to > len(lines) {
		to = len(lines)
	}

	out := []string{"The closest lines in the file:"}
	for k := from; k < to; k++ {
		line := lines[k]
		if utf8.RuneCountInString(line) > HintLineLength {
			runes := []rune(line)
			line = string(runes[:HintLineLength]) + "…"
		}
		out = append(out, fmt.Sprintf("%d: %s", k+1, line))
	}
	return strings.Join(out, "\n")
}

func GlobFiles(workdir, pattern, searchPath string, hidden bool, limit int) (string, map[string]any, error) {
	if strings.TrimSpace(pattern) == "" {
		return "", nil, fmt.Errorf("pattern must not be empty")
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	targetDir := workdir
	displayDir := "."
	if strings.TrimSpace(searchPath) != "" && searchPath != "undefined" && searchPath != "null" {
		targetDir = ResolvePath(searchPath, workdir)
		displayDir = searchPath
	}
	st, err := os.Stat(targetDir)
	if err != nil {
		return "", nil, fmt.Errorf("Search path does not exist: %s", displayDir)
	}
	if !st.IsDir() {
		return "", nil, fmt.Errorf("Search path is not a directory: %s", displayDir)
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
		if name == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		if !hidden {
			if strings.HasPrefix(name, ".") || (d.IsDir() && name == "node_modules") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() {
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
	return GrepFilesWithEncoding(workdir, pattern, searchPath, include, literal, caseSensitive, limit, "")
}

func GrepFilesWithEncoding(workdir, pattern, searchPath, include string, literal, caseSensitive bool, limit int, encodingSpec string) (string, map[string]any, error) {
	if pattern == "" {
		return "", nil, fmt.Errorf("Pattern must not be empty")
	}
	explicitEnc := strings.TrimSpace(encodingSpec) != "" && !strings.EqualFold(strings.TrimSpace(encodingSpec), "auto")
	if explicitEnc {
		if _, err := LookupEncoding(encodingSpec); err != nil {
			return "", nil, err
		}
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
	displayTarget := "."
	if strings.TrimSpace(searchPath) != "" && searchPath != "undefined" && searchPath != "null" {
		target = ResolvePath(searchPath, workdir)
		displayTarget = searchPath
	}
	st, err := os.Stat(target)
	if err != nil {
		return "", nil, fmt.Errorf("Search path does not exist: %s", displayTarget)
	}

	type grepMatch struct {
		file string
		line int
		text string
	}
	var matches []grepMatch
	truncated := false

	// Stream-scan each file line by line (avoiding os.ReadFile on huge files)
	searchSingleFile := func(filePath string) {
		f, err := os.Open(filePath)
		if err != nil {
			return
		}
		defer f.Close()

		br := bufio.NewReaderSize(f, 32*1024)
		peekBytes, peekErr := br.Peek(8192)
		if len(peekBytes) == 0 {
			return
		}

		var encInfo EncodingInfo
		if explicitEnc {
			encInfo, err = ResolveExplicitReadEncoding(encodingSpec, peekBytes)
			if err != nil {
				return
			}
		} else {
			encInfo, err = DetectTextEncoding(peekBytes, peekErr == io.EOF)
			if err != nil {
				return
			}
		}

		if encInfo.HasBOM && len(encInfo.BOM) > 0 && bytes.HasPrefix(peekBytes, encInfo.BOM) {
			_, _ = br.Discard(len(encInfo.BOM))
		}

		lineBr := bufio.NewReaderSize(NewUTF8DecodeReader(br, encInfo), 32*1024)

		lineNum := 0
		for {
			rawLine, readErr := lineBr.ReadString('\n')
			if len(rawLine) > 0 {
				lineNum++
				line := strings.TrimSuffix(rawLine, "\n")
				line = strings.TrimSuffix(line, "\r")
				if re.MatchString(line) {
					if len(matches) >= limit {
						truncated = true
						return
					}
					matches = append(matches, grepMatch{
						file: filePath,
						line: lineNum,
						text: clampLine(line),
					})
				}
			}
			if readErr != nil {
				break
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
			if d.IsDir() {
				// Like OpenCode's ripgrep (--hidden --glob=!**/.git/**), skip .git and node_modules,
				// but allow searching dotfiles/dot-directories such as .github or .env.
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
	writeGlobSegment(&sb, p, true)
	sb.WriteString("$")
	return regexp.Compile(sb.String())
}

func writeGlobSegment(sb *strings.Builder, p string, allowBrace bool) {
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
			if allowBrace {
				end := strings.IndexByte(p[i:], '}')
				if end != -1 {
					inner := p[i+1 : i+end]
					parts := strings.Split(inner, ",")
					sb.WriteString("(?:")
					for idx, part := range parts {
						if idx > 0 {
							sb.WriteByte('|')
						}
						writeGlobSegment(sb, part, false)
					}
					sb.WriteString(")")
					i += end + 1
					continue
				}
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
}
