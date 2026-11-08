package main

import (
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		"DCACHE_TOKEN":       "0123456789abcdef",
		"DCACHE_HMAC_SECRET": "0123456789abcdef0123456789abcdef",
		"S3_ENDPOINT":        "http://127.0.0.1:9000",
		"S3_BUCKET":          "cache",
		"S3_ACCESS_KEY":      "access",
		"S3_SECRET_KEY":      "secret",
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, opts, listen, err := loadConfig(envMap(validEnv()))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if listen != ":8080" {
		t.Errorf("listen = %q, want :8080", listen)
	}
	if cfg.DBPath != "./state/cache.db" {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}
	if cfg.Pool != "shared" {
		t.Errorf("Pool = %q", cfg.Pool)
	}
	if cfg.EngineVersion != "v1.0.0-beta.25" {
		t.Errorf("EngineVersion = %q", cfg.EngineVersion)
	}
	if cfg.CheckpointInterval != 10*time.Minute {
		t.Errorf("CheckpointInterval = %v", cfg.CheckpointInterval)
	}
	if cfg.RenewTimeout != 1500*time.Millisecond {
		t.Errorf("RenewTimeout = %v", cfg.RenewTimeout)
	}
	if cfg.MaxExports != 8 || cfg.MaxVerifiers != 16 || cfg.WarmRoots != 64 {
		t.Errorf("limits = %d/%d/%d", cfg.MaxExports, cfg.MaxVerifiers, cfg.WarmRoots)
	}
	if opts.Region != "us-east-1" {
		t.Errorf("Region = %q", opts.Region)
	}
	if opts.URLTTL != 15*time.Minute {
		t.Errorf("URLTTL = %v", opts.URLTTL)
	}
	if opts.Pool != "shared" {
		t.Errorf("storage Pool = %q", opts.Pool)
	}
	if opts.PublicEndpoint != "" {
		t.Errorf("PublicEndpoint = %q", opts.PublicEndpoint)
	}
}

func TestLoadConfigRequiredMissing(t *testing.T) {
	for _, key := range []string{
		"DCACHE_TOKEN", "DCACHE_HMAC_SECRET", "S3_ENDPOINT",
		"S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY",
	} {
		t.Run(key, func(t *testing.T) {
			env := validEnv()
			delete(env, key)
			_, _, _, err := loadConfig(envMap(env))
			if err == nil {
				t.Fatalf("expected error for missing %s", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name %s", err, key)
			}
		})
	}
}

func TestLoadConfigBlankRequired(t *testing.T) {
	env := validEnv()
	env["S3_BUCKET"] = "   "
	if _, _, _, err := loadConfig(envMap(env)); err == nil {
		t.Fatal("expected error for blank bucket")
	}
}

func TestLoadConfigShortSecrets(t *testing.T) {
	env := validEnv()
	env["DCACHE_TOKEN"] = "short"
	if _, _, _, err := loadConfig(envMap(env)); err == nil {
		t.Fatal("expected error for short token")
	}
	env = validEnv()
	env["DCACHE_HMAC_SECRET"] = "short"
	if _, _, _, err := loadConfig(envMap(env)); err == nil {
		t.Fatal("expected error for short hmac secret")
	}
}

func TestLoadConfigMalformed(t *testing.T) {
	cases := map[string]string{
		"DCACHE_MAX_EXPORTS":         "abc",
		"DCACHE_MAX_VERIFIERS":       "-1",
		"DCACHE_WARM_ROOTS":          "0",
		"DCACHE_URL_TTL":             "nope",
		"DCACHE_CHECKPOINT_INTERVAL": "-5m",
		"DCACHE_RENEW_TIMEOUT":       "0s",
	}
	for key, val := range cases {
		t.Run(key, func(t *testing.T) {
			env := validEnv()
			env[key] = val
			if _, _, _, err := loadConfig(envMap(env)); err == nil {
				t.Fatalf("expected error for %s=%s", key, val)
			}
		})
	}
}

func TestLoadConfigBadEndpoint(t *testing.T) {
	env := validEnv()
	env["S3_ENDPOINT"] = "not-a-url"
	if _, _, _, err := loadConfig(envMap(env)); err == nil {
		t.Fatal("expected error for bad endpoint")
	}
	env = validEnv()
	env["S3_PUBLIC_ENDPOINT"] = "ftp://example.com"
	if _, _, _, err := loadConfig(envMap(env)); err == nil {
		t.Fatal("expected error for bad public endpoint")
	}
}

func TestLoadConfigSecretNotLeaked(t *testing.T) {
	env := validEnv()
	env["DCACHE_HMAC_SECRET"] = "supersecretvalue-that-must-not-leak-123456"
	env["S3_SECRET_KEY"] = "s3secretvalue-that-must-not-leak"
	env["DCACHE_MAX_EXPORTS"] = "bad"
	_, _, _, err := loadConfig(envMap(env))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, secret := range []string{env["DCACHE_HMAC_SECRET"], env["S3_SECRET_KEY"], env["DCACHE_TOKEN"]} {
		if strings.Contains(msg, secret) {
			t.Errorf("error leaks secret %q", secret)
		}
	}
}

func TestLoadConfigExplicitOverrides(t *testing.T) {
	env := validEnv()
	env["DCACHE_LISTEN"] = "127.0.0.1:9999"
	env["DCACHE_DB"] = "/tmp/custom.db"
	env["DCACHE_POOL"] = "team-a"
	env["DCACHE_URL_TTL"] = "30m"
	env["DCACHE_ENGINE_VERSION"] = "v1.0.0-beta.25+commit.abc"
	env["DCACHE_CHECKPOINT_INTERVAL"] = "2m"
	env["DCACHE_MAX_EXPORTS"] = "3"
	env["DCACHE_MAX_VERIFIERS"] = "4"
	env["DCACHE_WARM_ROOTS"] = "5"
	env["DCACHE_RENEW_TIMEOUT"] = "2s"
	env["S3_PUBLIC_ENDPOINT"] = "https://public.example.com"
	env["S3_REGION"] = "eu-west-1"
	cfg, opts, listen, err := loadConfig(envMap(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q", listen)
	}
	if cfg.DBPath != "/tmp/custom.db" || cfg.Pool != "team-a" {
		t.Errorf("cfg paths = %q/%q", cfg.DBPath, cfg.Pool)
	}
	if cfg.EngineVersion != "v1.0.0-beta.25+commit.abc" {
		t.Errorf("EngineVersion = %q", cfg.EngineVersion)
	}
	if cfg.CheckpointInterval != 2*time.Minute || cfg.RenewTimeout != 2*time.Second {
		t.Errorf("durations = %v/%v", cfg.CheckpointInterval, cfg.RenewTimeout)
	}
	if cfg.MaxExports != 3 || cfg.MaxVerifiers != 4 || cfg.WarmRoots != 5 {
		t.Errorf("limits = %d/%d/%d", cfg.MaxExports, cfg.MaxVerifiers, cfg.WarmRoots)
	}
	if opts.URLTTL != 30*time.Minute || opts.Region != "eu-west-1" {
		t.Errorf("opts = %v/%q", opts.URLTTL, opts.Region)
	}
	if opts.PublicEndpoint != "https://public.example.com" {
		t.Errorf("PublicEndpoint = %q", opts.PublicEndpoint)
	}
	if opts.Pool != "team-a" {
		t.Errorf("storage Pool = %q", opts.Pool)
	}
}

func TestLoadConfigInvalidPool(t *testing.T) {
	env := validEnv()
	env["DCACHE_POOL"] = "../escape"
	if _, _, _, err := loadConfig(envMap(env)); err == nil {
		t.Fatal("expected error for invalid pool")
	}
}

func TestLoadConfigInvalidListen(t *testing.T) {
	env := validEnv()
	env["DCACHE_LISTEN"] = "not-an-address"
	if _, _, _, err := loadConfig(envMap(env)); err == nil {
		t.Fatal("expected error for invalid listen address")
	}
}
