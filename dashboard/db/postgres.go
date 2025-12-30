package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/lib/pq"
)

type Run struct {
	ID         int
	StartedAt  string
	EndedAt    string
	Namespace  string
	Mode       string
	Status     string // ok, fixed, failed, running
	PodCount   int
	ErrorCount int
	FixCount   int
	Report     string
	Log        string
}

type Fix struct {
	ID           int
	RunID        int
	Timestamp    string
	Namespace    string
	PodName      string
	ErrorType    string
	ErrorMessage string
	FixApplied   string
	Status       string
}

type NamespaceStats struct {
	Namespace  string
	RunCount   int
	OkCount    int
	FixedCount int
	FailedCount int
}

type DB struct {
	conn *sql.DB
}

// New creates a new database connection using PostgreSQL DSN
func New(dsn string) (*DB, error) {
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}

	// Test the connection
	if err := conn.Ping(); err != nil {
		return nil, err
	}

	// Tables are created by migrations, not here
	return &DB{conn: conn}, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

// Run operations

func (db *DB) CreateRun(namespace, mode string) (int64, error) {
	var id int64
	err := db.conn.QueryRow(`
		INSERT INTO clopus_watcher_runs (started_at, namespace, mode, status)
		VALUES (NOW(), $1, $2, 'running')
		RETURNING id
	`, namespace, mode).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (db *DB) CompleteRun(id int64, status string, podCount, errorCount, fixCount int, report, log string) error {
	_, err := db.conn.Exec(`
		UPDATE clopus_watcher_runs SET
			ended_at = NOW(),
			status = $1,
			pod_count = $2,
			error_count = $3,
			fix_count = $4,
			report = $5,
			log = $6
		WHERE id = $7
	`, status, podCount, errorCount, fixCount, report, log, id)
	return err
}

func (db *DB) GetRuns(namespace string, limit int) ([]Run, error) {
	query := `
		SELECT id, started_at::text, COALESCE(ended_at::text, ''), namespace, mode, status,
		       pod_count, error_count, fix_count, COALESCE(report, ''), COALESCE(log, '')
		FROM clopus_watcher_runs
	`
	args := []interface{}{}
	argIdx := 1

	if namespace != "" {
		query += fmt.Sprintf(" WHERE namespace = $%d", argIdx)
		args = append(args, namespace)
		argIdx++
	}

	query += fmt.Sprintf(" ORDER BY started_at DESC LIMIT $%d", argIdx)
	args = append(args, limit)

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []Run
	for rows.Next() {
		var r Run
		err := rows.Scan(&r.ID, &r.StartedAt, &r.EndedAt, &r.Namespace, &r.Mode,
			&r.Status, &r.PodCount, &r.ErrorCount, &r.FixCount, &r.Report, &r.Log)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}

func (db *DB) GetRun(id int) (*Run, error) {
	var r Run
	err := db.conn.QueryRow(`
		SELECT id, started_at::text, COALESCE(ended_at::text, ''), namespace, mode, status,
		       pod_count, error_count, fix_count, COALESCE(report, ''), COALESCE(log, '')
		FROM clopus_watcher_runs WHERE id = $1
	`, id).Scan(&r.ID, &r.StartedAt, &r.EndedAt, &r.Namespace, &r.Mode,
		&r.Status, &r.PodCount, &r.ErrorCount, &r.FixCount, &r.Report, &r.Log)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (db *DB) GetLastRunTime(namespace string) (string, error) {
	var lastRun string
	err := db.conn.QueryRow(`
		SELECT COALESCE(MAX(ended_at)::text, '') FROM clopus_watcher_runs WHERE namespace = $1 AND status != 'running'
	`, namespace).Scan(&lastRun)
	return lastRun, err
}

// Namespace operations

func (db *DB) GetNamespaces() ([]NamespaceStats, error) {
	rows, err := db.conn.Query(`
		SELECT
			namespace,
			COUNT(*) as run_count,
			SUM(CASE WHEN status = 'ok' THEN 1 ELSE 0 END) as ok_count,
			SUM(CASE WHEN status = 'fixed' THEN 1 ELSE 0 END) as fixed_count,
			SUM(CASE WHEN status = 'failed' OR status = 'issues_found' THEN 1 ELSE 0 END) as failed_count
		FROM clopus_watcher_runs
		GROUP BY namespace
		ORDER BY namespace
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []NamespaceStats
	for rows.Next() {
		var s NamespaceStats
		err := rows.Scan(&s.Namespace, &s.RunCount, &s.OkCount, &s.FixedCount, &s.FailedCount)
		if err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, nil
}

func (db *DB) GetNamespaceStats(namespace string) (*NamespaceStats, error) {
	var s NamespaceStats
	s.Namespace = namespace

	err := db.conn.QueryRow(`SELECT COUNT(*) FROM clopus_watcher_runs WHERE namespace = $1`, namespace).Scan(&s.RunCount)
	if err != nil {
		return nil, err
	}
	// Count 'ok' status as ok
	db.conn.QueryRow(`SELECT COUNT(*) FROM clopus_watcher_runs WHERE namespace = $1 AND status = 'ok'`, namespace).Scan(&s.OkCount)
	// Count 'fixed' status as fixed
	db.conn.QueryRow(`SELECT COUNT(*) FROM clopus_watcher_runs WHERE namespace = $1 AND status = 'fixed'`, namespace).Scan(&s.FixedCount)
	// Count 'failed' and 'issues_found' as failed (issues that need attention)
	db.conn.QueryRow(`SELECT COUNT(*) FROM clopus_watcher_runs WHERE namespace = $1 AND (status = 'failed' OR status = 'issues_found')`, namespace).Scan(&s.FailedCount)

	return &s, nil
}

// Fix operations

func (db *DB) GetFixes(limit int) ([]Fix, error) {
	rows, err := db.conn.Query(`
		SELECT id, COALESCE(run_id, 0), timestamp::text, namespace, pod_name, error_type,
		       COALESCE(error_message, ''), COALESCE(fix_applied, ''), status
		FROM clopus_watcher_fixes
		ORDER BY timestamp DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var fixes []Fix
	for rows.Next() {
		var f Fix
		err := rows.Scan(&f.ID, &f.RunID, &f.Timestamp, &f.Namespace, &f.PodName,
			&f.ErrorType, &f.ErrorMessage, &f.FixApplied, &f.Status)
		if err != nil {
			return nil, err
		}
		fixes = append(fixes, f)
	}
	return fixes, nil
}

func (db *DB) GetFixesByRun(runID int) ([]Fix, error) {
	rows, err := db.conn.Query(`
		SELECT id, COALESCE(run_id, 0), timestamp::text, namespace, pod_name, error_type,
		       COALESCE(error_message, ''), COALESCE(fix_applied, ''), status
		FROM clopus_watcher_fixes
		WHERE run_id = $1
		ORDER BY timestamp DESC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var fixes []Fix
	for rows.Next() {
		var f Fix
		err := rows.Scan(&f.ID, &f.RunID, &f.Timestamp, &f.Namespace, &f.PodName,
			&f.ErrorType, &f.ErrorMessage, &f.FixApplied, &f.Status)
		if err != nil {
			return nil, err
		}
		fixes = append(fixes, f)
	}
	return fixes, nil
}

func (db *DB) GetStats() (total, success, failed, pending int, err error) {
	err = db.conn.QueryRow("SELECT COUNT(*) FROM clopus_watcher_fixes").Scan(&total)
	if err != nil {
		return
	}
	err = db.conn.QueryRow("SELECT COUNT(*) FROM clopus_watcher_fixes WHERE status = 'success'").Scan(&success)
	if err != nil {
		return
	}
	err = db.conn.QueryRow("SELECT COUNT(*) FROM clopus_watcher_fixes WHERE status = 'failed'").Scan(&failed)
	if err != nil {
		return
	}
	err = db.conn.QueryRow("SELECT COUNT(*) FROM clopus_watcher_fixes WHERE status = 'pending' OR status = 'analyzing'").Scan(&pending)
	return
}

// ImportJSONResults imports watcher results from JSON files to PostgreSQL
// Scans resultsDir for run_*.json files and inserts them into the database
func (db *DB) ImportJSONResults(resultsDir string) error {
	files, err := filepath.Glob(filepath.Join(resultsDir, "run_*.json"))
	if err != nil {
		return err
	}

	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue // Skip files that can't be read
		}

		var result struct {
			ID         int64  `json:"id"`
			StartedAt  string `json:"started_at"`
			EndedAt    string `json:"ended_at"`
			Namespace  string `json:"namespace"`
			Mode       string `json:"mode"`
			Status     string `json:"status"`
			PodCount   int    `json:"pod_count"`
			ErrorCount int    `json:"error_count"`
			FixCount   int    `json:"fix_count"`
			Report     string `json:"report"`
			Log        string `json:"log"`
		}

		if err := json.Unmarshal(data, &result); err != nil {
			continue // Skip invalid JSON files
		}

		// Check if run already exists
		var exists bool
		err = db.conn.QueryRow("SELECT EXISTS(SELECT 1 FROM clopus_watcher_runs WHERE id = $1)", result.ID).Scan(&exists)
		if err != nil || exists {
			continue // Skip if already imported
		}

		// Parse timestamps
		startedAt := result.StartedAt
		if startedAt == "" {
			startedAt = time.Now().Format(time.RFC3339)
		}
		endedAt := result.EndedAt
		if endedAt == "" {
			endedAt = time.Now().Format(time.RFC3339)
		}

		// Insert run record
		_, err = db.conn.Exec(`
			INSERT INTO clopus_watcher_runs (id, started_at, ended_at, namespace, mode, status, pod_count, error_count, fix_count, report, log)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, result.ID, startedAt, endedAt, result.Namespace, result.Mode, result.Status, result.PodCount, result.ErrorCount, result.FixCount, result.Report, result.Log)

		if err != nil {
			continue // Skip files that fail to import
		}
	}

	return nil
}
