package logrotate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotator_Rotate(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	// Create a log file with some content
	content := make([]byte, 1024*1024) // 1MB
	for i := range content {
		content[i] = 'A'
	}

	err := os.WriteFile(logPath, content, 0644)
	if err != nil {
		t.Fatalf("Failed to create test log file: %v", err)
	}

	rotator := NewRotator(logPath, 512*1024, 3, 0) // 512KB max, 3 backups

	// Force rotation
	err = rotator.rotate()
	if err != nil {
		t.Fatalf("Failed to rotate: %v", err)
	}

	// Check that backup .1 was created
	backup1 := logPath + ".1"
	if _, err := os.Stat(backup1); os.IsNotExist(err) {
		t.Error("Backup file .1 was not created")
	}

	// Check that new log file exists
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		t.Error("New log file was not created")
	}

	// Check that new log file is empty or smaller
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("Failed to stat new log file: %v", err)
	}
	if info.Size() >= 512*1024 {
		t.Errorf("New log file is too large: %d bytes", info.Size())
	}
}

func TestRotator_GetBackupPath(t *testing.T) {
	rotator := NewRotator("/var/log/test.log", 1024, 5, 0)

	tests := []struct {
		num      int
		expected string
	}{
		{1, "/var/log/test.log.1"},
		{2, "/var/log/test.log.2"},
		{5, "/var/log/test.log.5"},
	}

	for _, tt := range tests {
		t.Run(string(rune(tt.num)), func(t *testing.T) {
			result := rotator.getBackupPath(tt.num)
			if result != tt.expected {
				t.Errorf("getBackupPath(%d) = %s, expected %s", tt.num, result, tt.expected)
			}
		})
	}
}

func TestParseBackupNumber(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"1", 1},
		{"10", 10},
		{"123", 123},
		{"abc", 0},
		{"1a", 0},
		{"", 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := parseBackupNumber(tt.input)
			if result != tt.expected {
				t.Errorf("parseBackupNumber(%s) = %d, expected %d", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsNumeric(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"1", true},
		{"123", true},
		{"0", true},
		{"abc", false},
		{"1a", false},
		{"", false},
		{"-1", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := isNumeric(tt.input)
			if result != tt.expected {
				t.Errorf("isNumeric(%s) = %v, expected %v", tt.input, result, tt.expected)
			}
		})
	}
}
