// Package logging is the daemon's log surface: slog as the structured
// logger (stdlib idioms — levels, attrs, dynamic level) writing through a
// compact line handler whose format the tailer and the /logs API parse.
// Rotation is lumberjack's (size-based, backed-up, compressed) — the file
// lives at ~/.agentwork/daemon.log.
//
// Line format (parse contract — the tailer and ReadLogs depend on it):
//
//	2026-08-14T17:29:35+08:00 [INFO] message k=v
package logging

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// tsLayout is the fixed-width prefix stamped on every file line; parsing
	// is a plain slice, never a format guess.
	tsLayout = "2006-01-02T15:04:05Z07:00"
	// tsWidth is len(tsLayout's rendering) — RFC3339 without fraction.
	tsWidth = 25
)

// Level is a log severity (the API/panel spelling). The runtime level is a
// single atomic knob: the handler drops records below it for BOTH sinks
// (stdout and the file — the idiomatic slog behavior; the panel's level
// selector is the knob).
type Level int32

const (
	Debug Level = iota
	Info
	Warn
	Error
)

var levelNames = map[Level]string{Debug: "debug", Info: "info", Warn: "warn", Error: "error"}

func (l Level) String() string { return levelNames[l] }

// ParseLevel resolves an API level name (unknown → Info).
func ParseLevel(s string) Level {
	for lv, name := range levelNames {
		if name == s {
			return lv
		}
	}
	return Info
}

var currentLevel atomic.Int32

func init() { currentLevel.Store(int32(Info)) }

// SetLevel changes the runtime level (the Web panel's PUT /logs/level).
func SetLevel(l Level) { currentLevel.Store(int32(l)) }

// GetLevel returns the current runtime level.
func GetLevel() Level { return Level(currentLevel.Load()) }

// slogLevel maps a Level to the slog scale (debug=-4 … error=8).
func slogLevel(l Level) slog.Level {
	switch l {
	case Debug:
		return slog.LevelDebug
	case Warn:
		return slog.LevelWarn
	case Error:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// levelPrefix renders a record's level as the parse-able "[LEVEL]" prefix.
func levelPrefix(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "[ERROR]"
	case l >= slog.LevelWarn:
		return "[WARN]"
	case l <= slog.LevelDebug:
		return "[DEBUG]"
	default:
		return "[INFO]"
	}
}

// Debugf/Infof/Warnf/Errorf are the leveled entry points (slog-backed; the
// format strings are pre-formatted messages — structured attrs can come
// later per call site).
func Debugf(format string, args ...any) {
	slog.Log(context.Background(), slog.LevelDebug, fmt.Sprintf(format, args...))
}
func Infof(format string, args ...any)  { slog.Info(fmt.Sprintf(format, args...)) }
func Warnf(format string, args ...any)  { slog.Warn(fmt.Sprintf(format, args...)) }
func Errorf(format string, args ...any) { slog.Error(fmt.Sprintf(format, args...)) }

// Fatalf logs at error and exits — startup wiring failures only.
func Fatalf(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// DefaultPath returns the log file path (~/.agentwork/daemon.log).
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "agentwork-daemon.log")
	}
	return filepath.Join(home, ".agentwork", "daemon.log")
}

// compactHandler renders the established line format. The dynamic level is
// read per record (the atomic — the Web panel changes it live).
type compactHandler struct {
	w io.Writer
}

// NewHandler builds the compact handler over the given sink (the caller
// wires io.MultiWriter(stdout, lumberjack) and slog.SetDefault).
func NewHandler(w io.Writer) slog.Handler { return &compactHandler{w: w} }

func (h *compactHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= slogLevel(GetLevel())
}

func (h *compactHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format(tsLayout))
	b.WriteString(" ")
	b.WriteString(levelPrefix(r.Level))
	b.WriteString(" ")
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	b.WriteString("\n")
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *compactHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *compactHandler) WithGroup(name string) slog.Handler       { return h }

// Line is one parsed log line (the API and the live tailer share it). The
// level is kept as the enum internally; ReadLogs renders it for JSON.
type Line struct {
	TS    time.Time `json:"ts"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
	lvl   Level
}

// ParseLine splits a stamped line into its timestamp + text. Unparseable
// lines keep the zero time (the reader filters them only when a range is
// requested).
func ParseLine(ln string) Line {
	l := Line{}
	text := ln
	if len(text) > tsWidth+1 {
		if t, err := time.Parse(tsLayout, text[:tsWidth]); err == nil {
			l.TS = t
			text = text[tsWidth+1:]
		}
	}
	switch {
	case strings.HasPrefix(text, "[DEBUG] "):
		l.lvl, l.Text = Debug, text[len("[DEBUG] "):]
	case strings.HasPrefix(text, "[WARN] "):
		l.lvl, l.Text = Warn, text[len("[WARN] "):]
	case strings.HasPrefix(text, "[ERROR] "):
		l.lvl, l.Text = Error, text[len("[ERROR] "):]
	case strings.HasPrefix(text, "[INFO] "):
		l.lvl, l.Text = Info, text[len("[INFO] "):]
	default:
		l.lvl, l.Text = Info, text
	}
	return l
}

// logFiles lists the log files in chronological order (lumberjack backups
// first — their timestamped names sort after the base name lexically — then
// the live file).
func logFiles(path string) []string {
	matches, _ := filepath.Glob(path + "*")
	sort.Strings(matches)
	if len(matches) == 0 {
		return []string{path}
	}
	// The base name sorts FIRST (prefix rule) — move it to the end.
	if matches[0] == path {
		matches = append(matches[1:], matches[0])
	}
	return matches
}

// ReadLogs returns lines from the current file AND its lumberjack backups
// (oldest first), filtered by the optional time range and capped at limit
// (the tail of the range — most recent first internally, reversed).
func ReadLogs(path string, after, before *time.Time, limit int, minLevel Level) ([]Line, error) {
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	var raw []string
	for _, p := range logFiles(path) {
		f, err := os.Open(p)
		if err != nil {
			continue // a backup may not exist yet
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			raw = append(raw, sc.Text())
		}
		f.Close()
	}
	out := make([]Line, 0, limit)
	for i := len(raw) - 1; i >= 0 && len(out) < limit; i-- {
		l := ParseLine(raw[i])
		if l.lvl < minLevel {
			continue
		}
		if after != nil && !l.TS.IsZero() && !l.TS.After(*after) {
			continue
		}
		if before != nil && !l.TS.IsZero() && l.TS.After(*before) {
			continue
		}
		l.Level = l.lvl.String()
		out = append(out, l)
	}
	// reverse to oldest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
