# SimpleRip

A minimal disc ripper written in Go. Wraps makemkvcon + ffprobe + rsync.
Inspired by Automatic Ripping Machine (ARM), with a focus on clean orchestration.

## Philosophy
SimpleRip never modifies the video stream. All ripping is delegated to makemkvcon;
all transcoding is delegated to Tdarr. SimpleRip's own logic covers:
- Disc scanning and title classification
- Duplicate detection and audio quality scoring
- rsync delivery with size verification
- Notifications via webhook

## Key design decisions
- MKV output is lossless and untouched — all video, audio, and subtitle
  tracks preserved as-is. Tdarr handles transcoding separately.
- No remuxing. The fat MKV from makemkvcon IS the final file.
- ffprobe is used to read finished MKV metadata for quality scoring and Discord
  notifications (codec, channels, size, track list) — never to modify files.
- Notification fires AFTER rsync completes and destination files are verified.
- makemkvcon license key is passed via MAKEMKV_KEY env var (never in process list).

## Commands
Three subcommands (all in cmd/simplerip/main.go):

### scan
Scans a disc with `makemkvcon -r info`, classifies titles, prints JSON.
Flags: `-device /dev/sr0`, `-fixture <file>` (replay captured output for testing).

If scan reports `missing_metadata: true`, the disc is encrypted or unreadable
and makemkvcon couldn't extract title information (duration, size, chapters).
In this case, try ripping title 0 or 1 manually:
```bash
simplerip rip -device /dev/sr0 -title 0 -output /tmp/test
```
Some heavily encrypted Blu-rays won't reveal metadata during scan but will rip
successfully once you specify a title index.

### rip
Rips a single title from a disc to an output directory.
Flags: `-device`, `-title` (index), `-output`.
Exits with code 3 on timeout (distinct from other errors).

### organize
Post-rip deduplication and rename workflow for manual rips. Reads an already-ripped
directory, removes duplicate MKVs (keeping the highest audio quality), looks up
TMDB/OMDb metadata, and renames files to `Title (Year)/Title (Year).mkv`.
Flags: `-dir` (required), `-query`, `-edition`, `-dry-run`, `-yes`.

**Note:** The organize command is optional when using daemon mode (`simplerip serve`),
as the automated pipeline performs TMDB lookup before ripping and delivers files
with proper names immediately. Organize is primarily useful for:
- Manual rips via `simplerip rip` command
- Re-processing old rips from other tools
- Correcting metadata after manual TMDB query override

## Title classification logic
Scan all titles with `makemkvcon -r info` first, then:
- All titles missing duration metadata (80%+ have zero duration) → Missing metadata mode, show warning
- 3+ titles within 60s duration of each other → TV mode, rip all as main titles
- Exactly 2 feature-length titles within tolerance → double feature, ask via Discord
- 1 long title (>40 min) + shorter others → rip main immediately, ask about extras
- Ambiguous → ask via Discord
- Junk = under 2 minutes, silently ignored

TV mode always rips everything automatically. Main feature never waits for user input.
Extras and ambiguous cases pause and ask via Discord.

Missing metadata mode occurs when makemkvcon can't read title information due to
encryption or disc read errors. The scan will show all titles with zero duration,
size, and chapter count. In this case, manual title selection is required — typically
title 0 or 1 is the main feature.

**Multi-angle discs:** Some Blu-rays (especially older releases like Star Wars) have
the same movie from multiple camera angles as separate titles. SimpleRip detects
multi-angle discs by identifying titles with identical duration and chapter counts.
When detected, only angle 1 is automatically selected as the main title (classification
rule 0.5, highest priority). The scan JSON output includes `multi_angle: true` and
`angle_count: N` fields to indicate multi-angle disc structure.

## Duplicate detection and quality scoring
`simplerip organize` groups MKVs by duration (±30s = same version), scores each by
audio quality, keeps the best, moves the rest to `_duplicates/`. A file with no
English audio is disqualified entirely.

Audio codec ranking (higher = better):
TrueHD (100) → DTS-HD MA (90) → FLAC/PCM (85) → DTS-HD HRA (80) →
DTS core (60) → EAC3 (50) → AC3 (40) → AAC (30) → MP3 (20)

Channel bonus: 7.1 (+40), 6.1 (+35), 5.1 (+30), stereo (+10).
English subtitles: +10. Size tiebreaker: 0–5 (capped).

## Metadata enrichment
TMDB is queried first. OMDb is cross-referenced via IMDb ID for runtime validation.
If TMDB and OMDb runtimes agree within 3 minutes, they are averaged. If they diverge
by more than 3 minutes, the longer value is used (safe upper bound for theatrical
runtime matching). Files within ±3 minutes of theatrical runtime get the clean name;
others get an `Alternate (Xmin)` label.

`QueryFromDirName()` converts folder names like "Star-Wars--Episode-I" into TMDB
search queries automatically, with progressive retry (drops last word until results
are found).

## Webhook / Discord notification model
SimpleRip POSTs JSON payloads to a configurable webhook URL (`notification.webhook_url`).
This can be either:

**Direct Discord webhook** (simple, one-way notifications):
- Set `webhook_url` to your Discord webhook URL
- Receives rip completion notifications with file metadata
- No interactive features (extras, ambiguous disc handling)

**n8n webhook** (interactive workflow):
- Set `webhook_url` to your n8n webhook endpoint  
- n8n formats payloads as Discord messages with action buttons
- User responds in Discord for extras/ambiguous disc decisions
- n8n POSTs response back to SimpleRip's HTTP server at :8090
- SimpleRip holds a response channel per job with configurable timeout (default 30 min)
- Job ID ties the callback to the right in-flight rip

Webhook payload includes full MKV metadata (codec, resolution, audio tracks, size)
so Discord displays it without a second lookup.

## Subprocess handling
makemkvcon can hang (known Linux issue, especially with Blu-ray drives).
All makemkvcon calls use context.WithTimeout. RipTitle returns ErrRipTimeout
(a distinct sentinel error) on deadline exceeded — callers can distinguish
timeout from other failures. Progress lines (PRGV) are parsed and logged as
"title <index>: <pct>%" to stdout in real time.

## Project structure
```
cmd/simplerip/main.go          scan, rip, organize subcommands + entrypoint
internal/disc/types.go         DiscType enum, MKVTitle, ClassifiedDisc structs
internal/ripper/makemkv.go     exec makemkvcon -r info, parse robot-mode stdout
internal/ripper/rip.go         exec makemkvcon mkv, parse progress, return ErrRipTimeout
internal/ripper/classify.go    title classification logic (TV/movie/double/ambiguous)
internal/inspect/ffprobe.go    exec ffprobe, parse JSON, extract audio/video/sub tracks
internal/inspect/quality.go    audio quality scoring for duplicate keeper selection
internal/metadata/tmdb.go      TMDB search + movie detail fetch
internal/metadata/omdb.go      OMDb lookup by IMDb ID, runtime parsing
internal/metadata/details.go   runtime reconciliation, edition labeling
internal/output/stage.go       rsync delivery, size verification, rip.json audit log
internal/output/clean.go       FlattenSubdirs, groupByDuration, ExecuteDedupe
internal/notify/discord.go     webhook payload builders (RipComplete, Extras, Ambiguous)
internal/server/webhook.go     HTTP callback server, per-job response channels
internal/config/config.go      load config.yaml, apply defaults, MAKEMKV_KEY override
```

## config.yaml structure
```yaml
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
  webhook_url: ""              # Discord webhook URL or n8n endpoint
  response_timeout_minutes: 30  # Only used with n8n interactive workflows
  callback_port: 8090           # Only used with n8n interactive workflows

makemkv:
  # key: use MAKEMKV_KEY env var instead
  timeout_minutes: 120
  devices:
    - /dev/sr0
    - /dev/sr1

metadata:
  tmdb_api_key: ""             # https://www.themoviedb.org/settings/api
  omdb_api_key: ""             # https://www.omdbapi.com/apikey.aspx
  preferred_language: eng
```

**MakeMKV key handling:**
- The `MAKEMKV_KEY` environment variable **always overrides** `makemkv.key` from config
- Environment variable is the **recommended approach** (keeps secrets out of config files)
- Docker Compose passes it through via compose.yaml (needed for Blu-ray ripping)
- Config file fallback exists for simpler local development setups
- `config/config.yaml` is gitignored — never commit it

## Git workflow
**Never push directly to main.** main is branch-protected — direct pushes are
blocked and the rule applies to admins too.

All changes must go through a pull request:
1. Create a branch: `git checkout -b your-branch`
2. Push the branch: `git push origin your-branch`
3. Open a PR targeting main
4. The `test` CI job must pass before merging

The `test` workflow runs `go test ./...` on every PR. Tests must never be
broken at merge time. The Docker publish workflow fires automatically on merge.

## Testing
Fixture support: `simplerip scan -fixture <file>` replays captured makemkvcon output
without a physical disc. Use this in tests via `ScanInfoFromReader()` in makemkv.go.

## Deployment
Docker Compose. Multi-stage Dockerfile:
- Stage 1: Go binary (golang:1.22-alpine)
- Stage 2: makemkvcon built from source (ubuntu:24.04, MAKEMKV_VERSION=1.18.3)
- Stage 3: Final image — just the binaries + ffmpeg + rsync

Optical drives passed through as devices (/dev/sr0, /dev/sr1).
Requires cap_add: SYS_RAWIO for drive access.
MAKEMKV_KEY set as environment variable.

## Automated daemon workflow
The daemon mode (`simplerip serve`) implements the full automated pipeline:

1. **Disc detection** - Polls optical devices every 5 seconds for disc insertion
2. **Scan** - Reads disc metadata (DiscName, titles, durations) via makemkvcon
3. **TMDB lookup** - Searches for movie using DiscName, auto-selects first result
4. **Classify** - Determines MainTitles vs Extras using duration patterns + multi-angle detection
5. **Rip** - Executes makemkvcon with real-time progress (shows actual movie title in UI)
6. **Deliver** - rsyncs files to NAS with proper folder structure: `Title (Year)/Title (Year).mkv`
7. **Cleanup** - Removes staging directory after successful delivery
8. **Notify** - Sends webhook notification (Discord direct or via n8n, if configured)

Progress updates stream to the web UI via WebSocket at `ws://localhost:8080/ws/progress`.
The UI displays the actual movie title during ripping (e.g. "Ripping Star Wars - Episode III
- Revenge of the Sith (2005): 45%") instead of generic "title 0" labels.

If TMDB API key is not configured or lookup fails, the raw DiscName is used for naming.
The organize command is no longer required in the automated workflow — metadata enrichment
happens before ripping so files are delivered with final names immediately.

## What still needs building
- TV show episode detection and naming (TMDB API doesn't provide per-episode disc metadata)
- Interactive extras selection workflow (Discord → n8n → SimpleRip callback integration)
- Ambiguous disc handling workflow (requires n8n callback system)

## Recent fixes (May 2026)
**Fixed duplicate rip detection (May 31, 2026):**
The disc polling logic was re-triggering rips on the same disc after the first rip completed.
Root cause: During active ripping, makemkvcon holds the drive exclusively. When Poll() ran
`checkDevice()` while the drive was busy, the check would timeout/fail and update state to
"no disc present". When the rip finished and the drive became available again, the disc
appeared as "newly inserted" and triggered another rip.

Fix: Modified `checkDevice()` to return `(hasDisc, ok)` tuple distinguishing between "no disc"
and "check failed". When `ok=false` (drive busy, timeout, error), state is not updated,
preventing false "disc removed" events during active rips. The same disc will only trigger
one rip until it's physically removed and reinserted.

**Automated workflow with TMDB integration (May 31, 2026):**
Integrated TMDB metadata lookup into the automated daemon pipeline:
- TMDB lookup now happens immediately after disc scan, before ripping
- Movie title and year are resolved early and used throughout the pipeline
- Progress updates show actual movie title instead of "title 0" labels
- Files are delivered to NAS with proper folder names: `Title (Year)/Title (Year).mkv`
- No separate "organize" step required for automated rips
- Falls back to raw DiscName if TMDB API key not configured or lookup fails

**makemkvcon v1.18.3 attribute ID corrections:**
The makemkvcon robot-mode output format changed between versions. Fixed attribute ID
mappings in `internal/ripper/makemkv.go`:
- TINFO attribute 8 = chapters (was incorrectly 9)
- TINFO attribute 9 = duration (was incorrectly 11)
- TINFO attribute 11 = size in bytes (was incorrectly 28)
- TINFO attribute 30 = title description (newly added, used for angle detection)
- SINFO attribute 1 = stream type code (was incorrectly 22)

**Multi-angle detection (Rule 0.5):**
Added `detectMultiAngle()` in `internal/ripper/classify.go` that:
- Identifies titles with identical duration (±3s) and chapter count
- Filters to titles with AngleNumber > 0 (parsed from attribute 30)
- Requires 2+ matching titles to trigger multi-angle classification
- Selects only angle 1 as MainTitle, classifies as Movie pattern
- Adds `MultiAngle` and `AngleCount` fields to `ClassificationResult`
- Outputs `multi_angle: true` and `angle_count: N` in scan JSON

**Test fixture updates:**
Updated `testdata/movie.txt`, `testdata/tvshow.txt`, and added
`testdata/missing-metadata.txt` to match makemkvcon v1.18.3 output format
with corrected attribute positions. Fixed all test mock scripts to use `#!/bin/sh`
instead of `#!/bin/bash` for Alpine Linux compatibility.
