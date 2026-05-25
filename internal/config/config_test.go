package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadFull(t *testing.T) {
	path := writeConfig(t, `
detection:
  tv_threshold: 5
  duration_tolerance_sec: 90
  min_feature_minutes: 45
  min_extra_minutes: 3

output:
  staging_dir: /mnt/staging
  nas_path: /mnt/nas/movies
  folder_format: "{{.Title}} ({{.Year}})"

notification:
  webhook_url: https://n8n.example.com/webhook/abc
  response_timeout_minutes: 45
  callback_port: 9090

makemkv:
  key: BETA_KEY_ABCDEF
  timeout_minutes: 180
  devices:
    - /dev/sr0
    - /dev/sr1

metadata:
  tmdb_api_key: tmdb-secret
  preferred_language: fra
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Detection.TVThreshold != 5 {
		t.Errorf("TVThreshold = %d, want 5", cfg.Detection.TVThreshold)
	}
	if cfg.Detection.DurationToleranceSec != 90 {
		t.Errorf("DurationToleranceSec = %d, want 90", cfg.Detection.DurationToleranceSec)
	}
	if cfg.Detection.MinFeatureMinutes != 45 {
		t.Errorf("MinFeatureMinutes = %d, want 45", cfg.Detection.MinFeatureMinutes)
	}
	if cfg.Detection.MinExtraMinutes != 3 {
		t.Errorf("MinExtraMinutes = %d, want 3", cfg.Detection.MinExtraMinutes)
	}
	if cfg.Output.StagingDir != "/mnt/staging" {
		t.Errorf("StagingDir = %q", cfg.Output.StagingDir)
	}
	if cfg.Output.NASPath != "/mnt/nas/movies" {
		t.Errorf("NASPath = %q", cfg.Output.NASPath)
	}
	if cfg.Notification.WebhookURL != "https://n8n.example.com/webhook/abc" {
		t.Errorf("WebhookURL = %q", cfg.Notification.WebhookURL)
	}
	if cfg.Notification.ResponseTimeoutMin != 45 {
		t.Errorf("ResponseTimeoutMin = %d, want 45", cfg.Notification.ResponseTimeoutMin)
	}
	if cfg.Notification.CallbackPort != 9090 {
		t.Errorf("CallbackPort = %d, want 9090", cfg.Notification.CallbackPort)
	}
	if cfg.MakeMKV.Key != "BETA_KEY_ABCDEF" {
		t.Errorf("MakeMKV.Key = %q", cfg.MakeMKV.Key)
	}
	if cfg.MakeMKV.TimeoutMinutes != 180 {
		t.Errorf("TimeoutMinutes = %d, want 180", cfg.MakeMKV.TimeoutMinutes)
	}
	if len(cfg.MakeMKV.Devices) != 2 || cfg.MakeMKV.Devices[0] != "/dev/sr0" {
		t.Errorf("Devices = %v", cfg.MakeMKV.Devices)
	}
	if cfg.Metadata.TMDBApiKey != "tmdb-secret" {
		t.Errorf("TMDBApiKey = %q", cfg.Metadata.TMDBApiKey)
	}
	if cfg.Metadata.PreferredLanguage != "fra" {
		t.Errorf("PreferredLanguage = %q", cfg.Metadata.PreferredLanguage)
	}
}

func TestLoadDefaults(t *testing.T) {
	// Minimal config — only required fields; everything else should default.
	path := writeConfig(t, `
output:
  staging_dir: /staging
  nas_path: /output
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Detection.TVThreshold != 3 {
		t.Errorf("default TVThreshold = %d, want 3", cfg.Detection.TVThreshold)
	}
	if cfg.Detection.DurationToleranceSec != 60 {
		t.Errorf("default DurationToleranceSec = %d, want 60", cfg.Detection.DurationToleranceSec)
	}
	if cfg.Detection.MinFeatureMinutes != 40 {
		t.Errorf("default MinFeatureMinutes = %d, want 40", cfg.Detection.MinFeatureMinutes)
	}
	if cfg.Detection.MinExtraMinutes != 2 {
		t.Errorf("default MinExtraMinutes = %d, want 2", cfg.Detection.MinExtraMinutes)
	}
	if cfg.Notification.ResponseTimeoutMin != 30 {
		t.Errorf("default ResponseTimeoutMin = %d, want 30", cfg.Notification.ResponseTimeoutMin)
	}
	if cfg.Notification.CallbackPort != 8090 {
		t.Errorf("default CallbackPort = %d, want 8090", cfg.Notification.CallbackPort)
	}
	if cfg.MakeMKV.TimeoutMinutes != 120 {
		t.Errorf("default TimeoutMinutes = %d, want 120", cfg.MakeMKV.TimeoutMinutes)
	}
	if cfg.Metadata.PreferredLanguage != "eng" {
		t.Errorf("default PreferredLanguage = %q, want \"eng\"", cfg.Metadata.PreferredLanguage)
	}
}

func TestLoadMakeMKVKeyEnvOverride(t *testing.T) {
	path := writeConfig(t, `
makemkv:
  key: from-file
`)
	t.Setenv("MAKEMKV_KEY", "from-env")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MakeMKV.Key != "from-env" {
		t.Errorf("MakeMKV.Key = %q, want \"from-env\" (env var should win)", cfg.MakeMKV.Key)
	}
}

func TestLoadEnvKeyWinsOverEmptyFileKey(t *testing.T) {
	path := writeConfig(t, `makemkv: {}`)
	t.Setenv("MAKEMKV_KEY", "env-only-key")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MakeMKV.Key != "env-only-key" {
		t.Errorf("MakeMKV.Key = %q, want \"env-only-key\"", cfg.MakeMKV.Key)
	}
}

func TestLoadFileKeyUsedWhenEnvAbsent(t *testing.T) {
	path := writeConfig(t, `
makemkv:
  key: file-key
`)
	// Ensure the env var is not set.
	os.Unsetenv("MAKEMKV_KEY")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MakeMKV.Key != "file-key" {
		t.Errorf("MakeMKV.Key = %q, want \"file-key\"", cfg.MakeMKV.Key)
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	path := writeConfig(t, `detection: [this is not: valid: yaml}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}
