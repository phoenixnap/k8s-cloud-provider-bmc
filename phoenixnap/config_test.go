package phoenixnap

import (
	"strings"
	"testing"
)

func TestGetConfig(t *testing.T) {
	tests := []struct {
		testName string
		file     *string
		env      map[string]string
		check    func(Config) string
		err      string
	}{
		{"nil config with env", nil, map[string]string{clientIDName: "id", clientSecretName: "secret"}, func(c Config) string {
			if c.ClientID != "id" || c.ClientSecret != "secret" {
				return "client credentials not read from env"
			}
			return ""
		}, ""},
		{"empty config with env", strPtr("  \n"), map[string]string{clientIDName: "id", clientSecretName: "secret"}, nil, ""},
		{"missing credentials", strPtr(`{}`), nil, nil, "environment variable \"PNAP_CLIENT_ID\""},
		{"invalid json", strPtr(`{"clientID": "id",}`), nil, nil, "failed to process json"},
		{"file fields", strPtr(`{"clientID":"id","clientSecret":"s","serviceNodeSelector":"lb=true","annotationIPLocation":"x/loc","base-url":"https://example.com"}`), nil, func(c Config) string {
			if c.ServiceNodeSelector != "lb=true" {
				return "serviceNodeSelector not read from file: " + c.ServiceNodeSelector
			}
			if c.AnnotationIPLocation != "x/loc" {
				return "annotationIPLocation not read from file: " + c.AnnotationIPLocation
			}
			if c.BaseURL == nil || *c.BaseURL != "https://example.com" {
				return "base-url not read from file"
			}
			return ""
		}, ""},
		{"env overrides file", strPtr(`{"clientID":"id","clientSecret":"s","serviceNodeSelector":"lb=true","location":"ASH"}`), map[string]string{envVarServiceNodeSelector: "lb=false", locationName: "PHX"}, func(c Config) string {
			if c.ServiceNodeSelector != "lb=false" {
				return "serviceNodeSelector env did not override file: " + c.ServiceNodeSelector
			}
			if c.Location != "PHX" {
				return "location env did not override file: " + c.Location
			}
			if c.AnnotationIPLocation != DefaultAnnotationIPLocation {
				return "annotationIPLocation default not applied: " + c.AnnotationIPLocation
			}
			return ""
		}, ""},
		{"invalid api server port", strPtr(`{"clientID":"id","clientSecret":"s"}`), map[string]string{envVarAPIServerPort: "abc"}, nil, "env var PNAP_API_SERVER_PORT must be a number"},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			for _, name := range []string{clientIDName, clientSecretName, locationName, loadBalancerSettingName, envVarAnnotationIPLocation, envVarAPIServerPort, envVarServiceNodeSelector} {
				t.Setenv(name, tt.env[name])
			}
			var (
				c   Config
				err error
			)
			if tt.file == nil {
				c, err = getConfig(nil)
			} else {
				c, err = getConfig(strings.NewReader(*tt.file))
			}
			switch {
			case tt.err == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Fatalf("expected error containing %q, got %v", tt.err, err)
			}
			if tt.check != nil {
				if msg := tt.check(c); msg != "" {
					t.Error(msg)
				}
			}
		})
	}
}

func strPtr(s string) *string {
	return &s
}
