# SimpleRip

A minimal, stable disc ripper written in Go. Wraps makemkvcon + ffprobe + rsync.
This is a deliberate rewrite of Automatic Ripping Machine (ARM), which is unstable.

## Philosophy
SimpleRip owns zero media logic. It is a pure orchestrator:
- Detect disc insertion (udev / polling /dev/sr*)
- Scan titles via `makemkvcon -r info` (no rip yet)
- Classify titles (TV, movie, double feature, ambiguous)
- Rip via `makemkvcon mkv` — output is final, no remux
- Inspect finished MKV via `ffprobe` for notification payload only
- Deliver to NAS via `rsync`
- Notify via Discord webhook ONLY after rsync exits 0 and files verified

## Key design decisions
- MKV output is lossless and untouched — all video, audio, and subtitle
  tracks preserved as-is. Tdarr handles transcoding separately.
- No remuxing. The fat MKV from makemkvcon IS the final file.
- ffprobe is used only to read finished MKV metadata for Discord notifications
  (codec, channels, size, track list) — never to modify files.
- Notification fires AFTER rsync completes and destination files are verified.
  This fixes the ARM problem of notifying before the move is done.

## Title classification logic
Scan all titles with `makemkvcon -r info` first, then:
- 3+ titles within 60s duration of each other → TV mode, rip all automatically
- 2 titles same duration → double feature, ask via Discord
- 1 long title (>40 min) + shorter others → single movie, rip main
  immediately, ask about extras via Discord
- Ambiguous → ask via Discord
- Junk = under 2 minutes, silently ignored

TV mode always rips everything automatically — no asking.
Main feature always starts ripping immediately, never waits for user input.
Extras and ambiguous cases pause and ask via Discord.

## n8n / Discord interaction model
- SimpleRip POSTs a JSON payload to an n8n webhook when input is needed
- n8n formats it as a Discord message with action buttons
- User responds in Discord
- n8n POSTs the response back to SimpleRip's HTTP server at :8090
- SimpleRip holds a response channel per job with a configurable timeout
  (default 30 min) — if no response, skip extras and continue
- Job ID ties the callback to the right in-flight rip

## Subprocess handling
makemkvcon can hang (known Linux issue, especially with Blu-ray drives).
All makemkvcon calls must use context.WithTimeout and handle DeadlineExceeded
gracefully — kill the process, fire a Discord alert, move on.

## Project structure
cmd/simplerip/main.go        — daemon entrypoint
internal/disc/detect.go      — disc type detection via udev/blkid
internal/disc/types.go       — DiscType enum, MKVTitle struct
internal/ripper/makemkv.go   — exec makemkvcon, parse stdout
internal/ripper/classify.go  — title classification logic
internal/output/stage.go     — move files, write rip.json log
internal/notify/discord.go   — webhook payloads
internal/server/webhook.go   — HTTP server for n8n callbacks
internal/config/config.go    — load config.yaml

## config.yaml structure
detection:
  tv_threshold: 3              # 3+ same-duration titles = TV
  duration_tolerance_sec: 60
  min_feature_minutes: 40
  min_extra_minutes: 2

output:
  staging_dir: /staging
  nas_path: /output
  folder_format: "{{.Title}} ({{.Year}})"

notification:
  webhook_url: ""              # n8n webhook URL
  response_timeout_minutes: 30
  callback_port: 8090

makemkv:
  key: ""                      # overridden by MAKEMKV_KEY env var
  timeout_minutes: 120
  devices:
    - /dev/sr0
    - /dev/sr1

metadata:
  tmdb_api_key: ""             # for title lookup
  preferred_language: eng

## Deployment
Docker Compose. Multi-stage Dockerfile:
- Stage 1: Go binary (golang:1.22-alpine)
- Stage 2: makemkvcon built from source (ubuntu:24.04, MAKEMKV_VERSION=1.18.3)
- Stage 3: Final image — just the binaries + ffmpeg + rsync

Optical drives passed through as devices (/dev/sr0, /dev/sr1).
Requires cap_add: SYS_RAWIO for drive access.
MAKEMKV_KEY set as environment variable.

## What to build first
1. `makemkvcon -r info` stdout parser — foundation for everything
2. Title classification logic
3. Single disc rip end-to-end (no Discord, no n8n yet)
4. rsync delivery + file verification
5. Discord notification (post-delivery)
6. n8n callback server + extras flow
7. TMDB metadata lookup
8. Dockerfile + docker-compose.yml
9. udev disc detection
