package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	c := Load([]string{}, func(string) string { return "" })
	if c.Addr != ":8080" {
		t.Errorf("addr = %q, want :8080", c.Addr)
	}
	if c.Embedded {
		t.Errorf("embedded should default false")
	}
}

func TestEnvOverridesFlagFallback(t *testing.T) {
	env := map[string]string{"SUBSTRATE_DATABASE_URL": "postgres://x"}
	c := Load([]string{"-addr", ":9000"}, func(k string) string { return env[k] })
	if c.Addr != ":9000" {
		t.Errorf("addr = %q, want :9000", c.Addr)
	}
	if c.DatabaseURL != "postgres://x" {
		t.Errorf("db url = %q", c.DatabaseURL)
	}
}
