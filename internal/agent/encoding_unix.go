//go:build !windows

package agent

import (
	"os"
	"strings"
)

func parseUnixLocaleEncoding() string {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		val := strings.TrimSpace(os.Getenv(key))
		if val == "" {
			continue
		}
		upper := strings.ToUpper(val)
		switch {
		case strings.Contains(upper, "UTF-8") || strings.Contains(upper, "UTF8"):
			return "utf-8"
		case strings.Contains(upper, "GB18030"):
			return "gb18030"
		case strings.Contains(upper, "GBK") || strings.Contains(upper, "GB2312") || strings.Contains(upper, "EUC-CN"):
			return "gbk"
		case strings.Contains(upper, "BIG5"):
			return "big5"
		case strings.Contains(upper, "SJIS") || strings.Contains(upper, "SHIFT_JIS") || strings.Contains(upper, "SHIFT-JIS"):
			return "shift_jis"
		case strings.Contains(upper, "EUC-JP") || strings.Contains(upper, "EUCJP"):
			return "euc-jp"
		case strings.Contains(upper, "EUC-KR") || strings.Contains(upper, "EUCKR"):
			return "euc-kr"
		case strings.Contains(upper, "ISO-8859-1") || strings.Contains(upper, "ISO8859-1") || strings.Contains(upper, "LATIN1"):
			return "iso-8859-1"
		case strings.Contains(upper, "KOI8-R"):
			return "koi8-r"
		case strings.Contains(upper, "KOI8-U"):
			return "koi8-u"
		}
	}
	return "utf-8"
}

func SystemFileEncoding() string {
	return parseUnixLocaleEncoding()
}

func SystemConsoleEncoding() string {
	return parseUnixLocaleEncoding()
}
