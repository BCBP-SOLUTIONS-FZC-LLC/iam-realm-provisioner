package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveAppEnv_Precedence(t *testing.T) {
	cases := []struct {
		name, appEnv, environment, want string
	}{
		{"APP_ENV wins", "Staging ", "prod", "staging"},
		{"ENVIRONMENT fallback", "", " PROD", "prod"},
		{"blank APP_ENV ignored", "   ", "test", "test"},
		{"neither set defaults to dev", "", "", "dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("APP_ENV", tc.appEnv)
			t.Setenv("ENVIRONMENT", tc.environment)
			assert.Equal(t, tc.want, resolveAppEnv())
		})
	}
}

// gincommon's logger only treats dev/local as development, so test maps
// to dev; every other environment passes through unchanged.
func TestLoggerEnv(t *testing.T) {
	for in, want := range map[string]string{"local": "dev", "dev": "dev", "test": "dev", "staging": "staging", "prod": "prod"} {
		assert.Equal(t, want, loggerEnv(in), in)
	}
}
