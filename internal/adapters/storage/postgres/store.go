package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/patchproof/patchproof/internal/domain"
	"github.com/patchproof/patchproof/internal/ports"
	postgresmigrations "github.com/patchproof/patchproof/migrations/postgres"
)

const (
	defaultMaxConns         = int32(10)
	defaultStatementTimeout = 30 * time.Second
	defaultApplicationName  = "patchproof"
	migrationLockKey        = int64(0x706174636870726f)
	partitionAheadMonths    = 3
)

// Config configures the PostgreSQL metadata store connection pool.
type Config struct {
	DSN              string
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	MaxConnIdleTime  time.Duration
	StatementTimeout time.Duration
	ApplicationName  string
}

// Store is a PostgreSQL implementation of ports.MetadataStore.
type Store struct {
	pool *pgxpool.Pool
}

var _ ports.MetadataStore = (*Store)(nil)

// querier is the subset of pgxpool.Pool and pgx.Tx used by the store helpers.
type querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// New opens a connection pool to PostgreSQL and verifies connectivity.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, &domain.ValidationError{Field: "dsn", Reason: "must not be empty"}
	}
	poolConfig, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	maxConns := cfg.MaxConns
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}
	minConns := cfg.MinConns
	if minConns < 0 {
		minConns = 0
	}
	if minConns > maxConns {
		return nil, &domain.ValidationError{Field: "min_conns", Reason: "must not exceed max_conns"}
	}
	poolConfig.MaxConns = maxConns
	poolConfig.MinConns = minConns
	if cfg.MaxConnLifetime > 0 {
		poolConfig.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolConfig.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	statementTimeout := cfg.StatementTimeout
	if statementTimeout <= 0 {
		statementTimeout = defaultStatementTimeout
	}
	applicationName := cfg.ApplicationName
	if strings.TrimSpace(applicationName) == "" {
		applicationName = defaultApplicationName
	}
	if poolConfig.ConnConfig.RuntimeParams == nil {
		poolConfig.ConnConfig.RuntimeParams = make(map[string]string)
	}
	poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(statementTimeout.Milliseconds(), 10)
	poolConfig.ConnConfig.RuntimeParams["application_name"] = applicationName

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Migrate applies embedded SQL migrations in filename order inside a
// transaction and records applied versions in schema_migrations.
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(postgresmigrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	type migration struct {
		name    string
		version int64
		script  string
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := migrationVersion(entry.Name())
		if err != nil {
			return err
		}
		raw, err := fs.ReadFile(postgresmigrations.FS, entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{name: entry.Name(), version: version, script: string(raw)})
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version bigint PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	for _, m := range migrations {
		var applied bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", m.version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", m.name, err)
		}
		if applied {
			continue
		}
		if _, err := tx.Exec(ctx, m.script); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES ($1, now())", m.version); err != nil {
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	if _, err := s.pool.Exec(ctx, "SELECT patchproof_ensure_partitions($1)", partitionAheadMonths); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}
	return nil
}

// Close releases all pooled connections.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// Ping verifies that the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func migrationVersion(name string) (int64, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("migration %s has no version prefix", name)
	}
	version, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migration %s has an invalid version prefix: %w", name, err)
	}
	return version, nil
}
