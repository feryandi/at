package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS apps (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    domain         TEXT NOT NULL UNIQUE,
    container_port INTEGER NOT NULL DEFAULT 8080,
    host_port      INTEGER NOT NULL UNIQUE,
    env_vars       TEXT NOT NULL DEFAULT '{}',
    created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS deployments (
    id           TEXT PRIMARY KEY,
    app_id       TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    commit_sha   TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending',
    image_id     TEXT NOT NULL DEFAULT '',
    container_id TEXT NOT NULL DEFAULT '',
    logs         TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    started_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    finished_at  DATETIME
);

CREATE INDEX IF NOT EXISTS idx_dep_app ON deployments(app_id, started_at DESC);
`

const (
	StatusPending  = "pending"
	StatusBuilding = "building"
	StatusRunning  = "running"
	StatusFailed   = "failed"
	StatusStopped  = "stopped"
)

type App struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Domain        string    `json:"domain"`
	ContainerPort int       `json:"container_port"`
	HostPort      int       `json:"host_port"`
	EnvVars       string    `json:"env_vars"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (a *App) EnvMap() (map[string]string, error) {
	m := make(map[string]string)
	if a.EnvVars == "" || a.EnvVars == "{}" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(a.EnvVars), &m); err != nil {
		return nil, fmt.Errorf("parsing env_vars: %w", err)
	}
	return m, nil
}

type Deployment struct {
	ID          string     `json:"id"`
	AppID       string     `json:"app_id"`
	CommitSHA   string     `json:"commit_sha"`
	Status      string     `json:"status"`
	ImageID     string     `json:"image_id"`
	ContainerID string     `json:"container_id"`
	Logs        string     `json:"logs"`
	Error       string     `json:"error"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

type DB struct {
	sql            *sql.DB
	portRangeStart int
}

func New(path string, portRangeStart int) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	d := &DB{sql: db, portRangeStart: portRangeStart}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return d, nil
}

// migrate drops legacy git-related columns from the apps table if they exist.
func (d *DB) migrate() error {
	var count int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('apps') WHERE name='git_url'`).Scan(&count); err != nil {
		return fmt.Errorf("migration check: %w", err)
	}
	if count == 0 {
		return nil
	}
	log.Println("store: migrating schema — removing git columns from apps table")
	for _, col := range []string{"git_url", "git_branch", "webhook_secret"} {
		if _, err := d.sql.Exec(fmt.Sprintf("ALTER TABLE apps DROP COLUMN %s", col)); err != nil {
			if !strings.Contains(err.Error(), "no such column") {
				return fmt.Errorf("drop column %s: %w", col, err)
			}
		}
	}
	return nil
}

func (d *DB) Close() error {
	return d.sql.Close()
}

// CreateApp inserts a new app and auto-assigns HostPort.
// The MAX(host_port) query and INSERT are wrapped in a transaction to prevent
// two concurrent scans from assigning the same port.
func (d *DB) CreateApp(app *App) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var maxPort sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(host_port) FROM apps`).Scan(&maxPort); err != nil {
		return fmt.Errorf("query max port: %w", err)
	}

	if maxPort.Valid {
		app.HostPort = int(maxPort.Int64) + 1
	} else {
		app.HostPort = d.portRangeStart
	}

	now := time.Now().UTC()
	app.CreatedAt = now
	app.UpdatedAt = now

	if app.EnvVars == "" {
		app.EnvVars = "{}"
	}
	if app.ContainerPort == 0 {
		app.ContainerPort = 8080
	}

	_, err = tx.Exec(`
		INSERT INTO apps (id, name, domain, container_port, host_port, env_vars, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		app.ID, app.Name, app.Domain,
		app.ContainerPort, app.HostPort, app.EnvVars,
		app.CreatedAt, app.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert app: %w", err)
	}
	return tx.Commit()
}

func (d *DB) GetApp(id string) (*App, error) {
	return d.scanApp(d.sql.QueryRow(`
		SELECT id, name, domain, container_port, host_port, env_vars, created_at, updated_at
		FROM apps WHERE id = ?`, id))
}

func (d *DB) GetAppByName(name string) (*App, error) {
	return d.scanApp(d.sql.QueryRow(`
		SELECT id, name, domain, container_port, host_port, env_vars, created_at, updated_at
		FROM apps WHERE name = ?`, name))
}

func (d *DB) ListApps() ([]*App, error) {
	rows, err := d.sql.Query(`
		SELECT id, name, domain, container_port, host_port, env_vars, created_at, updated_at
		FROM apps ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer rows.Close()

	var apps []*App
	for rows.Next() {
		app, err := d.scanApp(rows)
		if err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

func (d *DB) UpdateApp(app *App) error {
	app.UpdatedAt = time.Now().UTC()
	_, err := d.sql.Exec(`
		UPDATE apps SET name=?, domain=?, container_port=?, env_vars=?, updated_at=?
		WHERE id=?`,
		app.Name, app.Domain,
		app.ContainerPort, app.EnvVars,
		app.UpdatedAt, app.ID,
	)
	if err != nil {
		return fmt.Errorf("update app: %w", err)
	}
	return nil
}

func (d *DB) DeleteApp(id string) error {
	_, err := d.sql.Exec(`DELETE FROM apps WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete app: %w", err)
	}
	return nil
}

// scanApp works for both *sql.Row and *sql.Rows via the scanner interface.
type scanner interface {
	Scan(dest ...any) error
}

func (d *DB) scanApp(row scanner) (*App, error) {
	var app App
	err := row.Scan(
		&app.ID, &app.Name, &app.Domain,
		&app.ContainerPort, &app.HostPort, &app.EnvVars,
		&app.CreatedAt, &app.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan app: %w", err)
	}
	return &app, nil
}

func (d *DB) CreateDeployment(dep *Deployment) error {
	if dep.StartedAt.IsZero() {
		dep.StartedAt = time.Now().UTC()
	}
	if dep.Status == "" {
		dep.Status = StatusPending
	}
	_, err := d.sql.Exec(`
		INSERT INTO deployments (id, app_id, commit_sha, status, image_id, container_id, logs, error, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		dep.ID, dep.AppID, dep.CommitSHA, dep.Status,
		dep.ImageID, dep.ContainerID, dep.Logs, dep.Error, dep.StartedAt,
	)
	if err != nil {
		return fmt.Errorf("insert deployment: %w", err)
	}
	return nil
}

func (d *DB) GetDeployment(id string) (*Deployment, error) {
	return d.scanDeployment(d.sql.QueryRow(`
		SELECT id, app_id, commit_sha, status, image_id, container_id, logs, error, started_at, finished_at
		FROM deployments WHERE id=?`, id))
}

func (d *DB) GetLatestDeployment(appID string) (*Deployment, error) {
	return d.scanDeployment(d.sql.QueryRow(`
		SELECT id, app_id, commit_sha, status, image_id, container_id, logs, error, started_at, finished_at
		FROM deployments WHERE app_id=? ORDER BY started_at DESC LIMIT 1`, appID))
}

// GetLatestDeploymentsByAppIDs returns the most recent deployment for each app ID in a single query.
func (d *DB) GetLatestDeploymentsByAppIDs(appIDs []string) (map[string]*Deployment, error) {
	if len(appIDs) == 0 {
		return map[string]*Deployment{}, nil
	}
	placeholders := strings.Repeat("?,", len(appIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(appIDs))
	for i, id := range appIDs {
		args[i] = id
	}
	rows, err := d.sql.Query(fmt.Sprintf(`
		SELECT id, app_id, commit_sha, status, image_id, container_id, logs, error, started_at, finished_at
		FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY app_id ORDER BY started_at DESC) AS rn
			FROM deployments WHERE app_id IN (%s)
		) WHERE rn = 1`, placeholders), args...)
	if err != nil {
		return nil, fmt.Errorf("get latest deployments: %w", err)
	}
	defer rows.Close()

	result := make(map[string]*Deployment, len(appIDs))
	for rows.Next() {
		dep, err := d.scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		if dep != nil {
			result[dep.AppID] = dep
		}
	}
	return result, rows.Err()
}

func (d *DB) ListDeployments(appID string, limit int) ([]*Deployment, error) {
	rows, err := d.sql.Query(`
		SELECT id, app_id, commit_sha, status, image_id, container_id, logs, error, started_at, finished_at
		FROM deployments WHERE app_id=? ORDER BY started_at DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()

	var deps []*Deployment
	for rows.Next() {
		dep, err := d.scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		deps = append(deps, dep)
	}
	return deps, rows.Err()
}

func (d *DB) UpdateDeploymentStatus(id, status, errMsg string) error {
	// Only set finished_at for terminal statuses; intermediate transitions
	// (e.g. pending→building) should leave it NULL.
	terminal := status == StatusRunning || status == StatusFailed || status == StatusStopped
	var err error
	if terminal {
		now := time.Now().UTC()
		_, err = d.sql.Exec(`
			UPDATE deployments SET status=?, error=?, finished_at=? WHERE id=?`,
			status, errMsg, now, id,
		)
	} else {
		_, err = d.sql.Exec(`
			UPDATE deployments SET status=?, error=? WHERE id=?`,
			status, errMsg, id,
		)
	}
	if err != nil {
		return fmt.Errorf("update deployment status: %w", err)
	}
	return nil
}

func (d *DB) AppendDeploymentLog(id, text string) error {
	_, err := d.sql.Exec(`
		UPDATE deployments SET logs = logs || ? WHERE id=?`, text, id)
	if err != nil {
		return fmt.Errorf("append deployment log: %w", err)
	}
	return nil
}

func (d *DB) SetDeploymentContainer(id, imageID, containerID string) error {
	_, err := d.sql.Exec(`
		UPDATE deployments SET image_id=?, container_id=? WHERE id=?`,
		imageID, containerID, id,
	)
	if err != nil {
		return fmt.Errorf("set deployment container: %w", err)
	}
	return nil
}

func (d *DB) GetRunningDeployments() ([]*Deployment, error) {
	rows, err := d.sql.Query(`
		SELECT id, app_id, commit_sha, status, image_id, container_id, logs, error, started_at, finished_at
		FROM deployments WHERE status=?`, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("get running deployments: %w", err)
	}
	defer rows.Close()

	var deps []*Deployment
	for rows.Next() {
		dep, err := d.scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		deps = append(deps, dep)
	}
	return deps, rows.Err()
}

func (d *DB) scanDeployment(row scanner) (*Deployment, error) {
	var dep Deployment
	var finishedAt sql.NullTime
	err := row.Scan(
		&dep.ID, &dep.AppID, &dep.CommitSHA, &dep.Status,
		&dep.ImageID, &dep.ContainerID, &dep.Logs, &dep.Error,
		&dep.StartedAt, &finishedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan deployment: %w", err)
	}
	if finishedAt.Valid {
		dep.FinishedAt = &finishedAt.Time
	}
	return &dep, nil
}
