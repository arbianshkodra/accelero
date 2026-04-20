package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore implements Store using SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) a SQLite database and runs migrations.
func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=ON")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return s, nil
}

func (s *SQLiteStore) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS stacks (
		id TEXT PRIMARY KEY,
		name TEXT UNIQUE NOT NULL,
		repo_url TEXT NOT NULL,
		repo_username TEXT NOT NULL DEFAULT '',
		repo_token TEXT NOT NULL DEFAULT '',
		repo_branch TEXT NOT NULL DEFAULT '',
		compose_path TEXT NOT NULL,
		service_filter TEXT NOT NULL DEFAULT '',
		auto_deploy INTEGER NOT NULL DEFAULT 0,
		reconcile_interval_seconds INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'active',
		last_deployed_at DATETIME,
		last_reconciled_at DATETIME,
		git_commit TEXT NOT NULL DEFAULT '',
		docker_username TEXT NOT NULL DEFAULT '',
		docker_password TEXT NOT NULL DEFAULT '',
		docker_registry TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS deployments (
		id TEXT PRIMARY KEY,
		stack_id TEXT NOT NULL,
		stack_name TEXT NOT NULL,
		status TEXT NOT NULL,
		trigger TEXT NOT NULL,
		git_commit TEXT NOT NULL DEFAULT '',
		changes TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		started_at DATETIME NOT NULL,
		completed_at DATETIME,
		FOREIGN KEY (stack_id) REFERENCES stacks(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_deployments_stack_id ON deployments(stack_id);
	CREATE INDEX IF NOT EXISTS idx_deployments_started_at ON deployments(started_at);

	CREATE TABLE IF NOT EXISTS managed_containers (
		id TEXT PRIMARY KEY,
		stack_id TEXT NOT NULL,
		service_name TEXT NOT NULL,
		container_id TEXT NOT NULL,
		container_name TEXT NOT NULL,
		image TEXT NOT NULL,
		status TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (stack_id) REFERENCES stacks(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_managed_containers_stack_id ON managed_containers(stack_id);
	CREATE INDEX IF NOT EXISTS idx_managed_containers_container_id ON managed_containers(container_id);

	-- audit_entries is append-only; no foreign keys on stack_id because
	-- entries outlive the stack they describe (that's the whole point of
	-- an audit trail). Retention is time-based, handled by the app, not
	-- cascaded from stack deletion.
	CREATE TABLE IF NOT EXISTS audit_entries (
		id TEXT PRIMARY KEY,
		timestamp DATETIME NOT NULL,
		actor TEXT NOT NULL,
		remote_addr TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		operation TEXT NOT NULL,
		resource_type TEXT NOT NULL DEFAULT '',
		resource_id TEXT NOT NULL DEFAULT '',
		stack_id TEXT NOT NULL DEFAULT '',
		stack_name TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL,
		error_message TEXT NOT NULL DEFAULT '',
		metadata TEXT NOT NULL DEFAULT '{}'
	);

	-- (timestamp desc) is the dominant read pattern for the listing
	-- endpoint; extra composite indexes cover the common filters.
	CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON audit_entries(timestamp DESC);
	CREATE INDEX IF NOT EXISTS idx_audit_stack_ts ON audit_entries(stack_id, timestamp DESC);
	CREATE INDEX IF NOT EXISTS idx_audit_actor_ts ON audit_entries(actor, timestamp DESC);
	CREATE INDEX IF NOT EXISTS idx_audit_operation_ts ON audit_entries(operation, timestamp DESC);
	`
	_, err := s.db.Exec(schema)
	return err
}

// --- Stack operations ---

func (s *SQLiteStore) CreateStack(stack *Stack) error {
	_, err := s.db.Exec(`
		INSERT INTO stacks (id, name, repo_url, repo_username, repo_token, repo_branch,
			compose_path, service_filter, auto_deploy, reconcile_interval_seconds, status,
			docker_username, docker_password, docker_registry, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		stack.ID, stack.Name, stack.RepoURL, stack.RepoUsername, stack.RepoToken,
		stack.RepoBranch, stack.ComposePath, stack.ServiceFilter,
		boolToInt(stack.AutoDeploy), stack.ReconcileInterval, stack.Status,
		stack.DockerUsername, stack.DockerPassword, stack.DockerRegistry,
		stack.CreatedAt, stack.UpdatedAt,
	)
	return err
}

func (s *SQLiteStore) GetStack(id string) (*Stack, error) {
	return s.scanStack(s.db.QueryRow(`SELECT * FROM stacks WHERE id = ?`, id))
}

func (s *SQLiteStore) GetStackByName(name string) (*Stack, error) {
	return s.scanStack(s.db.QueryRow(`SELECT * FROM stacks WHERE name = ?`, name))
}

func (s *SQLiteStore) ListStacks() ([]*Stack, error) {
	rows, err := s.db.Query(`SELECT * FROM stacks ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stacks []*Stack
	for rows.Next() {
		stack, err := s.scanStackFromRows(rows)
		if err != nil {
			return nil, err
		}
		stacks = append(stacks, stack)
	}
	return stacks, rows.Err()
}

func (s *SQLiteStore) UpdateStack(stack *Stack) error {
	stack.UpdatedAt = time.Now()
	_, err := s.db.Exec(`
		UPDATE stacks SET name=?, repo_url=?, repo_username=?, repo_token=?, repo_branch=?,
			compose_path=?, service_filter=?, auto_deploy=?, reconcile_interval_seconds=?,
			status=?, last_deployed_at=?, last_reconciled_at=?, git_commit=?,
			docker_username=?, docker_password=?, docker_registry=?, updated_at=?
		WHERE id=?`,
		stack.Name, stack.RepoURL, stack.RepoUsername, stack.RepoToken,
		stack.RepoBranch, stack.ComposePath, stack.ServiceFilter,
		boolToInt(stack.AutoDeploy), stack.ReconcileInterval,
		stack.Status, stack.LastDeployedAt, stack.LastReconciledAt, stack.GitCommit,
		stack.DockerUsername, stack.DockerPassword, stack.DockerRegistry,
		stack.UpdatedAt, stack.ID,
	)
	return err
}

func (s *SQLiteStore) DeleteStack(id string) error {
	_, err := s.db.Exec(`DELETE FROM stacks WHERE id = ?`, id)
	return err
}

// --- Deployment operations ---

func (s *SQLiteStore) CreateDeployment(d *Deployment) error {
	_, err := s.db.Exec(`
		INSERT INTO deployments (id, stack_id, stack_name, status, trigger, git_commit,
			changes, error_message, started_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.StackID, d.StackName, d.Status, d.Trigger,
		d.GitCommit, d.Changes, d.ErrorMessage, d.StartedAt, d.CompletedAt,
	)
	return err
}

func (s *SQLiteStore) GetDeployment(id string) (*Deployment, error) {
	row := s.db.QueryRow(`SELECT * FROM deployments WHERE id = ?`, id)
	return s.scanDeployment(row)
}

func (s *SQLiteStore) ListDeployments(stackID string, limit int) ([]*Deployment, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT * FROM deployments WHERE stack_id = ? ORDER BY started_at DESC LIMIT ?`,
		stackID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deployments []*Deployment
	for rows.Next() {
		d, err := s.scanDeploymentFromRows(rows)
		if err != nil {
			return nil, err
		}
		deployments = append(deployments, d)
	}
	return deployments, rows.Err()
}

func (s *SQLiteStore) UpdateDeployment(d *Deployment) error {
	_, err := s.db.Exec(`
		UPDATE deployments SET status=?, git_commit=?, changes=?,
			error_message=?, completed_at=?
		WHERE id=?`,
		d.Status, d.GitCommit, d.Changes, d.ErrorMessage, d.CompletedAt, d.ID,
	)
	return err
}

func (s *SQLiteStore) CleanupOldDeployments(maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	result, err := s.db.Exec(
		`DELETE FROM deployments WHERE started_at < ? AND status IN (?, ?, ?)`,
		cutoff, DeploymentCompleted, DeploymentFailed, DeploymentRolledBack,
	)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// --- Container tracking ---

func (s *SQLiteStore) TrackContainer(c *ManagedContainer) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO managed_containers (id, stack_id, service_name,
			container_id, container_name, image, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.StackID, c.ServiceName, c.ContainerID,
		c.ContainerName, c.Image, c.Status, c.CreatedAt,
	)
	return err
}

func (s *SQLiteStore) ListContainers(stackID string) ([]*ManagedContainer, error) {
	rows, err := s.db.Query(
		`SELECT * FROM managed_containers WHERE stack_id = ? ORDER BY created_at DESC`,
		stackID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var containers []*ManagedContainer
	for rows.Next() {
		c := &ManagedContainer{}
		if err := rows.Scan(&c.ID, &c.StackID, &c.ServiceName, &c.ContainerID,
			&c.ContainerName, &c.Image, &c.Status, &c.CreatedAt); err != nil {
			return nil, err
		}
		containers = append(containers, c)
	}
	return containers, rows.Err()
}

func (s *SQLiteStore) RemoveContainer(containerID string) error {
	_, err := s.db.Exec(`DELETE FROM managed_containers WHERE container_id = ?`, containerID)
	return err
}

func (s *SQLiteStore) RemoveContainersByStack(stackID string) error {
	_, err := s.db.Exec(`DELETE FROM managed_containers WHERE stack_id = ?`, stackID)
	return err
}

// --- Audit log ---

// maxAuditListLimit caps how many rows a single ListAuditEntries call
// can return. Protects against accidental unbounded loads; the table
// grows append-only so a bug or an aggressive crawler could otherwise
// pull millions of rows in one response.
const maxAuditListLimit = 1000

// CreateAuditEntry inserts a single audit row. Intentionally NOT
// transactional with the operation it audits — an audit write failure
// must never block or roll back the user-facing action. Callers log
// the error and move on.
func (s *SQLiteStore) CreateAuditEntry(e *AuditEntry) error {
	meta := "{}"
	if len(e.Metadata) > 0 {
		b, err := json.Marshal(e.Metadata)
		if err != nil {
			return fmt.Errorf("marshal audit metadata: %w", err)
		}
		meta = string(b)
	}
	_, err := s.db.Exec(
		`INSERT INTO audit_entries (
			id, timestamp, actor, remote_addr, request_id,
			operation, resource_type, resource_id,
			stack_id, stack_name,
			outcome, error_message, metadata
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.Timestamp, e.Actor, e.RemoteAddr, e.RequestID,
		e.Operation, e.ResourceType, e.ResourceID,
		e.StackID, e.StackName,
		e.Outcome, e.ErrorMessage, meta,
	)
	return err
}

// ListAuditEntries returns rows newest-first subject to the filter. An
// empty StackID/Actor/Operation means "any"; a zero Since means "no
// lower bound." Limit defaults to 100 if unset and is capped at
// maxAuditListLimit.
func (s *SQLiteStore) ListAuditEntries(filter AuditFilter) ([]*AuditEntry, error) {
	var (
		clauses []string
		args    []interface{}
	)
	if filter.StackID != "" {
		clauses = append(clauses, "stack_id = ?")
		args = append(args, filter.StackID)
	}
	if filter.StackName != "" {
		clauses = append(clauses, "stack_name = ?")
		args = append(args, filter.StackName)
	}
	if filter.Actor != "" {
		clauses = append(clauses, "actor = ?")
		args = append(args, filter.Actor)
	}
	if filter.Operation != "" {
		clauses = append(clauses, "operation = ?")
		args = append(args, filter.Operation)
	}
	if !filter.Since.IsZero() {
		clauses = append(clauses, "timestamp >= ?")
		args = append(args, filter.Since)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > maxAuditListLimit {
		limit = maxAuditListLimit
	}

	query := `SELECT id, timestamp, actor, remote_addr, request_id,
		operation, resource_type, resource_id,
		stack_id, stack_name,
		outcome, error_message, metadata
		FROM audit_entries`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*AuditEntry
	for rows.Next() {
		var (
			e       AuditEntry
			metaRaw string
		)
		if err := rows.Scan(
			&e.ID, &e.Timestamp, &e.Actor, &e.RemoteAddr, &e.RequestID,
			&e.Operation, &e.ResourceType, &e.ResourceID,
			&e.StackID, &e.StackName,
			&e.Outcome, &e.ErrorMessage, &metaRaw,
		); err != nil {
			return nil, err
		}
		if metaRaw != "" && metaRaw != "{}" {
			if err := json.Unmarshal([]byte(metaRaw), &e.Metadata); err != nil {
				// Corrupt metadata blob shouldn't block the list —
				// surface the row without its metadata and move on.
				e.Metadata = nil
			}
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// --- Lifecycle ---

// Ping verifies the database is reachable by running a trivial query under
// the caller's context/timeout. SQLite doesn't maintain a network connection,
// so this is really a "database file is open and readable" check — enough
// for a /readyz probe.
func (s *SQLiteStore) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// --- scan helpers ---

type scannable interface {
	Scan(dest ...interface{}) error
}

func (s *SQLiteStore) scanStack(row scannable) (*Stack, error) {
	st := &Stack{}
	var autoDeploy int
	var lastDeployed, lastReconciled sql.NullTime

	err := row.Scan(
		&st.ID, &st.Name, &st.RepoURL, &st.RepoUsername, &st.RepoToken,
		&st.RepoBranch, &st.ComposePath, &st.ServiceFilter,
		&autoDeploy, &st.ReconcileInterval, &st.Status,
		&lastDeployed, &lastReconciled, &st.GitCommit,
		&st.DockerUsername, &st.DockerPassword, &st.DockerRegistry,
		&st.CreatedAt, &st.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	st.AutoDeploy = autoDeploy != 0
	if lastDeployed.Valid {
		st.LastDeployedAt = &lastDeployed.Time
	}
	if lastReconciled.Valid {
		st.LastReconciledAt = &lastReconciled.Time
	}
	return st, nil
}

func (s *SQLiteStore) scanStackFromRows(rows *sql.Rows) (*Stack, error) {
	return s.scanStack(rows)
}

func (s *SQLiteStore) scanDeployment(row scannable) (*Deployment, error) {
	d := &Deployment{}
	var completedAt sql.NullTime

	err := row.Scan(
		&d.ID, &d.StackID, &d.StackName, &d.Status, &d.Trigger,
		&d.GitCommit, &d.Changes, &d.ErrorMessage, &d.StartedAt, &completedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	if completedAt.Valid {
		d.CompletedAt = &completedAt.Time
	}
	return d, nil
}

func (s *SQLiteStore) scanDeploymentFromRows(rows *sql.Rows) (*Deployment, error) {
	return s.scanDeployment(rows)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
