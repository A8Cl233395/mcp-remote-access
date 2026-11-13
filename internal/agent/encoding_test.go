package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

func encodeGBK(t *testing.T, s string) []byte {
	t.Helper()
	b, _, err := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte(s))
	if err != nil {
		t.Fatalf("encodeGBK(%q): %v", s, err)
	}
	return b
}

func encodeUTF16LEBOM(t *testing.T, s string) []byte {
	t.Helper()
	enc := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM)
	b, _, err := transform.Bytes(enc.NewEncoder(), []byte(s))
	if err != nil {
		t.Fatalf("encodeUTF16LEBOM: %v", err)
	}
	return b
}

func encodeUTF16BEBOM(t *testing.T, s string) []byte {
	t.Helper()
	enc := unicode.UTF16(unicode.BigEndian, unicode.UseBOM)
	b, _, err := transform.Bytes(enc.NewEncoder(), []byte(s))
	if err != nil {
		t.Fatalf("encodeUTF16BEBOM: %v", err)
	}
	return b
}

func TestReadWriteEditGrepEncodings(t *testing.T) {
	dir := t.TempDir()

	// 1. Read GBK file (auto-detect)
	gbkText := "第一行：中文测试\r\n第二行：目录与文件\r\n"
	gbkPath := filepath.Join(dir, "gbk.txt")
	if err := os.WriteFile(gbkPath, encodeGBK(t, gbkText), 0644); err != nil {
		t.Fatal(err)
	}
	out, meta, err := ReadFileOrDir("gbk.txt", dir, nil, nil)
	if err != nil {
		t.Fatalf("ReadFileOrDir gbk failed: %v", err)
	}
	if meta["encoding"] != "gbk" {
		t.Fatalf("expected encoding=gbk, got %v", meta["encoding"])
	}
	if !strings.Contains(out, "(encoding: gbk)") || !strings.Contains(out, "1: 第一行：中文测试") || !strings.Contains(out, "2: 第二行：目录与文件") {
		t.Fatalf("unexpected gbk read output:\n%s", out)
	}

	// 2. Edit GBK file -> preserves GBK encoding and CRLF
	editOut, editMeta, err := EditFile("gbk.txt", "第二行：目录与文件", "第二行：已修改配置", false, dir)
	if err != nil {
		t.Fatalf("EditFile gbk failed: %v", err)
	}
	if editMeta["encoding"] != "gbk" || !strings.Contains(editOut, "encoding: gbk") {
		t.Fatalf("unexpected edit meta/out: %v / %s", editMeta, editOut)
	}
	rawEdited, _ := os.ReadFile(gbkPath)
	expectedEdited := encodeGBK(t, "第一行：中文测试\r\n第二行：已修改配置\r\n")
	if !bytes.Equal(rawEdited, expectedEdited) {
		t.Fatalf("edited GBK bytes mismatch: got %x, want %x", rawEdited, expectedEdited)
	}

	// Edit GBK with unrepresentable rune (emoji) should error cleanly without modifying file
	if _, _, err := EditFile("gbk.txt", "已修改配置", "🚀", false, dir); err == nil {
		t.Fatalf("expected error encoding emoji into GBK")
	}
	rawAfterFail, _ := os.ReadFile(gbkPath)
	if !bytes.Equal(rawAfterFail, expectedEdited) {
		t.Fatalf("file was modified despite encoding error")
	}

	// 3. UTF-16LE with BOM (must not trigger binary NUL rejection)
	u16Text := "Windows Registry Editor\r\n中文键值=测试\r\n"
	u16Path := filepath.Join(dir, "config.reg")
	if err := os.WriteFile(u16Path, encodeUTF16LEBOM(t, u16Text), 0644); err != nil {
		t.Fatal(err)
	}
	u16Out, u16Meta, err := ReadFileOrDir("config.reg", dir, nil, nil)
	if err != nil {
		t.Fatalf("ReadFileOrDir utf-16le failed: %v", err)
	}
	if u16Meta["encoding"] != "utf-16le" || !strings.Contains(u16Out, "2: 中文键值=测试") {
		t.Fatalf("unexpected utf-16le read output (%v):\n%s", u16Meta, u16Out)
	}
	if _, _, err := EditFile("config.reg", "中文键值=测试", "中文键值=更新成功", false, dir); err != nil {
		t.Fatalf("EditFile utf-16le failed: %v", err)
	}
	rawU16Edited, _ := os.ReadFile(u16Path)
	if !bytes.Equal(rawU16Edited, encodeUTF16LEBOM(t, "Windows Registry Editor\r\n中文键值=更新成功\r\n")) {
		t.Fatalf("edited utf-16le bytes mismatch")
	}

	// 4. UTF-16BE with BOM
	u16BEPath := filepath.Join(dir, "be.txt")
	if err := os.WriteFile(u16BEPath, encodeUTF16BEBOM(t, "首行\n次行\n"), 0644); err != nil {
		t.Fatal(err)
	}
	beOut, beMeta, err := ReadFileOrDir("be.txt", dir, nil, nil)
	if err != nil || beMeta["encoding"] != "utf-16be" || !strings.Contains(beOut, "1: 首行") {
		t.Fatalf("unexpected utf-16be read: err=%v meta=%v out=%s", err, beMeta, beOut)
	}

	// 5. UTF-8-BOM: no \uFEFF leaked on line 1, BOM preserved on edit
	u8BomPath := filepath.Join(dir, "bom.txt")
	if err := os.WriteFile(u8BomPath, append([]byte{0xEF, 0xBB, 0xBF}, []byte("hello\n世界\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	bomOut, bomMeta, err := ReadFileOrDir("bom.txt", dir, nil, nil)
	if err != nil || bomMeta["encoding"] != "utf-8-bom" || !strings.Contains(bomOut, "1: hello") || strings.ContainsRune(bomOut, '\uFEFF') {
		t.Fatalf("unexpected utf-8-bom read: err=%v meta=%v out=%q", err, bomMeta, bomOut)
	}
	if _, _, err := EditFile("bom.txt", "世界", "OpenCode", false, dir); err != nil {
		t.Fatalf("EditFile utf-8-bom failed: %v", err)
	}
	rawBomEdited, _ := os.ReadFile(u8BomPath)
	if !bytes.Equal(rawBomEdited, append([]byte{0xEF, 0xBB, 0xBF}, []byte("hello\nOpenCode\n")...)) {
		t.Fatalf("utf-8-bom not preserved: %x", rawBomEdited)
	}

	// 6. Write: defaults to utf-8 (even when overwriting existing GBK), rejects "auto", supports explicit gbk
	if _, _, err := WriteFileWithEncoding("gbk.txt", "覆盖为UTF8中文", dir, ""); err != nil {
		t.Fatalf("WriteFile default utf-8 failed: %v", err)
	}
	rawOverwritten, _ := os.ReadFile(gbkPath)
	if string(rawOverwritten) != "覆盖为UTF8中文" {
		t.Fatalf("expected utf-8 overwrite, got %x", rawOverwritten)
	}
	if _, _, err := WriteFileWithEncoding("x.txt", "abc", dir, "auto"); err == nil {
		t.Fatalf("expected error when passing auto to write")
	}
	if _, _, err := WriteFileWithEncoding("explicit_gbk.txt", "显式GBK写入", dir, "gbk"); err != nil {
		t.Fatalf("WriteFileWithEncoding gbk failed: %v", err)
	}
	rawExplicitGBK, _ := os.ReadFile(filepath.Join(dir, "explicit_gbk.txt"))
	if !bytes.Equal(rawExplicitGBK, encodeGBK(t, "显式GBK写入")) {
		t.Fatalf("explicit GBK write mismatch: %x", rawExplicitGBK)
	}

	// 7. Grep across UTF-8, GBK, and UTF-16LE files
	grepOut, _, err := GrepFiles(dir, "中文", "", "", true, true, 20)
	if err != nil {
		t.Fatalf("GrepFiles failed: %v", err)
	}
	if !strings.Contains(grepOut, "gbk.txt") || !strings.Contains(grepOut, "config.reg") {
		t.Fatalf("expected grep to match UTF-8 and UTF-16LE files, got:\n%s", grepOut)
	}
	grepGBKOut, _, err := GrepFiles(dir, "显式GBK写入", "", "", true, true, 20)
	if err != nil || !strings.Contains(grepGBKOut, "explicit_gbk.txt") {
		t.Fatalf("expected grep to match GBK file, got err=%v out=%s", err, grepGBKOut)
	}

	// 8. Binary file still rejected
	binPath := filepath.Join(dir, "bin.dat")
	_ = os.WriteFile(binPath, []byte{0x4D, 0x5A, 0x00, 0x90}, 0644)
	if _, _, err := ReadFileOrDir("bin.dat", dir, nil, nil); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("expected binary file error, got %v", err)
	}
}

func TestReadLargeNonUTF8Streaming(t *testing.T) {
	dir := t.TempDir()
	// Build a > FirstReadChunk (256KB) GBK file so streaming chunk decode is exercised across boundaries
	var sb strings.Builder
	for i := 1; i <= 12000; i++ {
		sb.WriteString("测试中文行内容-第")
		sb.WriteString(strings.Repeat("甲", 10))
		sb.WriteByte('\n')
	}
	gbkBytes := encodeGBK(t, sb.String())
	if len(gbkBytes) <= FirstReadChunk {
		t.Fatalf("test file not large enough: %d <= %d", len(gbkBytes), FirstReadChunk)
	}
	largeGBK := filepath.Join(dir, "large_gbk.txt")
	if err := os.WriteFile(largeGBK, gbkBytes, 0644); err != nil {
		t.Fatal(err)
	}

	offset := 11995
	limit := 5
	out, meta, err := ReadFileOrDir("large_gbk.txt", dir, &offset, &limit)
	if err != nil {
		t.Fatalf("ReadFileOrDir large GBK failed: %v", err)
	}
	if meta["encoding"] != "gbk" {
		t.Fatalf("expected gbk, got %v", meta["encoding"])
	}
	if !strings.Contains(out, "11995: 测试中文行内容-第甲甲甲甲甲甲甲甲甲甲") || !strings.Contains(out, "11999: 测试中文行内容-第甲甲甲甲甲甲甲甲甲甲") {
		t.Fatalf("unexpected large GBK page output:\n%s", out)
	}
}

func TestShellStreamWriterDecoding(t *testing.T) {
	rec := &JobRecord{}
	w := newShellStreamWriter(rec, "")
	// Force CJK console mode (GBK/CP936) in test regardless of host OS locale
	info, _ := LookupEncoding("gbk")
	w.fallback = info
	w.decoder = simplifiedchinese.GB18030
	w.autoDetect = true

	// 1. Split a 2-byte GBK line across two Write calls
	gbkLine := encodeGBK(t, "正在测试命令输出\r\n")
	mid := 3 // splits inside the second 2-byte GBK character
	_, _ = w.Write(gbkLine[:mid])
	_, _ = w.Write(gbkLine[mid:])

	// 2. Ambiguous short GBK word "目录" (0xC4 0xBF 0xC2 0xBC) which is accidentally valid 2-byte UTF-8!
	_, _ = w.Write(encodeGBK(t, "目录\r\n"))

	// 3. Genuine 3-byte UTF-8 line on the same stream (e.g. git/pwsh output)
	_, _ = w.Write([]byte("来自UTF8工具的中文输出\n"))

	// 4. Partial prompt without trailing newline, flushed on Close()
	_, _ = w.Write(encodeGBK(t, "请输入继续:"))
	w.Close()

	// Ensure timer doesn't race
	time.Sleep(10 * time.Millisecond)

	rec.mu.Lock()
	got := rec.buf.String()
	rec.mu.Unlock()

	want := "正在测试命令输出\r\n目录\r\n来自UTF8工具的中文输出\n请输入继续:"
	if got != want {
		t.Fatalf("shellStreamWriter decoded mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}
