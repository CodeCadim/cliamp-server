package config

import (
	"strings"
	"testing"
)

func TestValidateMinimumStreamBuffer(t *testing.T) {
	cfg := Defaults()
	cfg.Stations = map[string]StationConfig{
		"test": {Path: t.TempDir()},
	}
	cfg.Stream.BufferSize = minBufferSizeKB - 1

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "buffer_size") {
		t.Fatalf("Validate() error = %v, want buffer_size error", err)
	}

	cfg.Stream.BufferSize = minBufferSizeKB
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() at minimum buffer size: %v", err)
	}
}

func TestNeedsFFmpeg(t *testing.T) {
	tests := []struct {
		name     string
		stations map[string]StationConfig
		want     bool
	}{
		{"all passthrough", map[string]StationConfig{"a": {Passthrough: true}, "b": {Passthrough: true}}, false},
		{"one transcoded", map[string]StationConfig{"a": {Passthrough: true}, "b": {}}, true},
		{"passthrough with intro", map[string]StationConfig{"a": {Passthrough: true, IntroFile: "/i.mp3"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Stations: tt.stations}
			if got := cfg.NeedsFFmpeg(); got != tt.want {
				t.Errorf("NeedsFFmpeg() = %v, want %v", got, tt.want)
			}
		})
	}
}
