package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Detection    DetectionConfig    `yaml:"detection"`
	Output       OutputConfig       `yaml:"output"`
	Notification NotificationConfig `yaml:"notification"`
	Server       ServerConfig       `yaml:"server"`
	MakeMKV      MakeMKVConfig      `yaml:"makemkv"`
	Metadata     MetadataConfig     `yaml:"metadata"`
}

type DetectionConfig struct {
	TVThreshold          int `yaml:"tv_threshold"`
	DurationToleranceSec int `yaml:"duration_tolerance_sec"`
	MinFeatureMinutes    int `yaml:"min_feature_minutes"`
	MinExtraMinutes      int `yaml:"min_extra_minutes"`
}

type OutputConfig struct {
	StagingDir   string `yaml:"staging_dir"`
	NASPath      string `yaml:"nas_path"`
	FolderFormat string `yaml:"folder_format"`
}

type NotificationConfig struct {
	WebhookURL         string `yaml:"webhook_url"`
	ResponseTimeoutMin int    `yaml:"response_timeout_minutes"`
	CallbackPort       int    `yaml:"callback_port"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type MakeMKVConfig struct {
	Key            string   `yaml:"key"`
	TimeoutMinutes int      `yaml:"timeout_minutes"`
	CacheMB        int      `yaml:"cache_mb"`
	Devices        []string `yaml:"devices"`
}

type MetadataConfig struct {
	TMDBApiKey        string `yaml:"tmdb_api_key"`
	OMDbApiKey        string `yaml:"omdb_api_key"`
	PreferredLanguage string `yaml:"preferred_language"`
}

// Load reads path, applies defaults for any missing fields, then overrides
// MakeMKV.Key from the MAKEMKV_KEY environment variable if set.
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}

	// MAKEMKV_KEY env var always wins over the config file value.
	if key := os.Getenv("MAKEMKV_KEY"); key != "" {
		cfg.MakeMKV.Key = key
	}

	return &cfg, nil
}

// Defaults returns a Config populated with every built-in default value.
// Useful when no config.yaml is available (e.g. first run, unit tests).
// MAKEMKV_KEY is still applied from the environment if set.
func Defaults() *Config {
	cfg := defaults()
	if key := os.Getenv("MAKEMKV_KEY"); key != "" {
		cfg.MakeMKV.Key = key
	}
	return &cfg
}

func defaults() Config {
	return Config{
		Detection: DetectionConfig{
			TVThreshold:          3,
			DurationToleranceSec: 60,
			MinFeatureMinutes:    40,
			MinExtraMinutes:      2,
		},
		Notification: NotificationConfig{
			ResponseTimeoutMin: 30,
			CallbackPort:       8090,
		},
		Server: ServerConfig{
			Port: 8080,
		},
		MakeMKV: MakeMKVConfig{
			TimeoutMinutes: 120,
			CacheMB:        256,
		},
		Metadata: MetadataConfig{
			PreferredLanguage: "eng",
		},
	}
}
