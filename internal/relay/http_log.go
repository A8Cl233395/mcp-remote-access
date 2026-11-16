package relay

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type loggingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int64
	hijacked    bool
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *loggingResponseWriter) Flush() {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		w.hijacked = true
		if !w.wroteHeader {
			w.status = http.StatusSwitchingProtocols
			w.wroteHeader = true
		}
	}
	return conn, rw, err
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (s *Server) withHTTPLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &loggingResponseWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
		}
		defer func() {
			if s.cfg.Logger == nil || rw.hijacked {
				return
			}
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			target := sanitizeRequestTarget(r)
			method := sanitizeLogValue(r.Method)
			remote := sanitizeLogValue(r.RemoteAddr)
			xff := sanitizeLogValue(r.Header.Get("X-Forwarded-For"))
			dur := time.Since(start).Round(time.Microsecond)
			if xff != "" {
				s.cfg.Logger.Printf("http: %s %s scheme=%s status=%d bytes=%d duration=%s remote=%s xff=%s",
					method, target, scheme, rw.status, rw.bytes, dur, remote, xff)
				return
			}
			s.cfg.Logger.Printf("http: %s %s scheme=%s status=%d bytes=%d duration=%s remote=%s",
				method, target, scheme, rw.status, rw.bytes, dur, remote)
		}()
		next.ServeHTTP(rw, r)
	})
}

func sanitizeRequestTarget(r *http.Request) string {
	path := sanitizeLogValue(r.URL.Path)
	if r.URL.RawQuery == "" {
		return path
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return path + "?<invalid-query>"
	}
	for k, vals := range q {
		if isSensitiveQueryParam(k) {
			for i := range vals {
				vals[i] = "REDACTED"
			}
		}
	}
	return path + "?" + q.Encode()
}

func isSensitiveQueryParam(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "key", "token", "access_token", "refresh_token", "client_secret", "code", "secret", "password":
		return true
	default:
		return false
	}
}

func sanitizeLogValue(s string) string {
	if s == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}
