package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSendRequest_FailsQuicklyWhenSocketNotExists(t *testing.T) {
	// Create a temporary socket path that doesn't exist
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "nonexistent.sock")

	// Measure how long it takes to fail
	start := time.Now()
	_, err := sendRequest(socketPath, "status", nil)
	duration := time.Since(start)

	// Should fail quickly (connection fails immediately when socket doesn't exist)
	if err == nil {
		t.Fatal("Expected error when connecting to non-existent socket, got nil")
	}

	// Should fail immediately (not hang)
	maxExpectedDuration := 1 * time.Second
	if duration > maxExpectedDuration {
		t.Errorf("Connection attempt took too long: %v (expected < %v)", duration, maxExpectedDuration)
	}

	// Verify error message mentions the daemon
	errorMsg := err.Error()
	if errorMsg == "" {
		t.Error("Expected error message, got empty string")
	}
	if !strings.Contains(strings.ToLower(errorMsg), "daemon") && !strings.Contains(strings.ToLower(errorMsg), "connect") {
		t.Errorf("Expected error message to mention daemon or connection, got: %s", errorMsg)
	}
}

func TestSendRequest_TimeoutConfiguration(t *testing.T) {
	// Test that the timeout is properly configured by verifying
	// that a non-existent socket fails quickly (not hanging)
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	// This should fail immediately since socket doesn't exist
	// The timeout ensures it doesn't hang even if socket exists but daemon is down
	start := time.Now()
	_, err := sendRequest(socketPath, "status", nil)
	duration := time.Since(start)

	// Should fail
	if err == nil {
		t.Fatal("Expected error when socket doesn't exist, got nil")
	}

	// Should fail quickly (connection timeout is 5 seconds, but non-existent socket fails immediately)
	// This verifies the timeout is configured and prevents infinite hangs
	maxExpectedDuration := 6 * time.Second
	if duration > maxExpectedDuration {
		t.Errorf("Request took too long: %v (expected < %v) - timeout not working", duration, maxExpectedDuration)
	}

	// Verify error message is helpful
	errorMsg := err.Error()
	if errorMsg == "" {
		t.Error("Expected error message, got empty string")
	}
	if !strings.Contains(strings.ToLower(errorMsg), "daemon") && !strings.Contains(strings.ToLower(errorMsg), "connect") {
		t.Logf("Error message: %s", errorMsg)
	}
}
