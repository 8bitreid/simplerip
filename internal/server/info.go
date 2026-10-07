package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/8bitreid/simplerip/internal/disc"
)

const (
	infoProbeTimeout = 2 * time.Second
	infoCacheTTL     = 30 * time.Second
)

var rsyncCredentialPattern = regexp.MustCompile(`(^|//)[^/@]+@`)
var (
	ffprobeVersionPattern = regexp.MustCompile(`^ffprobe version\s+(\S+)`)
	makeMKVVersionPattern = regexp.MustCompile(`MakeMKV v[\d.]+`)
)

type toolVersion struct {
	Version *string `json:"version"`
	Error   string  `json:"error,omitempty"`
}

type toolVersions struct {
	MakeMKV toolVersion `json:"makemkvcon"`
	FFprobe toolVersion `json:"ffprobe"`
}

type infoCache struct {
	mu                sync.Mutex
	tools             toolVersions
	deliveryReachable bool
	deliveryExpiresAt time.Time
}

type infoResponse struct {
	Name           string           `json:"name"`
	Version        string           `json:"version"`
	Commit         string           `json:"commit"`
	BuildDate      string           `json:"build_date"`
	GoVersion      string           `json:"go_version"`
	Hostname       string           `json:"hostname"`
	StartedAt      time.Time        `json:"started_at"`
	UptimeSeconds  int64            `json:"uptime_seconds"`
	DrivesDetected int              `json:"drives_detected"`
	Tools          toolVersions     `json:"tools"`
	Delivery       deliveryInfo     `json:"delivery"`
	Staging        stagingInfo      `json:"staging"`
	Integrations   integrationsInfo `json:"integrations"`
	Stats          ripStats         `json:"stats"`
}

type deliveryInfo struct {
	Destination string `json:"destination"`
	Reachable   bool   `json:"reachable"`
}

type stagingInfo struct {
	Path       string  `json:"path"`
	FreeBytes  *uint64 `json:"free_bytes"`
	TotalBytes *uint64 `json:"total_bytes"`
	Error      string  `json:"error,omitempty"`
}

type integrationsInfo struct {
	TMDBConfigured    bool `json:"tmdb_configured"`
	DiscordConfigured bool `json:"discord_configured"`
}

type ripStats struct {
	Done      int64 `json:"done"`
	Error     int64 `json:"error"`
	Cancelled int64 `json:"cancelled"`
}

func probeToolVersions() toolVersions {
	ctx, cancel := context.WithTimeout(context.Background(), infoProbeTimeout)
	defer cancel()
	return toolVersions{
		MakeMKV: probeMakeMKVVersion(ctx),
		FFprobe: probeFFprobeVersion(ctx),
	}
}

func probeMakeMKVVersion(ctx context.Context) toolVersion {
	output, err := exec.CommandContext(ctx, "makemkvcon").CombinedOutput()
	if version := parseMakeMKVVersion(output); version != "" {
		return toolVersion{Version: &version}
	}
	if err != nil {
		return toolVersion{Error: fmt.Sprintf("makemkvcon: %v", err)}
	}
	return toolVersion{Error: "makemkvcon returned no version information"}
}

func parseMakeMKVVersion(output []byte) string {
	return makeMKVVersionPattern.FindString(string(output))
}

func probeFFprobeVersion(ctx context.Context) toolVersion {
	output, err := exec.CommandContext(ctx, "ffprobe", "-version").CombinedOutput()
	if err != nil {
		return toolVersion{Error: fmt.Sprintf("ffprobe: %v", err)}
	}
	version := parseFFprobeVersion(output)
	if version == "" {
		return toolVersion{Error: "ffprobe returned no version information"}
	}
	return toolVersion{Version: &version}
}

func parseFFprobeVersion(output []byte) string {
	firstLine, _, _ := strings.Cut(string(output), "\n")
	match := ffprobeVersionPattern.FindStringSubmatch(strings.TrimSpace(firstLine))
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func (s *Server) refreshDeliveryReachability(now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), infoProbeTimeout)
	defer cancel()
	s.updateDeliveryReachability(ctx, now)
}

func (s *Server) updateDeliveryReachability(ctx context.Context, now time.Time) {
	s.infoCache.mu.Lock()
	defer s.infoCache.mu.Unlock()
	if now.Before(s.infoCache.deliveryExpiresAt) {
		return
	}
	s.infoCache.deliveryReachable = probeDelivery(s.cfg.Output.NASPath, ctx)
	s.infoCache.deliveryExpiresAt = now.Add(infoCacheTTL)
}

func (s *Server) cachedDeliveryReachability(ctx context.Context, now time.Time) bool {
	s.infoCache.mu.Lock()
	defer s.infoCache.mu.Unlock()
	if !now.Before(s.infoCache.deliveryExpiresAt) {
		probeCtx, cancel := context.WithTimeout(ctx, infoProbeTimeout)
		defer cancel()
		s.infoCache.deliveryReachable = probeDelivery(s.cfg.Output.NASPath, probeCtx)
		s.infoCache.deliveryExpiresAt = now.Add(infoCacheTTL)
	}
	return s.infoCache.deliveryReachable
}

func probeDelivery(destination string, ctx context.Context) bool {
	if strings.TrimSpace(destination) == "" {
		return false
	}
	target := strings.TrimRight(destination, "/") + "/"
	return exec.CommandContext(ctx, "rsync", "--list-only", "--timeout=1", "--", target, "/dev/null").Run() == nil
}

func redactDestination(destination string) string {
	if parsed, err := url.Parse(destination); err == nil && parsed.User != nil {
		parsed.User = url.User("redacted")
		return parsed.String()
	}
	return rsyncCredentialPattern.ReplaceAllString(destination, "$1[redacted]@")
}

func (s *Server) handleInfo(c echo.Context) error {
	now := time.Now()
	staging := stagingInfo{Path: s.cfg.Output.StagingDir}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(staging.Path, &fs); err != nil {
		slog.Error("reading staging filesystem info", "path", staging.Path, "error", err)
		staging.Error = err.Error()
	} else {
		freeBytes := fs.Bavail * uint64(fs.Bsize)
		totalBytes := fs.Blocks * uint64(fs.Bsize)
		staging.FreeBytes = &freeBytes
		staging.TotalBytes = &totalBytes
	}

	var stats ripStats
	if s.store != nil {
		statsCtx, cancel := context.WithTimeout(c.Request().Context(), infoProbeTimeout)
		defer cancel()
		counts, err := s.store.JobStatusCounts(statsCtx)
		if err != nil {
			slog.Error("reading rip history counts", "error", err)
			return c.JSON(500, map[string]string{"error": "unable to read rip history counts"})
		}
		stats = ripStats{Done: counts["done"], Error: counts["error"], Cancelled: counts["cancelled"]}
	}

	s.mu.RLock()
	drivesDetected := 0
	for _, state := range s.curStates {
		if state.DriveStatus == disc.StatusDiscPresent {
			drivesDetected++
		}
	}
	s.mu.RUnlock()

	uptime := int64(time.Since(s.startedAt).Seconds())
	if uptime < 0 {
		uptime = 0
	}
	return c.JSON(200, infoResponse{
		Name:           "SimpleRip",
		Version:        s.build.Version,
		Commit:         s.build.Commit,
		BuildDate:      s.build.BuildDate,
		GoVersion:      runtime.Version(),
		Hostname:       s.hostname,
		StartedAt:      s.startedAt,
		UptimeSeconds:  uptime,
		DrivesDetected: drivesDetected,
		Tools:          s.infoCache.tools,
		Delivery: deliveryInfo{
			Destination: redactDestination(s.cfg.Output.NASPath),
			Reachable:   s.cachedDeliveryReachability(c.Request().Context(), now),
		},
		Staging: staging,
		Integrations: integrationsInfo{
			TMDBConfigured:    s.cfg.Metadata.TMDBApiKey != "" || s.cfg.Metadata.TMDBAccessToken != "",
			DiscordConfigured: s.cfg.Notification.WebhookURL != "" || s.cfg.Notification.DiscordWebhookURL != "",
		},
		Stats: stats,
	})
}
