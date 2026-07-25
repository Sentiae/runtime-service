package config

import "testing"

// TestTelemetryEnabledBinding pins the APP_TELEMETRY_ENABLED control to the
// struct field. The key was set in the fleet-host config-as-code long before it
// was bound, so it read as a control while telemetry stayed unconditionally on.
func TestTelemetryEnabledBinding(t *testing.T) {
	tests := []struct {
		name string
		env  string // "" ⇒ leave unset, exercising the default
		want bool
	}{
		{name: "default is enabled", env: "", want: true},
		{name: "explicit false disables", env: "false", want: false},
		{name: "explicit true enables", env: "true", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("APP_TELEMETRY_ENABLED", tt.env)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if cfg.Telemetry.Enabled != tt.want {
				t.Fatalf("Telemetry.Enabled = %v, want %v", cfg.Telemetry.Enabled, tt.want)
			}
		})
	}
}
