# simplerip

A minimal, stable Blu-ray/DVD ripper written in Go. Wraps **makemkvcon**, **ffprobe**, and **rsync**.

Inspired by [Automatic Ripping Machine (ARM)](https://github.com/automatic-ripping-machine/automatic-ripping-machine). SimpleRip takes a different architectural approach: it never modifies the video stream, delegates all ripping to makemkvcon, and delegates all transcoding to Tdarr. Its own logic is limited to disc classification, duplicate detection, and audio quality scoring for keeper selection.

---

## What it does

1. Detects disc insertion via udev / polling `/dev/sr*`
2. Scans titles with `makemkvcon -r info` (no rip yet)
3. Classifies titles — TV show, movie, double feature, extras, ambiguous
4. Rips via `makemkvcon mkv` — output is the final MKV, **no remux**
5. Inspects finished MKVs via `ffprobe` for quality scoring and notifications
6. Delivers to NAS via `rsync`
7. Notifies via Discord webhook **only after** rsync completes and files are verified

Transcoding is handled separately by Tdarr. SimpleRip never touches the video stream.

---

## Prerequisites

| Tool | Purpose |
|------|---------|
| `makemkvcon` | Ripping Blu-ray / DVD |
| `ffprobe` (part of ffmpeg) | MKV metadata inspection |
| `rsync` | NAS delivery |
| Go 1.22+ | Building from source |

---

## Installation

### From source

```bash
git clone https://github.com/8bitreid/simplerip.git
cd simplerip
go build -o simplerip ./cmd/simplerip
```

### Docker

```bash
export MAKEMKV_KEY="your-license-key-here"
docker compose up -d
```

To build from a local checkout, use `make up` instead: it stamps the image with the current branch (`<branch>-SNAPSHOT`), commit (with `-dirty` for uncommitted changes) and build time, which the dashboard shows under Version. `make version` prints what would be stamped.

Optical drives are passed through as devices (`/dev/sr0`, `/dev/sr1`). Set `MAKEMKV_KEY` for Blu-ray ripping (recommended) — see Configuration below. Set `SIMPLERIP_HOST` to override the hostname shown in the dashboard info card.

---

## Configuration

Copy the example config and fill in your paths and API keys:

```bash
cp config.yaml.example config/config.yaml
# Edit config/config.yaml with your TMDB/OMDb keys
```

### MakeMKV License Key

Set the `MAKEMKV_KEY` environment variable:

```bash
export MAKEMKV_KEY="your-license-key-here"
```

For Docker, this is passed through in compose.yaml. For systemd, add it to your service file. Get a free beta key at https://www.makemkv.com/forum/viewtopic.php?t=1053

### Key configuration fields:

```yaml
output:
  staging_dir: /staging      # fast local storage
  nas_path: /output          # NAS mount point

metadata:
  tmdb_api_key: ""           # https://www.themoviedb.org/settings/api
  tmdb_access_token: ""      # TMDB v4 API Read Access Token (Bearer), or set TMDB_ACCESS_TOKEN
  omdb_api_key: ""           # https://www.omdbapi.com/apikey.aspx

notification:
  webhook_url: ""            # n8n webhook for Discord alerts
  discord_webhook_url: ""    # prefer the DISCORD_WEBHOOK_URL env var
  ui_url: ""                 # linked from notifications (or SIMPLERIP_UI_URL)
  events: {needs_input: true, multi_title: true, complete: true, failed: true, duration_mismatch: true}
  callback_port: 8090        # port for n8n to POST responses back
```

See [config.yaml.example](config.yaml.example) for all options. The file `config/config.yaml` is gitignored.
When `tmdb_access_token` (or `TMDB_ACCESS_TOKEN`) is set, it is used as a Bearer token and takes precedence over `tmdb_api_key`. The UI searches TMDB `/search/multi` and offers both movies and TV shows.

---

## Commands

### `simplerip rip`

Start the disc-ripping daemon. Watches configured devices and rips automatically.

```bash
simplerip rip
```

### `simplerip organize`

Identify, deduplicate, and rename MKVs to Title (Year) format. Useful for organizing multi-playlist Blu-ray artifacts (where makemkvcon produces several near-identical MKVs from the same disc).

```bash
simplerip organize -dir /path/to/movie/dir [flags]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `-dir` | required | Directory containing MKV files to organize |
| `-query` | derived from dir name | Override the TMDB search query |
| `-edition` | `""` | Label alternate cuts (e.g. `"Director's Cut"`) |
| `-dry-run` | false | Show what would happen without moving files |
| `-yes` | false | Skip confirmation prompts |

**What organize does:**

1. **Flatten subdirs** — moves MKVs from any subdirectory (extras, bonus, etc.) up into the parent, then removes the now-empty subdirectory
2. **Deduplicate** — groups files by duration (±30 s = same version), scores each group by audio quality, keeps the best, moves the rest to `_duplicates/`
3. **TMDB lookup** — searches for the movie title, with progressive query retry (drops the last word until results are found)
4. **OMDb enrichment** — cross-references runtime from both TMDB and OMDb for edition detection
5. **Rename** — the file closest to the theatrical runtime gets `Title (Year)/Title (Year).mkv`; others get `Title (Year) - Alternate (Xmin).mkv`

**Example:**

```bash
simplerip organize -dir "/mnt/nas/Dev Movies/Pitch-Black (2000)"
simplerip organize -dir "/mnt/nas/Dev Movies/Pitch-Black (2000)" -edition "Director's Cut" -yes
simplerip organize -dir "/mnt/nas/Dev Movies/Pitch-Black (2000)" -dry-run
```

---

## Quality scoring

When duplicates are found, simplerip picks the keeper by audio quality — not file size. A file with no English audio is disqualified entirely.

| Criterion | Weight |
|-----------|--------|
| TrueHD / Atmos | 100 |
| DTS-HD Master Audio | 90 |
| FLAC / PCM (lossless) | 85 |
| DTS-HD HRA | 80 |
| DTS core | 60 |
| Dolby Digital Plus (EAC3) | 50 |
| Dolby Digital (AC3) | 40 |
| AAC | 30 |
| MP3 | 20 |
| **7.1 channels** | +40 |
| **6.1 channels** | +35 |
| **5.1 channels** | +30 |
| **Stereo** | +10 |
| English subtitles present | +10 |
| File size (tiebreaker, capped) | 0–5 |

The full breakdown is shown in the duplicate analysis report for every `[KEEP]` and `[DUPE]` entry.

---

## Discord / n8n integration

### Discord notifications

Set `DISCORD_WEBHOOK_URL` (put it in the gitignored `.env`; compose passes it through) and optionally `SIMPLERIP_UI_URL`. Each message includes the disc name, device, and media title, plus a link to the UI. Events, each toggled under `notification.events`:

- `needs_input` — no main title detected, or no confident TMDB match
- `multi_title` — more than one title ripped from a disc
- `complete` — rsync finished and files verified
- `failed` — scan, rip, or delivery failed
- `duration_mismatch` — ripped file length differs from the TMDB/OMDb runtime by more than 3 minutes

Sending is asynchronous and best-effort: a dead webhook is logged (without the URL) and never blocks or fails a rip. Senders implement `notify.Sender`, so other channels can be added without touching the pipeline.

SimpleRip POSTs JSON payloads to an n8n webhook when user input is needed (extras, double features, ambiguous discs). n8n formats it as a Discord message with action buttons. The user responds in Discord, n8n POSTs the response back to SimpleRip's callback server (`:8090`), and the rip continues.

If no response is received within `response_timeout_minutes`, extras are skipped and the main feature is delivered.

---

## Title classification

| Condition | Action |
|-----------|--------|
| 3+ titles in a similar-duration cluster | TV mode — rip the episode-like cluster; keep duration outliers as extras |
| 2 titles, same duration | Double feature — ask via Discord |
| 1 long title (>40 min) + shorter others | Rip main immediately, ask about extras |
| Ambiguous | Ask via Discord |
| Under 2 minutes | Silently ignored (junk) |

TV discs rip their selected titles in one `makemkvcon` invocation. The batch
timeout is `batch_analyze_budget_minutes` plus
`batch_save_budget_minutes` multiplied by the number of selected titles
(defaults: 45 minutes for analysis and 10 minutes per title save). If a batch
fails partway through, completed title files are kept and only missing titles
are retried individually. Movie ripping continues to use one invocation per
title.

When TMDB credentials are configured, TV discs are searched using a normalized disc label and, when available, meaningful MakeMKV title names. Similarity and the lead over competing results must support a clear show match before the show name is used automatically. Identification evidence, candidate titles, separate show/season/episode confidence, and lookup errors are recorded in job history; a suggestion is not a probability. An uncertain show keeps the safe disc-label naming and remains searchable/correctable in the UI. Movie lookup behavior is unchanged.

Season inference is separate from show identification. SimpleRip compares the runtimes of the episode-like title cluster with every regular season's episode runtimes when TMDB provides complete data for no more than 20 seasons; it chooses a season only when at least three titles support a close and distinctive match. Explicit season/episode markers in all relevant MakeMKV title names can also identify both directly. Episode numbers are otherwise inferred only when individual runtimes uniquely identify episodes; MakeMKV title indexes are never assumed to be viewing order. `DISC1`/volume labels are removed from the search query but are never treated as season numbers.

If season or episode order remains unknown, ripping continues without waiting indefinitely for a metadata lookup or a manual response. Output is kept under a disc-specific unsorted directory (or an unsorted subdirectory of a confidently matched show/season), retaining MakeMKV's filenames rather than inventing episode numbers. The UI's manual search/correction flow remains available. A manually selected show and season remains authoritative; with a manually chosen season and no starting episode, the existing behavior continues after the highest saved episode number. TMDB lookup failures are recorded and use the same safe fallback. These inferences use only disc metadata and the configured TMDB integration; SimpleRip does not inspect video frames or require another service.

---

## Project structure

```
cmd/simplerip/main.go          daemon entrypoint + organize subcommand
internal/disc/                 disc type detection
internal/ripper/               makemkvcon wrapper + title classification
internal/inspect/              ffprobe wrapper + quality scoring
internal/metadata/             TMDB + OMDb clients, edition detection
internal/output/               staging, rsync delivery, deduplication
internal/notify/               Discord webhook payloads
internal/server/               HTTP callback server for n8n responses
internal/config/               config.yaml loading
```

---

## License and legal

**MakeMKV:** The Docker image builds and bundles `makemkvcon` from source. By building or pulling this image you are accepting the MakeMKV End-User License Agreement, available at [makemkv.com](https://www.makemkv.com). MakeMKV is free to use while in beta; a license key is required for Blu-ray ripping. SimpleRip does not distribute MakeMKV binaries directly.

---

## Credits and attributions

- [Automatic Ripping Machine (ARM)](https://github.com/automatic-ripping-machine/automatic-ripping-machine) — the original inspiration for automated disc ripping on Linux. ARM is a community project with a lot of history; SimpleRip simply takes a different design approach.
- [MakeMKV](https://www.makemkv.com/) — the core ripping engine. MakeMKV is a commercial product; a valid license key is required for Blu-ray ripping. SimpleRip does not bundle or redistribute MakeMKV binaries.
- This product uses the [TMDB API](https://www.themoviedb.org/) but is not endorsed or certified by TMDB.
- Movie and series data provided in part by [OMDb API](https://www.omdbapi.com/).

---

## License

MIT
