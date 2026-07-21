package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arbianshkodra/accelero/internal/secrets"
	_ "modernc.org/sqlite"
)

// ValidateBackupFile checks that path is a healthy Accelero database snapshot
// before it's accepted for restore: valid SQLite magic, passes
// PRAGMA integrity_check, and contains the core `stacks` table. It opens the
// file read-only and does not touch the live database.
func ValidateBackupFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open backup: %w", err)
	}
	magic := make([]byte, 16)
	n, _ := io.ReadFull(f, magic)
	f.Close()
	if n < 16 || string(magic) != "SQLite format 3\x00" {
		return fmt.Errorf("not a SQLite database (bad magic header)")
	}

	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open backup db: %w", err)
	}
	defer db.Close()

	var res string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&res); err != nil {
		return fmt.Errorf("integrity check could not run: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("integrity check failed: %s", res)
	}

	var name string
	if err := db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='stacks'",
	).Scan(&name); err != nil {
		return fmt.Errorf("not an Accelero backup (missing 'stacks' table): %w", err)
	}
	return nil
}

// SQLiteStore implements Store using SQLite.
type SQLiteStore struct {
	db *sql.DB

	// cipher optionally encrypts sensitive fields (repo_token,
	// docker_password) at rest. Nil means "encryption disabled" —
	// the store writes and reads plaintext exactly as older versions
	// did. Wired from cmd/main.go based on ACCELERO_ENCRYPTION_KEY.
	cipher *secrets.Cipher
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

// SetCipher attaches (or detaches) the at-rest encryption cipher.
// Passing nil disables encryption. Setter rather than constructor
// parameter so the many existing NewSQLiteStore call sites (tests
// included) don't have to change; production wires it once in
// cmd/main.go right after NewSQLiteStore.
func (s *SQLiteStore) SetCipher(c *secrets.Cipher) {
	s.cipher = c
}

// encryptField wraps secrets.Encrypt — safe to call on a store with
// nil cipher (returns input unchanged). Kept here rather than forcing
// every call site to branch on s.cipher.
func (s *SQLiteStore) encryptField(plaintext string) (string, error) {
	return s.cipher.Encrypt(plaintext)
}

// decryptField is the read-side counterpart. Returns input unchanged
// if the stored value is plaintext (legacy rows) OR if the cipher is
// disabled AND the value doesn't look like ciphertext. If the stored
// value looks like ciphertext (v1: prefix) but encryption is disabled,
// errors — the store must not return raw ciphertext as a credential.
func (s *SQLiteStore) decryptField(stored string) (string, error) {
	return s.cipher.Decrypt(stored)
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

	-- Note: new columns on the stacks table are added below via
	-- applyColumnMigrations, NOT here. SQLite doesn't support
	-- "ALTER TABLE ... ADD COLUMN IF NOT EXISTS", and we want CREATE
	-- TABLE IF NOT EXISTS to stay strictly idempotent for fresh
	-- installs. The pattern below also keeps fresh installs and
	-- upgrades running through the same code path.

	-- Per-stack secrets. value is ciphertext (v1: prefix) when the
	-- cipher is attached, plaintext otherwise, identical to how
	-- repo_token and docker_password are handled on the stacks table.
	-- Composite PK means "set twice with the same name = upsert".
	-- FK cascade deletes a stack's secrets along with it; there is no
	-- reason to outlive the parent row.
	CREATE TABLE IF NOT EXISTS stack_secrets (
		stack_id TEXT NOT NULL,
		name TEXT NOT NULL,
		value TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (stack_id, name),
		FOREIGN KEY (stack_id) REFERENCES stacks(id) ON DELETE CASCADE
	);

	-- Per-stack Docker registry credentials. password column matches
	-- stack_secrets.value: encrypted (v1: prefix) when a cipher is
	-- attached, plaintext otherwise. PK is (stack_id, server) so
	-- "POST /registries" with the same server overwrites — matches
	-- the stack_secrets upsert flow.
	CREATE TABLE IF NOT EXISTS stack_registries (
		stack_id TEXT NOT NULL,
		server TEXT NOT NULL,
		username TEXT NOT NULL,
		password TEXT NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (stack_id, server),
		FOREIGN KEY (stack_id) REFERENCES stacks(id) ON DELETE CASCADE
	);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	return s.applyColumnMigrations()
}

// applyColumnMigrations adds new columns to existing tables. SQLite
// has no `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, so each addition
// is gated by a pragma_table_info lookup. Idempotent — running on a
// fresh DB still works because the columns are absent until the first
// pass adds them, and present (so skipped) on every pass after.
//
// Adding a column to the END of a table is safe even though scanStack
// uses `SELECT *`: SQLite's ALTER TABLE ADD COLUMN appends to the
// existing column order, and the matching scanStack adjustment lands
// in the same commit as the migration entry below.
func (s *SQLiteStore) applyColumnMigrations() error {
	type addCol struct {
		table, col, def string
	}
	cols := []addCol{
		{"stacks", "secrets_hash", "TEXT NOT NULL DEFAULT ''"},
		{"stacks", "requires_approval", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, c := range cols {
		present, err := s.columnExists(c.table, c.col)
		if err != nil {
			return fmt.Errorf("inspect %s columns: %w", c.table, err)
		}
		if present {
			continue
		}
		stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, c.table, c.col, c.def)
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("add %s.%s: %w", c.table, c.col, err)
		}
	}
	return nil
}

func (s *SQLiteStore) columnExists(table, column string) (bool, error) {
	rows, err := s.db.Query(`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), nil
}

// --- Stack operations ---

func (s *SQLiteStore) CreateStack(stack *Stack) error {
	// Encrypt the two sensitive fields before they hit SQLite. The
	// caller's Stack struct is mutated so field values keep round-
	// tripping as plaintext Go strings — only the DB column sees
	// ciphertext.
	encToken, err := s.encryptField(stack.RepoToken)
	if err != nil {
		return fmt.Errorf("encrypt repo_token: %w", err)
	}
	encDockerPw, err := s.encryptField(stack.DockerPassword)
	if err != nil {
		return fmt.Errorf("encrypt docker_password: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO stacks (id, name, repo_url, repo_username, repo_token, repo_branch,
			compose_path, service_filter, auto_deploy, reconcile_interval_seconds, status,
			docker_username, docker_password, docker_registry, created_at, updated_at, secrets_hash,
			requires_approval)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		stack.ID, stack.Name, stack.RepoURL, stack.RepoUsername, encToken,
		stack.RepoBranch, stack.ComposePath, stack.ServiceFilter,
		boolToInt(stack.AutoDeploy), stack.ReconcileInterval, stack.Status,
		stack.DockerUsername, encDockerPw, stack.DockerRegistry,
		stack.CreatedAt, stack.UpdatedAt, stack.SecretsHash,
		boolToInt(stack.RequiresApproval),
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
	encToken, err := s.encryptField(stack.RepoToken)
	if err != nil {
		return fmt.Errorf("encrypt repo_token: %w", err)
	}
	encDockerPw, err := s.encryptField(stack.DockerPassword)
	if err != nil {
		return fmt.Errorf("encrypt docker_password: %w", err)
	}
	_, err = s.db.Exec(`
		UPDATE stacks SET name=?, repo_url=?, repo_username=?, repo_token=?, repo_branch=?,
			compose_path=?, service_filter=?, auto_deploy=?, reconcile_interval_seconds=?,
			status=?, last_deployed_at=?, last_reconciled_at=?, git_commit=?,
			docker_username=?, docker_password=?, docker_registry=?, updated_at=?, secrets_hash=?,
			requires_approval=?
		WHERE id=?`,
		stack.Name, stack.RepoURL, stack.RepoUsername, encToken,
		stack.RepoBranch, stack.ComposePath, stack.ServiceFilter,
		boolToInt(stack.AutoDeploy), stack.ReconcileInterval,
		stack.Status, stack.LastDeployedAt, stack.LastReconciledAt, stack.GitCommit,
		stack.DockerUsername, encDockerPw, stack.DockerRegistry,
		stack.UpdatedAt, stack.SecretsHash, boolToInt(stack.RequiresApproval), stack.ID,
	)
	return err
}

func (s *SQLiteStore) DeleteStack(id string) error {
	// modernc.org/sqlite doesn't honour _foreign_keys=ON in the DSN,
	// so the FK cascade from stack_secrets doesn't fire automatically.
	// Historical rows (deployments, managed_containers) get left as
	// orphans — not a security issue since they carry no credential
	// material. Secrets are the exception: those rows hold data that
	// should disappear when the parent stack does, even if a future
	// key rotation would turn them into unreadable ciphertext.
	if _, err := s.db.Exec(`DELETE FROM stack_secrets WHERE stack_id = ?`, id); err != nil {
		return fmt.Errorf("delete stack_secrets for %s: %w", id, err)
	}
	if _, err := s.db.Exec(`DELETE FROM stack_registries WHERE stack_id = ?`, id); err != nil {
		return fmt.Errorf("delete stack_registries for %s: %w", id, err)
	}
	_, err := s.db.Exec(`DELETE FROM stacks WHERE id = ?`, id)
	return err
}

// ListStacksNeedingEncryption returns IDs of stacks whose sensitive
// columns are still in pre-encryption plaintext form (no v1: prefix).
// Empty-string values (no token, no password) don't need encryption
// and are excluded. Used by the admin migration endpoint — callers
// then call UpdateStack on each to re-save with encryption.
func (s *SQLiteStore) ListStacksNeedingEncryption() ([]string, error) {
	const prefix = secrets.CipherVersionV1 + ":"
	rows, err := s.db.Query(`
		SELECT id FROM stacks
		WHERE (repo_token != '' AND repo_token NOT LIKE ?)
		   OR (docker_password != '' AND docker_password NOT LIKE ?)`,
		prefix+"%", prefix+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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

// GetPendingApproval returns the most recent deployment for a stack that is
// still awaiting approval, or (nil, nil) if there is none. Used to dedupe:
// a stack with requires_approval should hold at most one open approval at a
// time so a reconcile loop doesn't spawn (and notify) a new one every cycle.
func (s *SQLiteStore) GetPendingApproval(stackID string) (*Deployment, error) {
	row := s.db.QueryRow(
		`SELECT * FROM deployments WHERE stack_id = ? AND status = ?
		 ORDER BY started_at DESC LIMIT 1`,
		stackID, DeploymentPendingApproval,
	)
	return s.scanDeployment(row)
}

// ListPendingApprovals returns all deployments awaiting approval across every
// stack, oldest first (so a timeout sweep processes the longest-waiting one
// first, and the API surfaces the queue in arrival order).
func (s *SQLiteStore) ListPendingApprovals() ([]*Deployment, error) {
	rows, err := s.db.Query(
		`SELECT * FROM deployments WHERE status = ? ORDER BY started_at ASC`,
		DeploymentPendingApproval,
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

// --- Per-stack secrets ---

// UpsertStackSecret writes a secret, creating or updating by
// (stack_id, name). The value is encrypted before hitting the DB when a
// cipher is attached. created_at is preserved across updates so callers
// can see "when was this first set" vs "when was it last rotated".
func (s *SQLiteStore) UpsertStackSecret(sec *StackSecret) error {
	encValue, err := s.encryptField(sec.Value)
	if err != nil {
		return fmt.Errorf("encrypt stack_secret value: %w", err)
	}
	now := time.Now()
	if sec.CreatedAt.IsZero() {
		sec.CreatedAt = now
	}
	sec.UpdatedAt = now

	// SQLite's ON CONFLICT ... DO UPDATE handles the upsert. We pass
	// the same encValue on both paths and use excluded.updated_at so
	// the column moves forward even if the value text happens to
	// collide (re-encrypting the same plaintext produces different
	// ciphertext thanks to the random nonce, so the row will actually
	// change).
	_, err = s.db.Exec(`
		INSERT INTO stack_secrets (stack_id, name, value, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (stack_id, name) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at`,
		sec.StackID, sec.Name, encValue, sec.CreatedAt, sec.UpdatedAt)
	return err
}

// ListStackSecrets returns every secret for a stack with its plaintext
// value decrypted. Callers MUST NOT expose Value through unauthenticated
// surfaces — the handler redacts it in API responses; the deploy path
// materialises it into a tmpfs env_file.
func (s *SQLiteStore) ListStackSecrets(stackID string) ([]*StackSecret, error) {
	rows, err := s.db.Query(`
		SELECT stack_id, name, value, created_at, updated_at
		FROM stack_secrets
		WHERE stack_id = ?
		ORDER BY name ASC`,
		stackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*StackSecret
	for rows.Next() {
		var sec StackSecret
		if err := rows.Scan(&sec.StackID, &sec.Name, &sec.Value, &sec.CreatedAt, &sec.UpdatedAt); err != nil {
			return nil, err
		}
		plain, err := s.decryptField(sec.Value)
		if err != nil {
			return nil, fmt.Errorf("decrypt stack_secret value for %s/%s: %w", sec.StackID, sec.Name, err)
		}
		sec.Value = plain
		out = append(out, &sec)
	}
	return out, rows.Err()
}

// DeleteStackSecret removes a single (stack_id, name) row. Returns
// (false, nil) when the row did not exist so the handler can map to
// 404 without a second lookup.
func (s *SQLiteStore) DeleteStackSecret(stackID, name string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM stack_secrets WHERE stack_id = ? AND name = ?`, stackID, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- Per-stack registry credentials ---

// UpsertStackRegistry writes a credential, creating or updating by
// (stack_id, server). Password is encrypted before hitting the DB
// when a cipher is attached. Like UpsertStackSecret, created_at is
// preserved across updates.
func (s *SQLiteStore) UpsertStackRegistry(r *StackRegistry) error {
	encPassword, err := s.encryptField(r.Password)
	if err != nil {
		return fmt.Errorf("encrypt stack_registry password: %w", err)
	}
	now := time.Now()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now

	_, err = s.db.Exec(`
		INSERT INTO stack_registries (stack_id, server, username, password, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (stack_id, server) DO UPDATE SET
			username = excluded.username,
			password = excluded.password,
			updated_at = excluded.updated_at`,
		r.StackID, r.Server, r.Username, encPassword, r.CreatedAt, r.UpdatedAt)
	return err
}

// ListStackRegistries returns every registry credential for a stack
// with its plaintext password decrypted. The handler layer is
// responsible for redacting the password in API responses; the deploy
// path uses it to authenticate image pulls.
func (s *SQLiteStore) ListStackRegistries(stackID string) ([]*StackRegistry, error) {
	rows, err := s.db.Query(`
		SELECT stack_id, server, username, password, created_at, updated_at
		FROM stack_registries
		WHERE stack_id = ?
		ORDER BY server ASC`,
		stackID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*StackRegistry
	for rows.Next() {
		var r StackRegistry
		if err := rows.Scan(&r.StackID, &r.Server, &r.Username, &r.Password, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		plain, err := s.decryptField(r.Password)
		if err != nil {
			return nil, fmt.Errorf("decrypt stack_registry password for %s/%s: %w", r.StackID, r.Server, err)
		}
		r.Password = plain
		out = append(out, &r)
	}
	return out, rows.Err()
}

// DeleteStackRegistry removes a single (stack_id, server) row.
// Returns (false, nil) when the row did not exist.
func (s *SQLiteStore) DeleteStackRegistry(stackID, server string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM stack_registries WHERE stack_id = ? AND server = ?`, stackID, server)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
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

// CleanupOldAuditEntries drops audit rows older than maxAge. The only
// API that removes audit rows — the handler / recorder paths are
// strictly append-only. A zero or negative maxAge is a no-op so the
// caller can disable retention by setting AUDIT_MAX_AGE=0.
//
// The cutoff is computed in UTC to match how timestamps are stored
// (the recorder normalises to UTC at insert time). Using local time
// here would produce off-by-TZ comparisons on hosts that aren't on
// UTC — the underlying sqlite driver serialises time.Time to text
// and text comparisons are lexicographic.
func (s *SQLiteStore) CleanupOldAuditEntries(maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	result, err := s.db.Exec(`DELETE FROM audit_entries WHERE timestamp < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
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

// Backup writes a consistent snapshot of the database to destPath using
// SQLite's `VACUUM INTO`. Unlike copying the .db file, this is safe against
// a live WAL database (it checkpoints internally) and produces a compact,
// defragmented, self-contained copy. destPath must not already exist.
//
// The path is a server-generated temp path (never user input), but we still
// escape single quotes since VACUUM INTO takes a string literal, not a bound
// parameter.
func (s *SQLiteStore) Backup(ctx context.Context, destPath string) error {
	escaped := strings.ReplaceAll(destPath, "'", "''")
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		return fmt.Errorf("vacuum into %q: %w", destPath, err)
	}
	return nil
}

// --- scan helpers ---

type scannable interface {
	Scan(dest ...interface{}) error
}

func (s *SQLiteStore) scanStack(row scannable) (*Stack, error) {
	st := &Stack{}
	var autoDeploy, requiresApproval int
	var lastDeployed, lastReconciled sql.NullTime

	err := row.Scan(
		&st.ID, &st.Name, &st.RepoURL, &st.RepoUsername, &st.RepoToken,
		&st.RepoBranch, &st.ComposePath, &st.ServiceFilter,
		&autoDeploy, &st.ReconcileInterval, &st.Status,
		&lastDeployed, &lastReconciled, &st.GitCommit,
		&st.DockerUsername, &st.DockerPassword, &st.DockerRegistry,
		&st.CreatedAt, &st.UpdatedAt,
		// Columns added via applyColumnMigrations land at the end of the
		// table in the order they were added, so they scan here after the
		// base columns: secrets_hash, then requires_approval.
		&st.SecretsHash,
		&requiresApproval,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	st.AutoDeploy = autoDeploy != 0
	st.RequiresApproval = requiresApproval != 0
	if lastDeployed.Valid {
		st.LastDeployedAt = &lastDeployed.Time
	}
	if lastReconciled.Valid {
		st.LastReconciledAt = &lastReconciled.Time
	}

	// Decrypt the two fields we encrypted on write. Legacy (plaintext)
	// rows written by older accelero versions pass through unchanged;
	// ciphertext that fails to decrypt fails the whole read so callers
	// never get garbage credentials.
	if st.RepoToken, err = s.decryptField(st.RepoToken); err != nil {
		return nil, fmt.Errorf("decrypt repo_token for stack %s: %w", st.ID, err)
	}
	if st.DockerPassword, err = s.decryptField(st.DockerPassword); err != nil {
		return nil, fmt.Errorf("decrypt docker_password for stack %s: %w", st.ID, err)
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
