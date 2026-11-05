package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HTTP.Addr != "127.0.0.1:8480" {
		t.Errorf("default addr = %q, want loopback-only 127.0.0.1:8480", c.HTTP.Addr)
	}
	if c.LogFormat != "json" || c.LogLevel != "info" {
		t.Errorf("default logging = %s/%s", c.LogLevel, c.LogFormat)
	}
	if c.DatabaseURL != "" {
		t.Errorf("database must be unset by default")
	}
	if c.ShutdownTimeout != 20*time.Second {
		t.Errorf("shutdown timeout = %s", c.ShutdownTimeout)
	}
}

func TestOverrides(t *testing.T) {
	//nolint:gosec // G101: synthetic credential (user u, password p, nothing listens); verifies the URL override is accepted.
	c, err := Load(env(map[string]string{
		EnvLogLevel:        "DEBUG",
		EnvLogFormat:       "text",
		EnvHTTPAddr:        "127.0.0.1:0",
		EnvShutdownTimeout: "3s",
		EnvDatabaseURL:     "postgres://u:p@localhost:5432/db?sslmode=disable",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LogLevel != "debug" || c.LogFormat != "text" || c.HTTP.Addr != "127.0.0.1:0" || c.ShutdownTimeout != 3*time.Second {
		t.Errorf("overrides not applied: %+v", c)
	}
}

func TestAllErrorsReportedTogether(t *testing.T) {
	//nolint:gosec // G101: synthetic credential (host h); verifies the error message does not echo the password.
	_, err := Load(env(map[string]string{
		EnvLogLevel:        "loud",
		EnvLogFormat:       "xml",
		EnvHTTPAddr:        "nope",
		EnvShutdownTimeout: "-1s",
		EnvReadTimeout:     "soon",
		EnvDatabaseURL:     "mysql://u:secret@h/db",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{EnvLogLevel, EnvLogFormat, EnvHTTPAddr, EnvShutdownTimeout, EnvReadTimeout, EnvDatabaseURL} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %s:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "secret") {
		t.Errorf("error leaks database password:\n%s", msg)
	}
}

func TestRedactURL(t *testing.T) {
	//nolint:gosec // G101: synthetic credentials (host h); these are the inputs whose passwords RedactURL must mask.
	cases := map[string]string{
		"":                               "",
		"postgres://u:hunter2@h:5432/db": "postgres://u:xxxxx@h:5432/db",
		"postgres://u@h/db":              "postgres://u@h/db",
		"postgres://u:p%40ss@h/db?sslmode=disable": "postgres://u:xxxxx@h/db?sslmode=disable",
		"::not a url": "[redacted]",
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// All values below are synthetic (host h, no server); each case lists the
// secret fragments that must not survive redaction.
func TestRedactURLQueryCredentials(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		secrets []string
	}{
		{
			name:    "password parameter",
			in:      "postgres://u@h/db?password=s3cretA&sslmode=disable",
			want:    "postgres://u@h/db?password=xxxxx&sslmode=disable",
			secrets: []string{"s3cretA"},
		},
		{
			name:    "sslpassword parameter",
			in:      "postgres://u@h/db?sslmode=verify-full&sslpassword=keyPassB",
			want:    "postgres://u@h/db?sslmode=verify-full&sslpassword=xxxxx",
			secrets: []string{"keyPassB"},
		},
		{ //nolint:gosec // G101: synthetic credential (host h, no server); the case asserts it is redacted.
			name:    "userinfo and both parameters",
			in:      "postgres://u:infoPassC@h/db?password=qPassD&sslpassword=sslPassE",
			want:    "postgres://u:xxxxx@h/db?password=xxxxx&sslpassword=xxxxx",
			secrets: []string{"infoPassC", "qPassD", "sslPassE"},
		},
		{
			name:    "URL-encoded value",
			in:      "postgres://u@h/db?password=p%40ss%26w%3Drd%20F&application_name=svc",
			want:    "postgres://u@h/db?password=xxxxx&application_name=svc",
			secrets: []string{"p%40ss", "p@ss", "w%3Drd", "rd%20F"},
		},
		{
			name:    "URL-encoded parameter name",
			in:      "postgres://u@h/db?pass%77ord=encKeyG&ssl%70assword=encKeyH",
			want:    "postgres://u@h/db?pass%77ord=xxxxx&ssl%70assword=xxxxx",
			secrets: []string{"encKeyG", "encKeyH"},
		},
		{
			name:    "case-insensitive name",
			in:      "postgres://u@h/db?PASSWORD=upperI",
			want:    "postgres://u@h/db?PASSWORD=xxxxx",
			secrets: []string{"upperI"},
		},
		{
			name:    "repeated parameter",
			in:      "postgres://u@h/db?password=firstJ&password=secondK",
			want:    "postgres://u@h/db?password=xxxxx&password=xxxxx",
			secrets: []string{"firstJ", "secondK"},
		},
		{
			name:    "semicolon separator",
			in:      "postgres://u@h/db?sslmode=disable;password=semiL",
			want:    "postgres://u@h/db?sslmode=disable;password=xxxxx",
			secrets: []string{"semiL"},
		},
		{
			name:    "undecodable parameter name is hidden",
			in:      "postgres://u@h/db?%zzM=hiddenN&sslmode=disable",
			want:    "postgres://u@h/db?xxxxx&sslmode=disable",
			secrets: []string{"hiddenN"},
		},
		{
			name: "unrelated parameters keep order and encoding",
			in:   "postgres://u@h/db?sslmode=disable&application_name=a%20b&connect_timeout=5&target_session_attrs=read-write",
			want: "postgres://u@h/db?sslmode=disable&application_name=a%20b&connect_timeout=5&target_session_attrs=read-write",
		},
		{
			name: "similar names are not secrets",
			in:   "postgres://u@h/db?passwordless=1&password_hint=none",
			want: "postgres://u@h/db?passwordless=1&password_hint=none",
		},
		{
			name: "parameter without value",
			in:   "postgres://u@h/db?password&sslmode=disable",
			want: "postgres://u@h/db?password&sslmode=disable",
		},
		{
			name:    "key/value DSN is fully redacted",
			in:      "host=h user=u password=dsnPassO",
			want:    "[redacted]",
			secrets: []string{"dsnPassO"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			if got != tc.want {
				t.Errorf("RedactURL(%q)\n got  %q\n want %q", tc.in, got, tc.want)
			}
			for _, s := range tc.secrets {
				if strings.Contains(got, s) {
					t.Errorf("redacted output still contains %q: %q", s, got)
				}
			}
		})
	}
}

func TestSummaryRedactsQueryCredentials(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvDatabaseURL: "postgres://u@h/db?sslmode=disable&password=querySecretP&sslpassword=sslSecretQ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Summary()["database_url"]
	if strings.Contains(got, "querySecretP") || strings.Contains(got, "sslSecretQ") {
		t.Errorf("summary leaks query credential: %s", got)
	}
	if !strings.Contains(got, "sslmode=disable") {
		t.Errorf("summary dropped an unrelated parameter: %s", got)
	}
}

func TestLoadKeepsDatabaseURLUnchanged(t *testing.T) {
	// Redaction is display-only: the DSN given to pgx must be byte-identical.
	const dsn = "postgres://u@h/db?password=p%40ssR&sslmode=disable"
	c, err := Load(env(map[string]string{EnvDatabaseURL: dsn}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DatabaseURL != dsn {
		t.Errorf("DatabaseURL = %q, want unchanged %q", c.DatabaseURL, dsn)
	}
}

func TestSummaryNeverContainsPassword(t *testing.T) {
	//nolint:gosec // G101: synthetic credential (host h); the test asserts this password never appears in Summary.
	c, err := Load(env(map[string]string{EnvDatabaseURL: "postgres://u:topsecret@h/db"}))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range c.Summary() {
		if strings.Contains(v, "topsecret") {
			t.Errorf("summary %s leaks password: %s", k, v)
		}
	}
}
