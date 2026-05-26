# simplerip

A minimal, stable Blu-ray/DVD ripper written in Go. Wraps **makemkvcon**, **ffprobe**, and **rsync**.

Deliberately rewritten from [Automatic Ripping Machine (ARM)](https://github.com/automatic-ripping-machine/automatic-ripping-machine), which is unstable. SimpleRip owns zero media logic — it is a pure orchestrator.

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
docker compose up -d
```

See [Dockerfile](Dockerfile) and [docker-compose.yml](docker-compose.yml). Optical drives are passed through as devices (`/dev/sr0`, `/dev/sr1`). Set `MAKEMKV_KEY` as an environment variable.

---

## Configuration

Copy the example config and fill in your paths and API keys:

```bash
cp config.yaml.example config/config.yaml
```

Key fields:

```yaml
output:
  staging_dir: /staging      # fast local storage
  nas_path: /output          # NAS mount point

makemkv:
  key: ""                    # or set MAKEMKV_KEY env var
  devices: [/dev/sr0, /dev/sr1]

metadata:
  tmdb_api_key: ""           # https://www.themoviedb.org/settings/api
  omdb_api_key: ""           # https://www.omdbapi.com/apikey.aspx

notification:
  webhook_url: ""            # n8n webhook for Discord alerts
  callback_port: 8090
```

API keys are gitignored — never commit `config/config.yaml`.

---

## Commands

### `simplerip rip`

Start the disc-ripping daemon. Watches configured devices and rips automatically.

```bash
simplerip rip
```

### `simplerip clean`

Deduplicate and rename an already-ripped directory. Useful for cleaning up multi-playlist Blu-ray artifacts (where makemkvcon produces several near-identical MKVs from the same disc).

```bash
simplerip clean -dir /path/to/movie/dir [flags]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `-dir` | required | Directory containing MKV files to clean |
| `-query` | derived from dir name | Override the TMDB search query |
| `-edition` | `""` | Label alternate cuts (e.g. `"Director's Cut"`) |
| `-dry-run` | false | Show what would happen without moving files |
| `-yes` | false | Skip confirmation prompts |

**What clean does:**

1. **Flatten subdirs** — moves MKVs from any subdirectory (extras, bonus, etc.) up into the parent, then removes the now-empty subdirectory
2. **Deduplicate** — groups files by duration (±30 s = same version), scores each group by audio quality, keeps the best, moves the rest to `_duplicates/`
3. **TMDB lookup** — searches for the movie title, with progressive query retry (drops the last word until results are found)
4. **OMDb enrichment** — cross-references runtime from both TMDB and OMDb for edition detection
5. **Rename** — the file closest to the theatrical runtime gets `Title (Year)/Title (Year).mkv`; others get `Title (Year) - Alternate (Xmin).mkv`

**Example:**

```bash
simplerip clean -dir "/mnt/nas/Dev Movies/Pitch-Black (2000)"
simplerip clean -dir "/mnt/nas/Dev Movies/Pitch-Black (2000)" -edition "Director's Cut" -yes
simplerip clean -dir "/mnt/nas/Dev Movies/Pitch-Black (2000)" -dry-run
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

SimpleRip POSTs JSON payloads to an n8n webhook when user input is needed (extras, double features, ambiguous discs). n8n formats it as a Discord message with action buttons. The user responds in Discord, n8n POSTs the response back to SimpleRip's callback server (`:8090`), and the rip continues.

If no response is received within `response_timeout_minutes`, extras are skipped and the main feature is delivered.

---

## Title classification

| Condition | Action |
|-----------|--------|
| 3+ titles within 60 s of each other | TV mode — rip all automatically |
| 2 titles, same duration | Double feature — ask via Discord |
| 1 long title (>40 min) + shorter others | Rip main immediately, ask about extras |
| Ambiguous | Ask via Discord |
| Under 2 minutes | Silently ignored (junk) |

---

## Project structure

```
cmd/simplerip/main.go          daemon entrypoint + clean subcommand
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

## License

MIT
