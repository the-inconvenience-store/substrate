package config

import "flag"

// Config holds runtime configuration.
type Config struct {
	Addr        string
	DatabaseURL string
	Embedded    bool
	LogLevel    string
	AdminToken  string
}

// Load parses flags (args, without program name) with environment fallback.
// getenv lets tests inject the environment.
func Load(args []string, getenv func(string) string) Config {
	fs := flag.NewFlagSet("substrate", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dbURL := fs.String("database-url", "", "postgres DSN (external mode)")
	embedded := fs.Bool("embedded", false, "run an embedded postgres")
	logLevel := fs.String("log-level", "info", "log level")
	adminToken := fs.String("admin-token", "", "bootstrap admin token")
	_ = fs.Parse(args)

	c := Config{
		Addr:        *addr,
		DatabaseURL: *dbURL,
		Embedded:    *embedded,
		LogLevel:    *logLevel,
		AdminToken:  *adminToken,
	}
	if v := getenv("SUBSTRATE_DATABASE_URL"); v != "" && c.DatabaseURL == "" {
		c.DatabaseURL = v
	}
	if getenv("SUBSTRATE_EMBEDDED") == "true" {
		c.Embedded = true
	}
	if v := getenv("SUBSTRATE_ADMIN_TOKEN"); v != "" && c.AdminToken == "" {
		c.AdminToken = v
	}
	return c
}
