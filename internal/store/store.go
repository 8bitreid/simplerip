package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	migrate "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver used by migrations
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// ErrJobActive is returned when deleting a job that is still in progress.
var ErrJobActive = errors.New("job is still in progress")

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Job struct {
	ID         string
	Device     string
	DiscLabel  string
	Title      string
	Year       int
	Status     string
	Pattern    string
	DiscType   string
	CreatedAt  time.Time
	FinishedAt *time.Time
	UpdatedAt  time.Time

	// Populated by ListJobs for failed jobs, from the latest error event.
	ErrorSummary string `json:",omitempty"`
	ErrorHint    string `json:",omitempty"`
}

type JobEvent struct {
	ID        int64
	JobID     string
	Stage     string
	Message   string
	Data      json.RawMessage
	CreatedAt time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	if err := runMigrations(databaseURL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return &Store{pool: pool}, nil
}

func runMigrations(databaseURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("opening db for migrations: %w", err)
	}
	defer db.Close()

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return fmt.Errorf("creating migrate driver: %w", err)
	}

	src, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("loading migration sources: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return fmt.Errorf("creating migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

// scanJob handles nullable columns (disc_label, title, year, pattern, disc_type).
func scanJob(scan func(...any) error) (Job, error) {
	var j Job
	var discLabel, title, pattern, discType *string
	var year *int
	err := scan(&j.ID, &j.Device, &discLabel, &title, &year, &j.Status, &pattern, &discType, &j.CreatedAt, &j.FinishedAt, &j.UpdatedAt)
	if err != nil {
		return Job{}, err
	}
	if discLabel != nil {
		j.DiscLabel = *discLabel
	}
	if title != nil {
		j.Title = *title
	}
	if year != nil {
		j.Year = *year
	}
	if pattern != nil {
		j.Pattern = *pattern
	}
	if discType != nil {
		j.DiscType = *discType
	}
	return j, nil
}

func (s *Store) CreateJob(ctx context.Context, device, discLabel, discType string) (Job, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO jobs (device, disc_label, disc_type)
		 VALUES ($1, $2, $3)
		 RETURNING id, device, disc_label, title, year, status, pattern, disc_type, created_at, finished_at, updated_at`,
		device, discLabel, discType,
	)
	j, err := scanJob(row.Scan)
	if err != nil {
		return Job{}, fmt.Errorf("creating job: %w", err)
	}
	return j, nil
}

func (s *Store) UpdateJob(ctx context.Context, id, title string, year int, status, pattern string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs
		 SET title=$2, year=$3, status=$4, pattern=$5,
		     finished_at=CASE WHEN $4 IN ('done','error','cancelled') THEN COALESCE(finished_at, now()) ELSE NULL END,
		     updated_at=now()
		 WHERE id=$1`,
		id, title, year, status, pattern,
	)
	if err != nil {
		return fmt.Errorf("updating job %s: %w", id, err)
	}
	return nil
}

// UpdateAutoIdentity updates a job's display identity only while no manual
// correction event exists for it.
func (s *Store) UpdateAutoIdentity(ctx context.Context, id, title string, year int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs
		 SET title=$2, year=$3, updated_at=now()
		 WHERE id=$1 AND NOT EXISTS (
		   SELECT 1 FROM job_events
		   WHERE job_id=$1 AND stage='identify' AND data->>'correction'='true'
		 )`,
		id, title, year,
	)
	if err != nil {
		return fmt.Errorf("updating automatic identity for job %s: %w", id, err)
	}
	return nil
}

// UpdateStatusPattern updates status and detection pattern, leaving the
// identified title and year untouched.
func (s *Store) UpdateStatusPattern(ctx context.Context, id, status, pattern string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs
		 SET status=$2, pattern=$3,
		     finished_at=CASE WHEN $2 IN ('done','error','cancelled') THEN COALESCE(finished_at, now()) ELSE NULL END,
		     updated_at=now()
		 WHERE id=$1`,
		id, status, pattern,
	)
	if err != nil {
		return fmt.Errorf("updating status/pattern for job %s: %w", id, err)
	}
	return nil
}

// UpdateStatus updates only the status column, leaving title, year, and pattern
// untouched. Use this for status-only transitions (e.g. scanning → ripping) so
// previously identified metadata is not clobbered mid-pipeline.
func (s *Store) UpdateStatus(ctx context.Context, id, status string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs
		 SET status=$2,
		     finished_at=CASE WHEN $2 IN ('done','error','cancelled') THEN COALESCE(finished_at, now()) ELSE NULL END,
		     updated_at=now()
		 WHERE id=$1`,
		id, status,
	)
	if err != nil {
		return fmt.Errorf("updating status for job %s: %w", id, err)
	}
	return nil
}

func (s *Store) AddEvent(ctx context.Context, jobID, stage, message string, data any) error {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("marshaling event data: %w", err)
		}
		raw = b
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO job_events (job_id, stage, message, data) VALUES ($1, $2, $3, $4)`,
		jobID, stage, message, raw,
	)
	if err != nil {
		return fmt.Errorf("adding event: %w", err)
	}
	return nil
}

// IdentityRecord is one identity decision recorded for a past job: either a
// manual correction or a confident automatic TV match.
type IdentityRecord struct {
	JobID     string
	DiscLabel string
	Data      json.RawMessage
	CreatedAt time.Time
}

// IdentityHistory returns identity decisions from jobs other than excludeJobID,
// newest first.
func (s *Store) IdentityHistory(ctx context.Context, excludeJobID string, limit int) ([]IdentityRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT e.job_id, COALESCE(j.disc_label, ''), e.data, e.created_at
		 FROM job_events e JOIN jobs j ON j.id = e.job_id
		 WHERE e.stage = 'identify' AND e.job_id::text <> $1
		   AND (e.data->>'correction' = 'true'
		        OR (e.data->>'action' = 'tv_identification' AND e.data->>'show_confidence' = 'high'))
		 ORDER BY e.created_at DESC, e.id DESC
		 LIMIT $2`,
		excludeJobID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("listing identity history: %w", err)
	}
	defer rows.Close()

	var records []IdentityRecord
	for rows.Next() {
		var r IdentityRecord
		if err := rows.Scan(&r.JobID, &r.DiscLabel, &r.Data, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning identity history: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *Store) ListJobs(ctx context.Context, limit, offset int) ([]Job, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT j.id, j.device, j.disc_label, j.title, j.year, j.status, j.pattern, j.disc_type, j.created_at, j.finished_at, j.updated_at,
		        e.message, e.data->>'hint'
		 FROM jobs j
		 LEFT JOIN LATERAL (
		   SELECT message, data FROM job_events
		   WHERE job_id = j.id AND stage = 'error'
		   ORDER BY created_at DESC LIMIT 1
		 ) e ON j.status = 'error'
		 ORDER BY COALESCE(j.finished_at, j.created_at) DESC, j.id DESC
		 LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var summary, hint *string
		j, err := scanJob(func(dest ...any) error { return rows.Scan(append(dest, &summary, &hint)...) })
		if err != nil {
			return nil, fmt.Errorf("scanning job: %w", err)
		}
		if summary != nil {
			j.ErrorSummary = *summary
		}
		if hint != nil {
			j.ErrorHint = *hint
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Store) JobStatusCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT status, count(*) FROM jobs
		 WHERE status IN ('done', 'error', 'cancelled')
		 GROUP BY status`,
	)
	if err != nil {
		return nil, fmt.Errorf("counting jobs by status: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int64, 3)
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scanning job status count: %w", err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating job status counts: %w", err)
	}
	return counts, nil
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, []JobEvent, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, device, disc_label, title, year, status, pattern, disc_type, created_at, finished_at, updated_at
		 FROM jobs WHERE id=$1`,
		id,
	)
	j, err := scanJob(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Job{}, nil, ErrNotFound
		}
		return Job{}, nil, fmt.Errorf("getting job %s: %w", id, err)
	}

	rows, err := s.pool.Query(ctx,
		`SELECT id, job_id, stage, message, data, created_at
		 FROM job_events WHERE job_id=$1
		 ORDER BY created_at ASC`,
		id,
	)
	if err != nil {
		return Job{}, nil, fmt.Errorf("getting events for job %s: %w", id, err)
	}
	defer rows.Close()

	var events []JobEvent
	for rows.Next() {
		var e JobEvent
		if err := rows.Scan(&e.ID, &e.JobID, &e.Stage, &e.Message, &e.Data, &e.CreatedAt); err != nil {
			return Job{}, nil, fmt.Errorf("scanning event: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return Job{}, nil, fmt.Errorf("iterating events: %w", err)
	}

	return j, events, nil
}

// DeleteJob removes a finished (done, error, or cancelled) job and, via ON DELETE CASCADE,
// its events. In-progress jobs are refused with ErrJobActive.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM jobs WHERE id=$1 AND status IN ('done','error','cancelled')`, id)
	if err != nil {
		return fmt.Errorf("deleting job %s: %w", id, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	if _, _, err := s.GetJob(ctx, id); err != nil {
		return err
	}
	return ErrJobActive
}

// DeleteFinishedJobs removes every done, error, or cancelled job and returns how many.
func (s *Store) DeleteFinishedJobs(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM jobs WHERE status IN ('done','error','cancelled')`)
	if err != nil {
		return 0, fmt.Errorf("deleting finished jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}
