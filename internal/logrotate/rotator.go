package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const rotatorCheckInterval = 5 * time.Second

// Rotator handles log file rotation
type Rotator struct {
	filePath      string
	maxBytes      int64
	backups       int
	maxAge        int // days, 0 means no age limit
	lastCheck     time.Time
	checkInterval time.Duration
}

// NewRotator creates a new log rotator
func NewRotator(filePath string, maxBytes int64, backups int, maxAge int) *Rotator {
	return &Rotator{
		filePath:      filePath,
		maxBytes:      maxBytes,
		backups:       backups,
		maxAge:        maxAge,
		checkInterval: rotatorCheckInterval,
		lastCheck:     time.Now(),
	}
}

// CheckAndRotate checks if rotation is needed and performs it
func (r *Rotator) CheckAndRotate() error {
	now := time.Now()
	if now.Sub(r.lastCheck) < r.checkInterval {
		return nil // Skip check if not enough time has passed
	}
	r.lastCheck = now

	info, err := os.Stat(r.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // File doesn't exist yet, nothing to rotate
		}
		return fmt.Errorf("failed to stat log file: %w", err)
	}

	if info.Size() >= r.maxBytes {
		return r.rotate()
	}

	// Clean up old logs based on maxAge
	if r.maxAge > 0 {
		if err := r.cleanupOldLogs(); err != nil {
			return fmt.Errorf("failed to cleanup old logs: %w", err)
		}
	}

	return nil
}

// rotate performs the log rotation
func (r *Rotator) rotate() error {
	// Rotate existing backups
	for i := r.backups - 1; i >= 1; i-- {
		oldPath := r.getBackupPath(i)
		newPath := r.getBackupPath(i + 1)

		if _, err := os.Stat(oldPath); err == nil {
			if err := os.Rename(oldPath, newPath); err != nil {
				return fmt.Errorf("failed to rotate backup %d: %w", i, err)
			}
		}
	}

	// Move current log to .1
	backup1Path := r.getBackupPath(1)
	if _, err := os.Stat(r.filePath); err == nil {
		if err := os.Rename(r.filePath, backup1Path); err != nil {
			return fmt.Errorf("failed to rotate current log: %w", err)
		}
	}

	// Create new log file
	file, err := os.Create(r.filePath)
	if err != nil {
		return fmt.Errorf("failed to create new log file: %w", err)
	}
	file.Close()

	return nil
}

// getBackupPath returns the path for a backup file with the given number
func (r *Rotator) getBackupPath(num int) string {
	return fmt.Sprintf("%s.%d", r.filePath, num)
}

// cleanupOldLogs removes log files older than maxAge days
func (r *Rotator) cleanupOldLogs() error {
	if r.maxAge <= 0 {
		return nil
	}

	cutoffTime := time.Now().AddDate(0, 0, -r.maxAge)
	dir := filepath.Dir(r.filePath)
	baseName := filepath.Base(r.filePath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read log directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		// Check if this is a backup of our log file
		if name == baseName || strings.HasPrefix(name, baseName+".") {
			info, err := entry.Info()
			if err != nil {
				continue
			}

			// Check if file is older than maxAge
			if info.ModTime().Before(cutoffTime) {
				// Only delete if it's a backup (not the main log file)
				if name != baseName {
					filePath := filepath.Join(dir, name)
					if err := os.Remove(filePath); err != nil {
						// Log error but continue
						continue
					}
				}
			}
		}
	}

	return nil
}

// CleanupOldBackups removes backup files beyond the backup count
func (r *Rotator) CleanupOldBackups() error {
	dir := filepath.Dir(r.filePath)
	baseName := filepath.Base(r.filePath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read log directory: %w", err)
	}

	backupFiles := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if strings.HasPrefix(name, baseName+".") {
			suffix := strings.TrimPrefix(name, baseName+".")
			// Check if suffix is a number
			if isNumeric(suffix) {
				backupFiles = append(backupFiles, name)
			}
		}
	}

	// Sort by backup number (extract number and sort)
	type backupInfo struct {
		name string
		num  int
	}
	backups := make([]backupInfo, 0, len(backupFiles))
	for _, name := range backupFiles {
		suffix := strings.TrimPrefix(name, baseName+".")
		num := parseBackupNumber(suffix)
		if num > 0 {
			backups = append(backups, backupInfo{name: name, num: num})
		}
	}

	// Sort by number descending
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].num > backups[j].num
	})

	// Remove backups beyond the limit
	for i := r.backups; i < len(backups); i++ {
		filePath := filepath.Join(dir, backups[i].name)
		if err := os.Remove(filePath); err != nil {
			// Continue on error
			continue
		}
	}

	return nil
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func parseBackupNumber(s string) int {
	num := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			num = num*10 + int(r-'0')
		} else {
			return 0
		}
	}
	return num
}
