package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

var errBinaryDetected = errors.New("binary file detected")

// EncodingInfo describes a resolved or detected text encoding.
type EncodingInfo struct {
	Name      string
	HasBOM    bool
	BOM       []byte
	AllowsNul bool
	IsCJK     bool
	Enc       encoding.Encoding // nil for utf-8 / utf-8-bom
}

func LookupEncoding(spec string) (EncodingInfo, error) {
	norm := strings.ToLower(strings.TrimSpace(spec))
	norm = strings.ReplaceAll(norm, "_", "-")

	switch norm {
	case "", "utf-8", "utf8", "cp65001":
		return EncodingInfo{Name: "utf-8"}, nil
	case "utf-8-bom", "utf8-bom":
		return EncodingInfo{
			Name:   "utf-8-bom",
			HasBOM: true,
			BOM:    []byte{0xEF, 0xBB, 0xBF},
		}, nil
	case "utf-16le", "utf16le", "utf-16", "utf16", "cp1200":
		return EncodingInfo{
			Name:      "utf-16le",
			HasBOM:    true,
			BOM:       []byte{0xFF, 0xFE},
			AllowsNul: true,
			Enc:       unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM),
		}, nil
	case "utf-16le-nobom", "utf16le-nobom":
		return EncodingInfo{
			Name:      "utf-16le",
			HasBOM:    false,
			AllowsNul: true,
			Enc:       unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM),
		}, nil
	case "utf-16be", "utf16be", "cp1201":
		return EncodingInfo{
			Name:      "utf-16be",
			HasBOM:    true,
			BOM:       []byte{0xFE, 0xFF},
			AllowsNul: true,
			Enc:       unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM),
		}, nil
	case "utf-16be-nobom", "utf16be-nobom":
		return EncodingInfo{
			Name:      "utf-16be",
			HasBOM:    false,
			AllowsNul: true,
			Enc:       unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM),
		}, nil
	case "gbk", "cp936", "gb2312", "ms936":
		return EncodingInfo{
			Name:  "gbk",
			IsCJK: true,
			Enc:   simplifiedchinese.GBK,
		}, nil
	case "gb18030", "cp54936":
		return EncodingInfo{
			Name:  "gb18030",
			IsCJK: true,
			Enc:   simplifiedchinese.GB18030,
		}, nil
	case "big5", "cp950", "ms950":
		return EncodingInfo{
			Name:  "big5",
			IsCJK: true,
			Enc:   traditionalchinese.Big5,
		}, nil
	case "shift-jis", "shiftjis", "sjis", "cp932", "ms932":
		return EncodingInfo{
			Name:  "shift_jis",
			IsCJK: true,
			Enc:   japanese.ShiftJIS,
		}, nil
	case "euc-jp", "eucjp":
		return EncodingInfo{
			Name:  "euc-jp",
			IsCJK: true,
			Enc:   japanese.EUCJP,
		}, nil
	case "euc-kr", "euckr", "cp949", "ms949":
		return EncodingInfo{
			Name:  "euc-kr",
			IsCJK: true,
			Enc:   korean.EUCKR,
		}, nil
	case "windows-1252", "cp1252", "win1252":
		return EncodingInfo{
			Name: "windows-1252",
			Enc:  charmap.Windows1252,
		}, nil
	case "iso-8859-1", "iso8859-1", "latin1", "cp28591":
		return EncodingInfo{
			Name: "iso-8859-1",
			Enc:  charmap.ISO8859_1,
		}, nil
	case "windows-1251", "cp1251":
		return EncodingInfo{
			Name: "windows-1251",
			Enc:  charmap.Windows1251,
		}, nil
	case "windows-1250", "cp1250":
		return EncodingInfo{
			Name: "windows-1250",
			Enc:  charmap.Windows1250,
		}, nil
	case "windows-1253", "cp1253":
		return EncodingInfo{
			Name: "windows-1253",
			Enc:  charmap.Windows1253,
		}, nil
	case "windows-1254", "cp1254":
		return EncodingInfo{
			Name: "windows-1254",
			Enc:  charmap.Windows1254,
		}, nil
	case "windows-1255", "cp1255":
		return EncodingInfo{
			Name: "windows-1255",
			Enc:  charmap.Windows1255,
		}, nil
	case "windows-1256", "cp1256":
		return EncodingInfo{
			Name: "windows-1256",
			Enc:  charmap.Windows1256,
		}, nil
	case "windows-1257", "cp1257":
		return EncodingInfo{
			Name: "windows-1257",
			Enc:  charmap.Windows1257,
		}, nil
	case "windows-1258", "cp1258":
		return EncodingInfo{
			Name: "windows-1258",
			Enc:  charmap.Windows1258,
		}, nil
	case "cp437":
		return EncodingInfo{
			Name: "cp437",
			Enc:  charmap.CodePage437,
		}, nil
	case "cp850":
		return EncodingInfo{
			Name: "cp850",
			Enc:  charmap.CodePage850,
		}, nil
	case "cp866":
		return EncodingInfo{
			Name: "cp866",
			Enc:  charmap.CodePage866,
		}, nil
	case "koi8-r", "cp20866":
		return EncodingInfo{
			Name: "koi8-r",
			Enc:  charmap.KOI8R,
		}, nil
	case "koi8-u", "cp21866":
		return EncodingInfo{
			Name: "koi8-u",
			Enc:  charmap.KOI8U,
		}, nil
	case "ansi", "system":
		return LookupEncoding(SystemFileEncoding())
	case "oem", "console":
		return LookupEncoding(SystemConsoleEncoding())
	default:
		return EncodingInfo{}, fmt.Errorf("unsupported encoding %q", spec)
	}
}

// ResolveExplicitReadEncoding resolves an explicit encoding for read/edit/grep.
// If the file actually starts with a matching BOM, it sets HasBOM/BOM accordingly so edit preserves BOM state.
func ResolveExplicitReadEncoding(spec string, sample []byte) (EncodingInfo, error) {
	info, err := LookupEncoding(spec)
	if err != nil {
		return EncodingInfo{}, err
	}
	switch info.Name {
	case "utf-8":
		if bytes.HasPrefix(sample, []byte{0xEF, 0xBB, 0xBF}) {
			info.Name = "utf-8-bom"
			info.HasBOM = true
			info.BOM = []byte{0xEF, 0xBB, 0xBF}
		}
	case "utf-8-bom":
		if !bytes.HasPrefix(sample, []byte{0xEF, 0xBB, 0xBF}) {
			info.HasBOM = false
			info.BOM = nil
		}
	case "utf-16le":
		if !bytes.HasPrefix(sample, []byte{0xFF, 0xFE}) {
			info.HasBOM = false
			info.BOM = nil
		}
	case "utf-16be":
		if !bytes.HasPrefix(sample, []byte{0xFE, 0xFF}) {
			info.HasBOM = false
			info.BOM = nil
		}
	}
	return info, nil
}

func isValidUTF8Sample(sample []byte, isEOF bool) bool {
	if len(sample) == 0 {
		return true
	}
	if isEOF {
		return utf8.Valid(sample)
	}
	// If sample was cut at a fixed buffer boundary, trim up to 3 trailing incomplete UTF-8 bytes.
	for trim := 0; trim <= 3 && trim < len(sample); trim++ {
		sub := sample[:len(sample)-trim]
		if utf8.Valid(sub) {
			if trim == 0 {
				return true
			}
			// Only accept trimming if the tail actually looks like the start of a multi-byte UTF-8 rune
			tail := sample[len(sample)-trim:]
			if !utf8.FullRune(tail) && tail[0] >= 0xC2 {
				return true
			}
			return false
		}
	}
	return false
}

func canDecodeCleanly(sample []byte, enc EncodingInfo, isEOF bool) bool {
	if enc.Enc == nil {
		return isValidUTF8Sample(sample, isEOF)
	}
	maxTrim := 0
	if !isEOF {
		maxTrim = 3
	}
	for trim := 0; trim <= maxTrim && trim < len(sample); trim++ {
		sub := sample[:len(sample)-trim]
		dec, _, err := transform.Bytes(enc.Enc.NewDecoder(), sub)
		if err != nil {
			continue
		}
		if !bytes.ContainsRune(dec, utf8.RuneError) {
			return true
		}
	}
	return false
}

// DetectTextEncoding detects the encoding of sample (first chunk or full file).
func DetectTextEncoding(sample []byte, isEOF bool) (EncodingInfo, error) {
	// 1. Check BOM before NUL check so UTF-16LE/BE files are never rejected as binary.
	if bytes.HasPrefix(sample, []byte{0xEF, 0xBB, 0xBF}) {
		return LookupEncoding("utf-8-bom")
	}
	if bytes.HasPrefix(sample, []byte{0xFF, 0xFE}) {
		return LookupEncoding("utf-16le")
	}
	if bytes.HasPrefix(sample, []byte{0xFE, 0xFF}) {
		return LookupEncoding("utf-16be")
	}

	// 2. Reject binary files containing NUL bytes.
	if bytes.IndexByte(sample, 0) != -1 {
		return EncodingInfo{}, errBinaryDetected
	}

	// 3. Valid UTF-8 (including pure ASCII).
	if isValidUTF8Sample(sample, isEOF) {
		return EncodingInfo{Name: "utf-8"}, nil
	}

	// 4. Non-UTF-8 text: build ordered candidate list starting with OS file encoding.
	var candidates []string
	seen := make(map[string]bool)
	addCand := func(name string) {
		if name == "" || name == "utf-8" || seen[name] {
			return
		}
		seen[name] = true
		candidates = append(candidates, name)
	}

	sysEnc := SystemFileEncoding()
	addCand(sysEnc)
	if sysEnc == "gbk" {
		addCand("gb18030")
	}
	// Common multi-byte CJK encodings before single-byte encodings
	addCand("gbk")
	addCand("gb18030")
	addCand("big5")
	addCand("shift_jis")
	addCand("euc-kr")
	addCand("euc-jp")
	addCand("windows-1252")

	for _, name := range candidates {
		info, err := LookupEncoding(name)
		if err != nil || info.Enc == nil {
			continue
		}
		if canDecodeCleanly(sample, info, isEOF) {
			return info, nil
		}
	}

	// Last-resort fallback: prefer system non-UTF-8 encoding if configured, else windows-1252.
	if sysEnc != "" && sysEnc != "utf-8" {
		if info, err := LookupEncoding(sysEnc); err == nil {
			return info, nil
		}
	}
	return LookupEncoding("windows-1252")
}

// DecodeToUTF8 decodes raw file bytes (stripping BOM if enc.HasBOM) into a UTF-8 string.
func DecodeToUTF8(raw []byte, enc EncodingInfo) (string, error) {
	data := raw
	if enc.HasBOM && len(enc.BOM) > 0 && bytes.HasPrefix(data, enc.BOM) {
		data = data[len(enc.BOM):]
	}
	if enc.Enc == nil {
		return string(data), nil
	}
	out, _, err := transform.Bytes(enc.Enc.NewDecoder(), data)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// EncodeFromUTF8 encodes UTF-8 text into target encoding bytes (prepending BOM if enc.HasBOM).
// Returns an error if text contains characters unrepresentable in the target encoding.
func EncodeFromUTF8(text string, enc EncodingInfo) ([]byte, error) {
	var body []byte
	if enc.Enc == nil {
		body = []byte(text)
	} else {
		var err error
		body, _, err = transform.Bytes(enc.Enc.NewEncoder(), []byte(text))
		if err != nil {
			return nil, fmt.Errorf("cannot encode content as %s: %w", enc.Name, err)
		}
	}
	if enc.HasBOM && len(enc.BOM) > 0 {
		out := make([]byte, 0, len(enc.BOM)+len(body))
		out = append(out, enc.BOM...)
		out = append(out, body...)
		return out, nil
	}
	return body, nil
}

// nulRejectReader wraps an io.Reader and returns errBinaryDetected if any 0x00 byte is read.
type nulRejectReader struct {
	r io.Reader
}

func (n *nulRejectReader) Read(p []byte) (int, error) {
	k, err := n.r.Read(p)
	if k > 0 && bytes.IndexByte(p[:k], 0) != -1 {
		return 0, errBinaryDetected
	}
	return k, err
}

// NewUTF8DecodeReader wraps r (already positioned past any BOM) to stream-decode enc into UTF-8.
func NewUTF8DecodeReader(r io.Reader, enc EncodingInfo) io.Reader {
	src := r
	if !enc.AllowsNul {
		src = &nulRejectReader{r: r}
	}
	if enc.Enc == nil {
		return src
	}
	return transform.NewReader(src, enc.Enc.NewDecoder())
}

// shellStreamWriter decodes child process stdout/stderr bytes into valid UTF-8 before writing to JobRecord.
type shellStreamWriter struct {
	mu         sync.Mutex
	rec        *JobRecord
	fallback   EncodingInfo
	decoder    encoding.Encoding
	autoDetect bool
	pending    []byte
	flushTimer *time.Timer
	closed     bool
}

func ValidateShellEncoding(spec string) error {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" || strings.EqualFold(trimmed, "auto") {
		return nil
	}
	_, err := LookupEncoding(trimmed)
	return err
}

func newShellStreamWriter(rec *JobRecord, spec string) *shellStreamWriter {
	autoDetect := strings.TrimSpace(spec) == "" || strings.EqualFold(strings.TrimSpace(spec), "auto")
	encName := spec
	if autoDetect {
		encName = SystemConsoleEncoding()
	}
	info, err := LookupEncoding(encName)
	if err != nil {
		info = EncodingInfo{Name: "utf-8"}
	}
	dec := info.Enc
	if info.Name == "gbk" {
		// CP936 console output is safely decoded with GB18030 (strict superset of GBK/CP936).
		dec = simplifiedchinese.GB18030
	}
	return &shellStreamWriter{
		rec:        rec,
		fallback:   info,
		decoder:    dec,
		autoDetect: autoDetect,
	}
}

func (w *shellStreamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	w.mu.Lock()
	if w.flushTimer != nil {
		w.flushTimer.Stop()
		w.flushTimer = nil
	}
	w.pending = append(w.pending, p...)

	var ready [][]byte
	for {
		idx := bytes.IndexAny(w.pending, "\n\r")
		if idx == -1 {
			break
		}
		seg := make([]byte, idx+1)
		copy(seg, w.pending[:idx+1])
		w.pending = w.pending[idx+1:]
		ready = append(ready, w.decodeSegment(seg))
	}

	if len(w.pending) > 0 {
		if isPureASCII(w.pending) {
			seg := make([]byte, len(w.pending))
			copy(seg, w.pending)
			w.pending = w.pending[:0]
			ready = append(ready, seg)
		} else if len(w.pending) >= 64*1024 {
			// Cap unbounded buffering for very long lines without newlines.
			cut := len(w.pending)
			if cut > 4 {
				// Avoid splitting up to 3 trailing bytes of a multi-byte char.
				cut -= 3
			}
			seg := make([]byte, cut)
			copy(seg, w.pending[:cut])
			w.pending = w.pending[cut:]
			ready = append(ready, w.decodeSegment(seg))
			if len(w.pending) > 0 && !w.closed {
				w.flushTimer = time.AfterFunc(50*time.Millisecond, w.Flush)
			}
		} else if !w.closed {
			// Delay flushing a partial non-ASCII line slightly so multi-byte characters split across pipe reads stay intact,
			// while interactive prompts without a trailing newline still show up promptly.
			w.flushTimer = time.AfterFunc(50*time.Millisecond, w.Flush)
		}
	}
	w.mu.Unlock()

	for _, out := range ready {
		if len(out) > 0 {
			_, _ = w.rec.writeOutput(out)
		}
	}
	return len(p), nil
}

func (w *shellStreamWriter) Flush() {
	w.mu.Lock()
	if w.flushTimer != nil {
		w.flushTimer.Stop()
		w.flushTimer = nil
	}
	if len(w.pending) == 0 {
		w.mu.Unlock()
		return
	}
	seg := make([]byte, len(w.pending))
	copy(seg, w.pending)
	w.pending = w.pending[:0]
	out := w.decodeSegment(seg)
	w.mu.Unlock()

	if len(out) > 0 {
		_, _ = w.rec.writeOutput(out)
	}
}

func (w *shellStreamWriter) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.Flush()
}

func isPureASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

func (w *shellStreamWriter) decodeSegment(seg []byte) []byte {
	if len(seg) == 0 || isPureASCII(seg) {
		return seg
	}
	if !w.autoDetect {
		if w.decoder == nil {
			return seg
		}
		out, _, err := transform.Bytes(w.decoder.NewDecoder(), seg)
		if err != nil {
			return seg
		}
		return out
	}

	// Auto-detect per segment/line:
	if w.decoder == nil {
		// System console encoding is UTF-8.
		if utf8.Valid(seg) {
			return seg
		}
		// Fallback for legacy GBK output even on a UTF-8 locale if cleanly decodable.
		if out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), seg); err == nil && !bytes.ContainsRune(out, utf8.RuneError) {
			return out
		}
		return seg
	}

	if utf8.Valid(seg) {
		if !w.fallback.IsCJK {
			return seg
		}
		// Disambiguate short CJK GBK words (like "目录" = C4 BF C2 BC) that collide with 2-byte UTF-8.
		// Genuine UTF-8 CJK/symbol output uses >=3-byte runes (r >= 0x0800).
		has3ByteOrMore := false
		for i := 0; i < len(seg); {
			r, sz := utf8.DecodeRune(seg[i:])
			if r >= 0x0800 {
				has3ByteOrMore = true
				break
			}
			i += sz
		}
		if has3ByteOrMore {
			return seg
		}
		if out, _, err := transform.Bytes(w.decoder.NewDecoder(), seg); err == nil && !bytes.ContainsRune(out, utf8.RuneError) {
			for i := 0; i < len(out); {
				r, sz := utf8.DecodeRune(out[i:])
				if r >= 0x2E80 {
					return out
				}
				i += sz
			}
		}
		return seg
	}

	out, _, err := transform.Bytes(w.decoder.NewDecoder(), seg)
	if err != nil {
		return seg
	}
	return out
}
