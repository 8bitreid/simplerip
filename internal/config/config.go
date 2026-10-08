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
	Database     DatabaseConfig     `yaml:"database"`
}

type DatabaseConfig struct {
	URL string `yaml:"url"`
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

	// DiscordWebhookURL receives pipeline notifications. Prefer the
	// DISCORD_WEBHOOK_URL env var, which always overrides this value, so the
	// secret stays out of config files.
	DiscordWebhookURL string `yaml:"discord_webhook_url"`
	// UIURL is the address of the SimpleRip web UI, linked from notifications.
	// Overridden by SIMPLERIP_UI_URL.
	UIURL  string             `yaml:"ui_url"`
	Events NotificationEvents `yaml:"events"`
}

// NotificationEvents toggles each notification type. All default to on.
type NotificationEvents struct {
	NeedsInput       bool `yaml:"needs_input"`
	MultiTitle       bool `yaml:"multi_title"`
	Complete         bool `yaml:"complete"`
	Failed           bool `yaml:"failed"`
	DurationMismatch bool `yaml:"duration_mismatch"`
}

type ServerConfig struct {
	Port int `yaml:"port"`
}

type MakeMKVConfig struct {
	Key                       string   `yaml:"key"`
	TimeoutMinutes            int      `yaml:"timeout_minutes"`
	BatchAnalyzeBudgetMinutes int      `yaml:"batch_analyze_budget_minutes"`
	BatchSaveBudgetMinutes    int      `yaml:"batch_save_budget_minutes"`
	CacheMB                   int      `yaml:"cache_mb"`
	ReadErrorLimit            int      `yaml:"read_error_limit"`
	NoProgressMin             int      `yaml:"no_progress_minutes"`
	MaxRipRetries             int      `yaml:"max_rip_retries"`
	Devices                   []string `yaml:"devices"`
}

type MetadataConfig struct {
	TMDBApiKey        string `yaml:"tmdb_api_key"`
	TMDBAccessToken   string `yaml:"tmdb_access_token"`
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

	applyEnv(&cfg)
	return &cfg, nil
}

// Defaults returns a Config populated with every built-in default value.
// Useful when no config.yaml is available (e.g. first run, unit tests).
// MAKEMKV_KEY is still applied from the environment if set.
func Defaults() *Config {
	cfg := defaults()
	applyEnv(&cfg)
	return &cfg
}

// applyEnv lets environment variables override secrets and deployment-specific
// values in the config file.
func applyEnv(cfg *Config) {
	if key := os.Getenv("MAKEMKV_KEY"); key != "" {
		cfg.MakeMKV.Key = key
	}
	if v := os.Getenv("DISCORD_WEBHOOK_URL"); v != "" {
		cfg.Notification.DiscordWebhookURL = v
	}
	if v := os.Getenv("SIMPLERIP_UI_URL"); v != "" {
		cfg.Notification.UIURL = v
	}
	if v := os.Getenv("TMDB_ACCESS_TOKEN"); v != "" {
		cfg.Metadata.TMDBAccessToken = v
	}
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
			Events: NotificationEvents{
				NeedsInput:       true,
				MultiTitle:       true,
				Complete:         true,
				Failed:           true,
				DurationMismatch: true,
			},
		},
		Server: ServerConfig{
			Port: 8080,
		},
		MakeMKV: MakeMKVConfig{
			TimeoutMinutes:            120,
			BatchAnalyzeBudgetMinutes: 45,
			BatchSaveBudgetMinutes:    10,
			CacheMB:                   0, // 0 = auto-select by disc type (DVD→512, Blu-ray→1024)
			ReadErrorLimit:            100,
			NoProgressMin:             15,
			MaxRipRetries:             1,
		},
		Metadata: MetadataConfig{
			PreferredLanguage: "eng",
		},
	}
}
