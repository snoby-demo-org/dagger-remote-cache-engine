package main

import (
	"testing"
	"time"
)

func TestTransferTimeoutConfiguration(t *testing.T) {
	cfg, _, _, err := loadConfig(envMap(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UploadTimeout != 15*time.Minute {
		t.Errorf("upload default = %s; want 15m", cfg.UploadTimeout)
	}
	env := validEnv()
	env["DCACHE_UPLOAD_TIMEOUT"] = "7m"
	env["DCACHE_REQUEST_TIMEOUT"] = "12s"
	cfg, _, _, err = loadConfig(envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UploadTimeout != 7*time.Minute || cfg.RequestTimeout != 12*time.Second {
		t.Errorf("overrides not applied: upload=%s request=%s", cfg.UploadTimeout, cfg.RequestTimeout)
	}
	for _, key := range []string{"DCACHE_UPLOAD_TIMEOUT", "DCACHE_REQUEST_TIMEOUT"} {
		for _, value := range []string{"bad", "0s", "-1s"} {
			env = validEnv()
			env[key] = value
			if _, _, _, err := loadConfig(envMap(env)); err == nil {
				t.Errorf("%s=%q accepted", key, value)
			}
		}
	}
}
