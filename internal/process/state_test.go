package process

import (
	"testing"
)

func TestState_IsRunning(t *testing.T) {
	tests := []struct {
		state    State
		expected bool
	}{
		{StateRunning, true},
		{StateStopped, false},
		{StateStarting, false},
		{StateExited, false},
		{StateFatal, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if tt.state.IsRunning() != tt.expected {
				t.Errorf("IsRunning() for %s = %v, expected %v", tt.state, tt.state.IsRunning(), tt.expected)
			}
		})
	}
}

func TestState_IsStopped(t *testing.T) {
	tests := []struct {
		state    State
		expected bool
	}{
		{StateStopped, true},
		{StateExited, true},
		{StateFatal, true},
		{StateRunning, false},
		{StateStarting, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if tt.state.IsStopped() != tt.expected {
				t.Errorf("IsStopped() for %s = %v, expected %v", tt.state, tt.state.IsStopped(), tt.expected)
			}
		})
	}
}

func TestState_String(t *testing.T) {
	if StateRunning.String() != "RUNNING" {
		t.Errorf("Expected 'RUNNING', got '%s'", StateRunning.String())
	}
}
