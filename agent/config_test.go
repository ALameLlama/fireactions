package agent

import "testing"

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{name: "standalone configuration", config: Config{Port: 9001, LogLevel: "info"}},
		{name: "missing port", config: Config{LogLevel: "info"}, wantErr: true},
		{name: "missing log level", config: Config{Port: 9001}, wantErr: true},
		{name: "invalid log level", config: Config{Port: 9001, LogLevel: "invalid"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
