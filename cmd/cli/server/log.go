package server

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Cloud-Developer-Department/hwcloud/cmd/cli/config"
	"github.com/Cloud-Developer-Department/hwcloud/kernel"
)

// SetupLog configures slog + log package output. Writes to a rotated log
// file when cfg.File is set; otherwise discards log output silently.
//
// IMPORTANT: does NOT write to os.Stderr. The ACP protocol uses stderr as
// a control pipe — any log output there fills the pipe buffer and blocks
// the process. Use fmt.Fprintf(os.Stderr, ...) for intentional console output.
func SetupLog(cfg config.LogConfig) (func(), error) {
	level := parseLevel(cfg.Level)

	mw := &multiCloser{}

	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0755); err != nil {
			return nil, fmt.Errorf("log dir: %w", err)
		}
		rw, err := newRotWriter(cfg.File, cfg.MaxSize, cfg.MaxBackups)
		if err != nil {
			return nil, fmt.Errorf("log file: %w", err)
		}
		mw.AddCloser(rw)
	}

	// When no log file is configured, fall back to discarding.
	// log.Printf and slog output go only to the file, never to stderr.
	if len(mw.writers) == 0 {
		mw.AddWriter(io.Discard)
	}

	// slog.
	h := slog.NewJSONHandler(mw, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))

	// Redirect log.Printf etc. to the same writer so existing
	// log calls in server packages also land in the log file.
	log.SetOutput(mw)

	return func() { mw.Close() }, nil
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return kernel.LevelTrace
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ── multiCloser ──

type multiCloser struct {
	mu      sync.Mutex
	writers []io.Writer
	closers []io.Closer
}

func (m *multiCloser) AddWriter(w io.Writer) {
	m.writers = append(m.writers, w)
	if c, ok := w.(io.Closer); ok {
		m.closers = append(m.closers, c)
	}
}

func (m *multiCloser) AddCloser(c io.Closer) {
	m.writers = append(m.writers, c.(io.Writer))
	m.closers = append(m.closers, c)
}

func (m *multiCloser) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var lastN int
	var lastErr error
	for _, w := range m.writers {
		n, err := w.Write(p)
		if err == nil {
			lastN = n
		} else {
			lastErr = err
		}
	}
	return lastN, lastErr
}

func (m *multiCloser) Close() {
	for _, c := range m.closers {
		_ = c.Close()
	}
}

// ── rotWriter ──

type rotWriter struct {
	mu         sync.Mutex
	path       string
	maxSize    int
	maxBackups int
	f          *os.File
	size       int64
}

func newRotWriter(path string, maxSizeMB, maxBackups int) (*rotWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	fi, _ := f.Stat()
	return &rotWriter{
		path:       path,
		maxSize:    maxSizeMB * 1024 * 1024,
		maxBackups: maxBackups,
		f:          f,
		size:       fi.Size(),
	}, nil
}

func (w *rotWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.f.Write(p)
	w.size += int64(n)

	if w.size >= int64(w.maxSize) {
		w.rotate()
	}
	return n, err
}

func (w *rotWriter) rotate() {
	w.f.Close()

	ts := fmt.Sprintf("%d", time.Now().Unix())
	backup := w.path + "." + ts
	os.Rename(w.path, backup)

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	w.f = f
	w.size = 0

	dir := filepath.Dir(w.path)
	base := filepath.Base(w.path)
	entries, _ := os.ReadDir(dir)
	var backups []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), base+".") {
			if e.Name() == base+"."+ts {
				continue
			}
			backups = append(backups, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(backups)
	if w.maxBackups > 0 && len(backups) > w.maxBackups {
		for _, p := range backups[:len(backups)-w.maxBackups] {
			os.Remove(p)
		}
	}
}

func (w *rotWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
