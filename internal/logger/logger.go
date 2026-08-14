// Package logger provides structured logging with automatic log rotation.
//
// Logs are written to disk through lumberjack, which rotates them on size.
// The package wraps slog and adds two levels it does not carry, Trace below
// Debug and Fail above Error.
package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"

	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/fsretry"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Logger embeds *slog.Logger so callers get every standard slog method
// (Debug, Info, Warn, Error, With, WithGroup, ...) as a receiver method,
// and adds Trace and Fail for the custom levels slog does not expose.
//
// Use this type wherever you would otherwise hold a *slog.Logger so the
// custom-level methods stay reachable. Pass l.Logger to APIs that
// require a plain *slog.Logger.
type Logger struct {
	*slog.Logger
}

// Handler implements slog.Handler with log4net-style formatting.
// It writes timestamped, level-tagged log lines with optional key-value
// attributes and group prefixes.
type Handler struct {
	w      io.Writer
	level  slog.Level
	fields []field
	prefix string
}

// field is an attribute whose rendered key was fixed when the attribute was
// added.
//
// slog scopes a group to the attributes that follow it, so the prefix an
// attribute carries is the one in force at the moment it joined the handler.
// Deriving it later from the handler's current group would relabel earlier
// attributes and can render two of them under one name.
type field struct {
	key   string
	value slog.Value
}

// ///////////////////////////////////////////////
// Log Levels
// ///////////////////////////////////////////////

// The severity scale, on the four-apart spacing slog uses for its own
// levels.
const (
	LevelTrace = slog.Level(-8)
	LevelDebug = slog.Level(-4)
	LevelInfo  = slog.Level(0)
	LevelWarn  = slog.Level(4)
	LevelError = slog.Level(8)
	LevelFail  = slog.Level(12)
)

// logDirMode keeps the log directory to its owner. The log sits beside the
// database and the credential file on every install that does not separate
// them, so whichever component creates the directory first decides who can
// read what is in it.
const logDirMode = 0o700

// lineEnding is the platform-appropriate line ending for log output.
var lineEnding = "\n"

// levelNames maps custom slog levels to human-readable names used in log output.
var levelNames = map[slog.Level]string{
	LevelTrace: "TRACE",
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
	LevelFail:  "FAIL",
}

func init() {
	if runtime.GOOS == "windows" {
		lineEnding = "\r\n"
	}
}

// ///////////////////////////////////////////////
// Constructors
// ///////////////////////////////////////////////

// New wraps an existing *slog.Logger so callers can use Trace and Fail.
// Returns nil if base is nil.
func New(base *slog.Logger) *Logger {
	if base == nil {
		return nil
	}
	return &Logger{Logger: base}
}

// Discard returns a Logger that drops every record.
func Discard() *Logger {
	return &Logger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// NewHandler creates a new Handler that writes to w, filtering out
// records below minLevel.
func NewHandler(w io.Writer, level slog.Level) *Handler {
	return &Handler{w: w, level: level}
}

// NewLogger creates a configured Logger backed by lumberjack rotation.
// The returned io.Closer must be closed when the logger is no longer needed.
func NewLogger(logPath string, minLevel slog.Level, maxSizeMB int) (*Logger, io.Closer, error) {
	// Owner-only, and narrow rather than wide because this directory is the
	// data directory whenever the log sits beside the database, which is
	// every Windows install and every Unix one with the home overridden. A
	// notify agent starting before anything else would otherwise be what
	// creates the directory holding the OAuth token, at whatever mode it
	// asked for, and MkdirAll does nothing to one already there.
	logDir := filepath.Dir(logPath)
	if err := fsretry.MkdirPrivate(logDir, logDirMode); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}

	rotator := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    maxSizeMB,
		MaxBackups: 3,
		MaxAge:     28,
		Compress:   false,
	}

	handler := NewHandler(rotator, minLevel)
	return &Logger{Logger: slog.New(handler)}, rotator, nil
}

// ///////////////////////////////////////////////
// Logger custom-level methods
// ///////////////////////////////////////////////

// Trace logs a message at the TRACE level (slog level -8).
func (l *Logger) Trace(msg string, args ...any) {
	l.Log(context.Background(), LevelTrace, msg, args...)
}

// Fail logs a message at the FAIL level (slog level 12).
func (l *Logger) Fail(msg string, args ...any) {
	l.Log(context.Background(), LevelFail, msg, args...)
}

// With returns a Logger that always carries the given attributes.
// Overrides slog.Logger.With so the returned Logger keeps Trace and Fail.
func (l *Logger) With(args ...any) *Logger {
	return &Logger{Logger: l.Logger.With(args...)}
}

// WithGroup returns a Logger that prefixes attribute keys with name.
// Overrides slog.Logger.WithGroup so the returned Logger keeps Trace and Fail.
func (l *Logger) WithGroup(name string) *Logger {
	return &Logger{Logger: l.Logger.WithGroup(name)}
}

// ///////////////////////////////////////////////
// Handler interface methods
// ///////////////////////////////////////////////

// Enabled reports whether the handler is configured to log at the given level.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle formats and writes a single log record. The output format is:
//
//	<timestamp> [<LEVEL>] <message> | <key=value>, ...
//
// Message, keys, and values all go through escape.Field rather than being
// escaped at the call sites. Most of what reaches a log line is written by
// someone else, a stream title or a category or a filename, and a record is
// one line with a known field structure: text carrying a line ending would
// close this record and open a convincing fake one after it, and text
// carrying a separator would add fields of its own. Rendering in one place
// is the only version of this that cannot be forgotten at a new call site.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	// A level with no name of its own renders through slog's stringer,
	// which spells it as an offset from the nearest named one.
	levelName := levelNames[r.Level]
	if levelName == "" {
		levelName = r.Level.String()
	}

	timestamp := r.Time.UTC().Format("2006-01-02T15:04:05.000Z")
	line := fmt.Sprintf("%s [%s] %s", timestamp, levelName, escape.Field(r.Message))

	attrs := make([]string, 0, len(h.fields)+r.NumAttrs())
	for _, f := range h.fields {
		attrs = append(attrs, formatField(f))
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, formatField(field{key: h.prefix + a.Key, value: a.Value}))
		return true
	})

	if len(attrs) > 0 {
		line += " | " + strings.Join(attrs, ", ")
	}

	line += lineEnding
	_, err := h.w.Write([]byte(line))
	return err
}

// formatField renders one attribute as key=value.
//
// The key is rendered alongside the value because an attribute key can come
// from a map of remote metadata just as easily as from a literal in this
// repo, and the cost of quoting one that never needed it is nothing.
func formatField(f field) string {
	return fmt.Sprintf("%s=%s", escape.Field(f.key), escape.Field(f.value.String()))
}

// WithAttrs returns a new Handler whose output includes the given attributes
// on every record. Returns the receiver unchanged if attrs is empty.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := *h
	h2.fields = make([]field, len(h.fields), len(h.fields)+len(attrs))
	copy(h2.fields, h.fields)
	for _, a := range attrs {
		h2.fields = append(h2.fields, field{key: h.prefix + a.Key, value: a.Value})
	}
	return &h2
}

// WithGroup returns a new Handler that prefixes the keys of every attribute
// added after it with the given group name. Returns the receiver unchanged
// if name is empty.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.prefix = h.prefix + name + "."
	return &h2
}
