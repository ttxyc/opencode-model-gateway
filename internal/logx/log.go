package logx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var secretRe = regexp.MustCompile(`(ms-[A-Za-z0-9_-]{8,}|sk[-_][A-Za-z0-9_-]{8,}|rc-[A-Za-z0-9]{8,}|atr_[A-Za-z0-9_-]{8,}|nvapi-[A-Za-z0-9_-]{8,}|Bearer\s+[A-Za-z0-9._~+/=-]{8,})`)

type Logger struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	maxFiles int
	sizes    map[string]int64
}

func New(dir string, maxBytes int64, maxFiles int) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Logger{dir: dir, maxBytes: maxBytes, maxFiles: maxFiles, sizes: map[string]int64{}}, nil
}

func Redact(v any, limit int) string {
	var text string
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		text = x
	case error:
		text = x.Error()
	default:
		b, err := json.Marshal(x)
		if err != nil {
			text = fmt.Sprint(x)
		} else {
			text = string(b)
		}
	}
	text = secretRe.ReplaceAllStringFunc(text, func(m string) string {
		if strings.HasPrefix(strings.ToLower(m), "bearer") {
			return "Bearer ***"
		}
		if len(m) <= 9 {
			return m
		}
		return m[:5] + "..." + m[len(m)-4:]
	})
	if limit > 0 && len(text) > limit {
		text = text[:limit]
	}
	return text
}

func (l *Logger) append(file, line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	full := filepath.Join(l.dir, file)
	size := l.sizes[full]
	if size == 0 {
		if st, err := os.Stat(full); err == nil {
			size = st.Size()
		}
	}
	if l.maxBytes > 0 && size > l.maxBytes {
		l.rotate(full)
		size = 0
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
	l.sizes[full] = size + int64(len(line)) + 1
}

func (l *Logger) rotate(full string) {
	for i := l.maxFiles - 1; i >= 1; i-- {
		from := full
		if i > 1 {
			from = fmt.Sprintf("%s.%d", full, i-1)
		}
		to := fmt.Sprintf("%s.%d", full, i)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}
	_ = os.Remove(full)
	delete(l.sizes, full)
}

func (l *Logger) write(level, event string, fields map[string]any) {
	rec := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"level": level,
		"event": event,
	}
	for k, v := range fields {
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"ts": rec["ts"], "level": level, "event": event, "serializationError": true})
	}
	line := Redact(string(b), 200000)
	file := "gateway.log"
	if level == "error" || level == "crash" {
		file = "gateway-error.log"
	}
	l.append(file, line)
	if level == "crash" {
		l.append("gateway-crash.log", line)
	}
}

func (l *Logger) Info(event string, fields map[string]any)  { l.write("info", event, fields) }
func (l *Logger) Warn(event string, fields map[string]any)  { l.write("warn", event, fields) }
func (l *Logger) Error(event string, fields map[string]any) { l.write("error", event, fields) }
func (l *Logger) Crash(event string, fields map[string]any) { l.write("crash", event, fields) }

func (l *Logger) Path() string { return l.dir }
