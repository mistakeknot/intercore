package main

import (
	"testing"

	"github.com/mistakeknot/intercore/internal/publish"
)

func TestPublishDoctorExitCode(t *testing.T) {
	tests := []struct {
		name     string
		findings []publish.Finding
		want     int
	}{
		{name: "healthy", want: 0},
		{name: "warning only", findings: []publish.Finding{{Severity: "warning"}}, want: 0},
		{name: "error", findings: []publish.Finding{{Severity: "error"}}, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := publishDoctorExitCode(tt.findings); got != tt.want {
				t.Fatalf("publishDoctorExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}
