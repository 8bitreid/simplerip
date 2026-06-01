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
	_ "github.com/jackc/pgx/v5/stdlib"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Job struct {
	ID        string
	Device    string
	DiscLabel string
	Title     string
	Year      int
	Status    string
	Pattern   string
	CreatedAt time.Time
	UpdatedAt time.Time
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

// scanJob handles nullable columns (disc_label, title, year, pattern).
func scanJob(scan func(...any) error) (Job, error) {
	var j Job
	var discLabel, title, pattern *string
	var year *int
	err := scan(&j.ID, &j.Device, &discLabel, &title, &year, &j.Status, &pattern, &j.CreatedAt, &j.UpdatedAt)
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
	return j, nil
}

func (s *Store) CreateJob(ctx context.Context, device, discLabel string) (Job, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO jobs (device, disc_label)
		 VALUES ($1, $2)
		 RETURNING id, device, disc_label, title, year, status, pattern, created_at, updated_at`,
		device, discLabel,
	)
	j, err := scanJob(row.Scan)
	if err != nil {
		return Job{}, fmt.Errorf("creating job: %w", err)
	}
	return j, nil
}

func (s *Store) UpdateJob(ctx context.Context, id, title string, year int, status, pattern string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET title=$2, year=$3, status=$4, pattern=$5, updated_at=now() WHERE id=$1`,
		id, title, year, status, pattern,
	)
	if err != nil {
		return fmt.Errorf("updating job %s: %w", id, err)
	}
	return nil
}

// UpdateStatus updates only the status column, leaving title, year, and pattern
// untouched. Use this for status-only transitions (e.g. scanning → ripping) so
// previously identified metadata is not clobbered mid-pipeline.
func (s *Store) UpdateStatus(ctx context.Context, id, status string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status=$2, updated_at=now() WHERE id=$1`,
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

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, device, disc_label, title, year, status, pattern, created_at, updated_at
		 FROM jobs
		 ORDER BY created_at DESC
		 LIMIT 100`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scanning job: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, []JobEvent, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, device, disc_label, title, year, status, pattern, created_at, updated_at
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
