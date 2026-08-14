package logger

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/natefinch/lumberjack.v2"

	"zach.tools/go/stream-dvr/internal/escape"
)

// ///////////////////////////////////////////////
// Test Helpers
// ///////////////////////////////////////////////

// newTestLogger creates a logger for testing and registers cleanup.
func newTestLogger(t *testing.T, logPath string, minLevel slog.Level, maxSizeMB int) (*Logger, *lumberjack.Logger) {
	t.Helper()

	logDir := filepath.Dir(logPath)
	require.NoError(t, os.MkdirAll(logDir, 0o755), "create log directory")

	rotator := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    maxSizeMB,
		MaxBackups: 3,
		MaxAge:     28,
		Compress:   false,
	}

	t.Cleanup(func() {
		rotator.Close()
	})

	handler := NewHandler(rotator, minLevel)
	return &Logger{Logger: slog.New(handler)}, rotator
}

// ///////////////////////////////////////////////
// New
// ///////////////////////////////////////////////

func TestNew_WrapsSlogLogger(t *testing.T) {
	base := slog.New(NewHandler(os.Stdout, LevelInfo))
	got := New(base)
	require.NotNil(t, got)
	assert.Same(t, base, got.Logger)
}

func TestNew_NilReturnsNil(t *testing.T) {
	assert.Nil(t, New(nil))
}

// ///////////////////////////////////////////////
// Logger Factory
// ///////////////////////////////////////////////

func TestNewLogger(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "newlogger", "test.log")

	logger, closer, err := NewLogger(logPath, LevelInfo, 10)
	require.NoError(t, err)
	defer closer.Close()

	require.NotNil(t, logger)

	logger.Info("hello from NewLogger")
	content, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "hello from NewLogger")
}

func TestNewLogger_MkdirAllError(t *testing.T) {
	tmpDir := t.TempDir()
	blocker := filepath.Join(tmpDir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))

	logPath := filepath.Join(blocker, "sub", "test.log")
	_, _, err := NewLogger(logPath, LevelInfo, 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create log directory")
}

func TestNewLogger_FileOutput(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, LevelInfo, 10)

	require.NotNil(t, logger)

	logger.Info("test info", "key1", "value1")
	logger.Warn("test warn", "key2", "value2")
	logger.Error("test error", "key3", "value3")

	_, err := os.Stat(logPath)
	assert.False(t, os.IsNotExist(err), "Expected log file to be created")

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)

	contentStr := string(content)
	assert.Contains(t, contentStr, "test info")
	assert.Contains(t, contentStr, "key1")
}

func TestNewLogger_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "subdir", "nested", "test.log")

	logger, _ := newTestLogger(t, logPath, LevelInfo, 10)

	logger.Info("test message")

	_, err := os.Stat(filepath.Join(tmpDir, "subdir", "nested"))
	assert.False(t, os.IsNotExist(err), "Expected nested directories to be created")

	_, err = os.Stat(logPath)
	assert.False(t, os.IsNotExist(err), "Expected log file to be created")
}

// ///////////////////////////////////////////////
// Logger custom-level methods
// ///////////////////////////////////////////////

func TestLogger_Trace(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, LevelTrace, 10)

	logger.Trace("trace via method", "k", "v")

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "trace via method")
	assert.Contains(t, string(content), "TRACE")
	assert.Contains(t, string(content), "k=v")
}

func TestLogger_Fail(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, LevelTrace, 10)

	logger.Fail("fail via method", "error", "critical")

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "fail via method")
	assert.Contains(t, string(content), "FAIL")
	assert.Contains(t, string(content), "error=critical")
}

func TestLogger_With(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, LevelTrace, 10)

	child := logger.With("service", "api")
	require.IsType(t, &Logger{}, child, "With should return *Logger so Trace/Fail stay reachable")

	child.Trace("traced", "k", "v")
	child.Info("infoed")

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "service=api")
	assert.Contains(t, string(content), "traced")
	assert.Contains(t, string(content), "infoed")
}

func TestLogger_WithGroup(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, LevelTrace, 10)

	child := logger.WithGroup("req")
	require.IsType(t, &Logger{}, child, "WithGroup should return *Logger so Trace/Fail stay reachable")

	child.Trace("handled", "method", "GET")

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(content), "req.method=GET")
}

// ///////////////////////////////////////////////
// Handler levels
// ///////////////////////////////////////////////

func TestHandler_Levels(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, LevelInfo, 10)

	logger.Debug("debug message")
	logger.Info("info message")
	logger.Warn("warn message")
	logger.Error("error message")

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)

	contentStr := string(content)

	assert.NotContains(t, contentStr, "debug message")

	assert.Contains(t, contentStr, "info message")
	assert.Contains(t, contentStr, "warn message")
	assert.Contains(t, contentStr, "error message")
}

func TestHandler_DebugMode(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, LevelDebug, 10)

	logger.Debug("debug message")
	logger.Info("info message")

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)

	contentStr := string(content)

	assert.Contains(t, contentStr, "debug message")
	assert.Contains(t, contentStr, "info message")
}

func TestHandler_CustomLevels(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, LevelTrace, 10)

	logger.Trace("trace message", "key", "value")
	logger.Info("info message")
	logger.Fail("fail message", "error", "critical")

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)

	contentStr := string(content)

	assert.Contains(t, contentStr, "TRACE")
	assert.Contains(t, contentStr, "INFO")
	assert.Contains(t, contentStr, "FAIL")
}

// ///////////////////////////////////////////////
// Handler interface
// ///////////////////////////////////////////////

func TestNewHandler_DefaultsToInfo(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")
	logger, _ := newTestLogger(t, logPath, LevelInfo, 10)

	assert.True(t, logger.Enabled(context.TODO(), slog.LevelInfo), "Expected Info level to be enabled")

	assert.False(t, logger.Enabled(context.TODO(), slog.LevelDebug), "Expected Debug level to be disabled by default")
}

func TestNewHandler_DebugEnabled(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")
	logger, _ := newTestLogger(t, logPath, LevelDebug, 10)

	assert.True(t, logger.Enabled(context.TODO(), slog.LevelDebug), "Expected Debug level to be enabled in debug mode")
}

func TestHandler_WithAttributes(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, LevelInfo, 10)

	logger.Info("test message",
		"string_key", "string_value",
		"int_key", 42,
		"bool_key", true,
	)

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)

	contentStr := string(content)

	assert.Contains(t, contentStr, "string_key")
	assert.Contains(t, contentStr, "string_value")
	assert.Contains(t, contentStr, "int_key")
	assert.Contains(t, contentStr, "42")
}

func TestHandler_Format(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, LevelInfo, 10)

	logger.Info("test message", "key", "value")

	file, err := os.Open(logPath)
	require.NoError(t, err)
	defer file.Close()

	scanner := bufio.NewScanner(file)
	require.True(t, scanner.Scan(), "Expected at least one log line")

	line := scanner.Text()

	assert.GreaterOrEqual(t, len(line), 10, "Expected log line to contain timestamp")

	assert.Contains(t, line, "INFO")

	assert.Contains(t, line, "test message")
}

// ///////////////////////////////////////////////
// Untrusted text
// ///////////////////////////////////////////////

func TestHandler_UntrustedTextCannotForgeARecord(t *testing.T) {
	// A stream title is written by the broadcaster. Left as it arrives, one
	// carrying a line ending closes the real record and opens a second that
	// reads exactly like a genuine line, timestamp and level included.
	const forged = "\r\n2026-08-12T00:00:00.000Z [INFO] recording complete"

	tests := []struct {
		name string
		emit func(l *Logger, payload string)
	}{
		{name: "in the message", emit: func(l *Logger, p string) { l.Info(p) }},
		{name: "in an attribute value", emit: func(l *Logger, p string) { l.Info("stream live", "title", p) }},
		{name: "in an attribute key", emit: func(l *Logger, p string) { l.Info("stream live", p, "x") }},
		{name: "carried by With", emit: func(l *Logger, p string) { l.With("title", p).Info("stream live") }},
		{name: "carried by WithGroup", emit: func(l *Logger, p string) { l.WithGroup("poll").Info("stream live", "title", p) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := &Logger{Logger: slog.New(NewHandler(&buf, LevelInfo))}

			tt.emit(logger, "ExampleChannel live"+forged)

			written := buf.String()
			body := strings.TrimSuffix(written, lineEnding)

			// The payload's text is allowed to survive, and must: the
			// operator needs to see what arrived. What must not survive is
			// the line ending that would make it a record of its own.
			lines := strings.Split(body, lineEnding)
			assert.Len(t, lines, 1, "one call produced %d physical lines: %q", len(lines), written)
			assert.NotContains(t, body, "\r", "a raw carriage return survived into the record")
			assert.NotContains(t, body, "\n", "a raw line feed survived into the record")
		})
	}
}

func TestHandler_UntrustedTextCannotForgeAField(t *testing.T) {
	// A record is a message and a list of key=value fields. Text carrying
	// those separators renders as fields of its own choosing, so an operator
	// reading the log is shown a level, an error, or a channel nobody logged.
	// Reachable with fully remote text: the daemon logs a stream title.
	tests := []struct {
		name       string
		emit       func(l *Logger)
		wantFields int
	}{
		{
			name:       "a value carrying a comma and an equals",
			emit:       func(l *Logger) { l.Info("stream live", "title", "movie night, level=FAIL, error=disk gone") },
			wantFields: 1,
		},
		{
			name:       "a key carrying a comma",
			emit:       func(l *Logger) { l.Info("stream live", "a=1, b", "2") },
			wantFields: 1,
		},
		{
			name:       "a message carrying the attribute separator",
			emit:       func(l *Logger) { l.Info("stream live | user=root") },
			wantFields: 0,
		},
		{
			name:       "a value carried by With",
			emit:       func(l *Logger) { l.With("title", "night, level=FAIL").Info("stream live") },
			wantFields: 1,
		},
		{
			name:       "a real attribute list still parses",
			emit:       func(l *Logger) { l.Info("stream live", "channel", "examplechannel", "id", 7) },
			wantFields: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := &Logger{Logger: slog.New(NewHandler(&buf, LevelInfo))}

			tt.emit(logger)

			body := strings.TrimSuffix(buf.String(), lineEnding)
			fields := recordFields(body)
			assert.Len(t, fields, tt.wantFields,
				"a reader of %q sees %d fields, want %d", body, len(fields), tt.wantFields)
		})
	}
}

func TestHandler_AForgedFieldIsStillReadable(t *testing.T) {
	// Quoting is chosen over stripping so the operator still sees exactly
	// what arrived, which is itself the interesting part.
	const title = "movie night, level=FAIL, error=disk gone"

	var buf bytes.Buffer
	logger := &Logger{Logger: slog.New(NewHandler(&buf, LevelInfo))}

	logger.Info("stream live", "title", title)

	fields := recordFields(strings.TrimSuffix(buf.String(), lineEnding))
	require.Len(t, fields, 1)

	key, value, found := cutUnquoted(fields[0], "=")
	require.True(t, found, "field %q has no key=value split", fields[0])
	assert.Equal(t, "title", key)

	recovered, err := strconv.Unquote(value)
	require.NoError(t, err, "value %q is not a Go literal", value)
	assert.Equal(t, title, recovered, "the title a reader recovers is not the one that arrived")
}

func TestHandler_OneRecordCannotEatTheRotationBudget(t *testing.T) {
	// streamlink's own output is wrapped into an error and logged as an
	// attribute, and lumberjack rotates on total bytes: one record that long
	// evicts every other record in the retained window.
	const budget = 4 * escape.MaxOut

	payload := strings.Repeat("x", 200_000)

	var buf bytes.Buffer
	logger := &Logger{Logger: slog.New(NewHandler(&buf, LevelInfo))}

	logger.Info("capture failed", "error", payload)

	assert.LessOrEqual(t, buf.Len(), budget,
		"one record wrote %d bytes for a %d byte attribute, want at most %d", buf.Len(), len(payload), budget)
	assert.Contains(t, buf.String(), strconv.Itoa(len(payload)),
		"the record does not say how much of the attribute was dropped")
}

// ///////////////////////////////////////////////
// Groups
// ///////////////////////////////////////////////

func TestHandler_AGroupOnlyLabelsWhatFollowsIt(t *testing.T) {
	// slog scopes a group to the attributes added after it. Labelling the
	// earlier ones with it renames them, and two attributes can end up
	// rendered under one name with nothing to tell them apart.
	var buf bytes.Buffer
	base := slog.New(NewHandler(&buf, LevelInfo))

	base.With("session", 7).WithGroup("capture").Info("started", "id", 2)

	line := buf.String()
	assert.Contains(t, line, "session=7", "an attribute added before the group was relabelled")
	assert.Contains(t, line, "capture.id=2", "an attribute added after the group lost its prefix")
	assert.NotContains(t, line, "capture.session", "the group reached back to an earlier attribute")
}

func TestHandler_AGroupCannotCollideTwoAttributes(t *testing.T) {
	// The same key on either side of a group is two facts. Rendering both
	// under one name loses one of them silently.
	var buf bytes.Buffer
	base := slog.New(NewHandler(&buf, LevelInfo))

	base.With("id", "outer").WithGroup("capture").Info("started", "id", "inner")

	fields := recordFields(strings.TrimSuffix(buf.String(), lineEnding))
	require.Len(t, fields, 2)

	keys := make([]string, 0, len(fields))
	for _, f := range fields {
		key, _, _ := cutUnquoted(f, "=")
		keys = append(keys, key)
	}
	assert.Equal(t, []string{"id", "capture.id"}, keys, "two attributes rendered under names that do not tell them apart")
}

// ///////////////////////////////////////////////
// Reading a record back
// ///////////////////////////////////////////////

// recordFields splits a rendered record's attribute list into fields.
//
// This is the reader a log consumer has to be, so assertions run through it
// rather than through a naive split: a field's own text may contain a
// separator, and the quoting is what says so.
func recordFields(line string) []string {
	_, attrs, found := cutUnquoted(line, " | ")
	if !found {
		return nil
	}

	var fields []string
	for {
		head, tail, more := cutUnquoted(attrs, ", ")
		fields = append(fields, head)
		if !more {
			return fields
		}
		attrs = tail
	}
}

// cutUnquoted splits s at the first sep that falls outside a Go string
// literal, tracking the backslash escapes such a literal may contain.
func cutUnquoted(s, sep string) (before, after string, found bool) {
	quoted := false
	for i := 0; i < len(s); i++ {
		switch {
		case quoted && s[i] == '\\':
			i++
		case s[i] == '"':
			quoted = !quoted
		case !quoted && strings.HasPrefix(s[i:], sep):
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func TestHandler_TerminalControlBytesDoNotReachTheFile(t *testing.T) {
	// serve --foreground writes to a terminal, and any cat or Get-Content
	// of the log renders it in one. An escape sequence stored raw runs
	// against whoever reads the file.
	var buf bytes.Buffer
	logger := &Logger{Logger: slog.New(NewHandler(&buf, LevelInfo))}

	logger.Info("stream live", "title", "clear\x1b[2Jthe screen\a")

	assert.NotContains(t, buf.String(), "\x1b", "an escape byte reached the log file")
	assert.NotContains(t, buf.String(), "\a", "a bell byte reached the log file")
	assert.Contains(t, buf.String(), "the screen", "the title itself should still be readable")
}

func TestHandler_NonStandardLevel(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	logger, _ := newTestLogger(t, logPath, slog.Level(-10), 10)

	logger.Log(context.Background(), slog.Level(2), "non-standard level message")

	content, err := os.ReadFile(logPath)
	require.NoError(t, err)

	contentStr := string(content)
	assert.Contains(t, contentStr, "non-standard level message")
	assert.Contains(t, contentStr, "INFO+2")
}

func TestHandler_WithAttrsEmpty(t *testing.T) {
	handler := NewHandler(os.Stdout, LevelInfo)
	got := handler.WithAttrs(nil)
	assert.Same(t, handler, got.(*Handler), "WithAttrs with no attrs should return same handler")
}

func TestHandler_WithGroupEmpty(t *testing.T) {
	handler := NewHandler(os.Stdout, LevelInfo)
	got := handler.WithGroup("")
	assert.Same(t, handler, got.(*Handler), "WithGroup with empty name should return same handler")
}
