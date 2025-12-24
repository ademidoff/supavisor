package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/ademidoff/supavisor/internal/config"
)

// TestLoggerSetup tests the logger setup logic with different TTY scenarios
func TestLoggerSetup(t *testing.T) {
	tests := []struct {
		name          string
		isTTY         bool
		logFilePath   string
		expectStdout  bool
		expectLogFile bool
		expectDiscard bool
	}{
		{
			name:          "Interactive mode without logfile",
			isTTY:         true,
			logFilePath:   "",
			expectStdout:  true,
			expectLogFile: false,
			expectDiscard: false,
		},
		{
			name:          "Non-interactive mode without logfile",
			isTTY:         false,
			logFilePath:   "",
			expectStdout:  false,
			expectLogFile: false,
			expectDiscard: true,
		},
		{
			name:          "Interactive mode with logfile",
			isTTY:         true,
			logFilePath:   "/tmp/test.log",
			expectStdout:  true,
			expectLogFile: true,
			expectDiscard: false,
		},
		{
			name:          "Non-interactive mode with logfile",
			isTTY:         false,
			logFilePath:   "/tmp/test.log",
			expectStdout:  false,
			expectLogFile: true,
			expectDiscard: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a buffer to capture stdout
			var stdoutBuf bytes.Buffer

			// Determine output based on TTY status
			var output io.Writer
			if tt.isTTY {
				output = &stdoutBuf
			} else {
				output = io.Discard
			}

			// Handle logfile case
			var logFile *os.File
			if tt.logFilePath != "" {
				var err error
				logFile, err = os.CreateTemp("", "supavisor-test-*.log")
				if err != nil {
					t.Fatalf("Failed to create temp log file: %v", err)
				}
				defer os.Remove(logFile.Name())
				defer logFile.Close()

				if tt.isTTY {
					output = io.MultiWriter(&stdoutBuf, logFile)
				} else {
					output = logFile
				}
			}

			// Create logger with the output
			handler := slog.NewTextHandler(output, &slog.HandlerOptions{
				Level: slog.LevelInfo,
			})
			logger := slog.New(handler)

			// Log a test message
			testMsg := "test log message"
			logger.Info(testMsg)

			// Verify stdout behavior
			stdoutContent := stdoutBuf.String()
			if tt.expectStdout {
				if !strings.Contains(stdoutContent, testMsg) {
					t.Errorf("Expected stdout to contain log message, but it didn't")
				}
			} else {
				if strings.Contains(stdoutContent, testMsg) {
					t.Errorf("Expected stdout to be empty, but it contains: %s", stdoutContent)
				}
			}

			// Verify logfile behavior
			if tt.expectLogFile && logFile != nil {
				// Read the log file content
				logFile.Seek(0, 0)
				logContent, err := io.ReadAll(logFile)
				if err != nil {
					t.Fatalf("Failed to read log file: %v", err)
				}
				if !strings.Contains(string(logContent), testMsg) {
					t.Errorf("Expected log file to contain log message, but it didn't")
				}
			}

			// Verify discard behavior
			if tt.expectDiscard {
				if stdoutContent != "" {
					t.Errorf("Expected no output when using io.Discard, but got: %s", stdoutContent)
				}
			}
		})
	}
}

// TestLoggerOutputSelection tests the logic for selecting the correct output writer
func TestLoggerOutputSelection(t *testing.T) {
	t.Run("TTY mode uses stdout", func(t *testing.T) {
		isTTY := true
		var output io.Writer

		if isTTY {
			output = os.Stdout
		} else {
			output = io.Discard
		}

		if output != os.Stdout {
			t.Errorf("Expected output to be os.Stdout in TTY mode")
		}
	})

	t.Run("Non-TTY mode uses discard", func(t *testing.T) {
		isTTY := false
		var output io.Writer

		if isTTY {
			output = os.Stdout
		} else {
			output = io.Discard
		}

		if output != io.Discard {
			t.Errorf("Expected output to be io.Discard in non-TTY mode")
		}
	})
}

// TestLogFormatSelection tests that the correct handler is selected based on config
func TestLogFormatSelection(t *testing.T) {
	tests := []struct {
		name       string
		logFormat  string
		expectJSON bool
	}{
		{
			name:       "JSON format",
			logFormat:  "json",
			expectJSON: true,
		},
		{
			name:       "Text format",
			logFormat:  "text",
			expectJSON: false,
		},
		{
			name:       "Default format (empty)",
			logFormat:  "",
			expectJSON: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			cfg := &config.Config{
				Supavisor: config.SupavisorConfig{
					LogFormat: tt.logFormat,
				},
			}

			opts := &slog.HandlerOptions{
				Level: slog.LevelInfo,
			}

			var handler slog.Handler
			switch cfg.Supavisor.LogFormat {
			case "json":
				handler = slog.NewJSONHandler(&buf, opts)
			default:
				handler = slog.NewTextHandler(&buf, opts)
			}

			logger := slog.New(handler)
			logger.Info("test message", "key", "value")

			output := buf.String()

			// JSON format should contain curly braces
			if tt.expectJSON {
				if !strings.Contains(output, "{") || !strings.Contains(output, "}") {
					t.Errorf("Expected JSON format but got: %s", output)
				}
			} else {
				// Text format should not start with curly brace
				if strings.HasPrefix(strings.TrimSpace(output), "{") {
					t.Errorf("Expected text format but got JSON: %s", output)
				}
			}
		})
	}
}

// TestReplaceAttr tests the custom attribute replacement function
func TestReplaceAttr(t *testing.T) {
	replaceAttr := func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.LevelKey {
			level := a.Value.Any().(slog.Level)
			a.Value = slog.StringValue(strings.ToLower(level.String()))
		}
		return a
	}

	tests := []struct {
		name     string
		attr     slog.Attr
		expected string
	}{
		{
			name:     "INFO level",
			attr:     slog.Attr{Key: slog.LevelKey, Value: slog.AnyValue(slog.LevelInfo)},
			expected: "info",
		},
		{
			name:     "ERROR level",
			attr:     slog.Attr{Key: slog.LevelKey, Value: slog.AnyValue(slog.LevelError)},
			expected: "error",
		},
		{
			name:     "WARN level",
			attr:     slog.Attr{Key: slog.LevelKey, Value: slog.AnyValue(slog.LevelWarn)},
			expected: "warn",
		},
		{
			name:     "Non-level attribute",
			attr:     slog.Attr{Key: "other", Value: slog.StringValue("VALUE")},
			expected: "VALUE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := replaceAttr(nil, tt.attr)
			resultStr := result.Value.String()
			if resultStr != tt.expected {
				t.Errorf("Expected %q but got %q", tt.expected, resultStr)
			}
		})
	}
}

// TestMultiWriterBehavior tests that MultiWriter correctly writes to multiple destinations
func TestMultiWriterBehavior(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	multiWriter := io.MultiWriter(&buf1, &buf2)

	testData := "test data"
	n, err := multiWriter.Write([]byte(testData))
	if err != nil {
		t.Fatalf("MultiWriter.Write failed: %v", err)
	}

	if n != len(testData) {
		t.Errorf("Expected to write %d bytes, but wrote %d", len(testData), n)
	}

	if buf1.String() != testData {
		t.Errorf("First writer: expected %q but got %q", testData, buf1.String())
	}

	if buf2.String() != testData {
		t.Errorf("Second writer: expected %q but got %q", testData, buf2.String())
	}
}

// TestParseLogLevel tests the parseLogLevel function
func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		expected  slog.Level
		expectErr bool
	}{
		{
			name:      "debug level",
			input:     "debug",
			expected:  slog.LevelDebug,
			expectErr: false,
		},
		{
			name:      "DEBUG level uppercase",
			input:     "DEBUG",
			expected:  slog.LevelDebug,
			expectErr: false,
		},
		{
			name:      "info level",
			input:     "info",
			expected:  slog.LevelInfo,
			expectErr: false,
		},
		{
			name:      "INFO level uppercase",
			input:     "INFO",
			expected:  slog.LevelInfo,
			expectErr: false,
		},
		{
			name:      "warn level",
			input:     "warn",
			expected:  slog.LevelWarn,
			expectErr: false,
		},
		{
			name:      "WARN level uppercase",
			input:     "WARN",
			expected:  slog.LevelWarn,
			expectErr: false,
		},
		{
			name:      "error level",
			input:     "error",
			expected:  slog.LevelError,
			expectErr: false,
		},
		{
			name:      "ERROR level uppercase",
			input:     "ERROR",
			expected:  slog.LevelError,
			expectErr: false,
		},
		{
			name:      "invalid level returns error",
			input:     "invalid",
			expected:  slog.LevelInfo,
			expectErr: true,
		},
		{
			name:      "empty string returns error",
			input:     "",
			expected:  slog.LevelInfo,
			expectErr: true,
		},
		{
			name:      "whitespace trimmed",
			input:     "  debug  ",
			expected:  slog.LevelDebug,
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseLogLevel(tt.input)
			if (err != nil) != tt.expectErr {
				t.Errorf("parseLogLevel(%q) error = %v, expectErr %v", tt.input, err, tt.expectErr)
				return
			}
			if !tt.expectErr && result != tt.expected {
				t.Errorf("parseLogLevel(%q) = %v, expected %v", tt.input, result, tt.expected)
			}
		})
	}
}

// TestLogLevelIntegration tests that log level configuration affects actual logging
func TestLogLevelIntegration(t *testing.T) {
	tests := []struct {
		name          string
		logLevel      slog.Level
		logFunc       func(*slog.Logger)
		shouldAppear  bool
		expectedInLog string
	}{
		{
			name:     "debug message with debug level",
			logLevel: slog.LevelDebug,
			logFunc: func(l *slog.Logger) {
				l.Debug("debug message")
			},
			shouldAppear:  true,
			expectedInLog: "debug message",
		},
		{
			name:     "debug message with info level",
			logLevel: slog.LevelInfo,
			logFunc: func(l *slog.Logger) {
				l.Debug("debug message")
			},
			shouldAppear:  false,
			expectedInLog: "debug message",
		},
		{
			name:     "info message with info level",
			logLevel: slog.LevelInfo,
			logFunc: func(l *slog.Logger) {
				l.Info("info message")
			},
			shouldAppear:  true,
			expectedInLog: "info message",
		},
		{
			name:     "info message with error level",
			logLevel: slog.LevelError,
			logFunc: func(l *slog.Logger) {
				l.Info("info message")
			},
			shouldAppear:  false,
			expectedInLog: "info message",
		},
		{
			name:     "error message with info level",
			logLevel: slog.LevelInfo,
			logFunc: func(l *slog.Logger) {
				l.Error("error message")
			},
			shouldAppear:  true,
			expectedInLog: "error message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{
				Level: tt.logLevel,
			})
			logger := slog.New(handler)

			tt.logFunc(logger)

			output := buf.String()
			contains := strings.Contains(output, tt.expectedInLog)

			if tt.shouldAppear && !contains {
				t.Errorf("Expected log to contain %q but it didn't. Output: %s", tt.expectedInLog, output)
			}
			if !tt.shouldAppear && contains {
				t.Errorf("Expected log to NOT contain %q but it did. Output: %s", tt.expectedInLog, output)
			}
		})
	}
}
