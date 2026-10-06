// Package store persists relay settings, jobs, per-device checkpoints and
// response tallies in a local SQLite database.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  status TEXT NOT NULL,
  target_url TEXT NOT NULL,
  mode TEXT NOT NULL DEFAULT 'forward',
  headers TEXT NOT NULL DEFAULT '{}',
  range_from TEXT NOT NULL,
  range_to TEXT NOT NULL,
  concurrency INTEGER NOT NULL,
  rate_limit INTEGER NOT NULL,
  timeout_seconds INTEGER NOT NULL,
  total INTEGER NOT NULL DEFAULT 0,
  sent INTEGER NOT NULL DEFAULT 0,
  rejected INTEGER NOT NULL DEFAULT 0,
  retries INTEGER NOT NULL DEFAULT 0,
  possible_duplicates INTEGER NOT NULL DEFAULT 0,
  waiting_since TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  started_at TEXT,
  finished_at TEXT
);
CREATE TABLE IF NOT EXISTS job_devices (
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  device_id INTEGER NOT NULL,
  name TEXT NOT NULL,
  unique_id TEXT NOT NULL,
  total INTEGER NOT NULL DEFAULT -1,
  sent INTEGER NOT NULL DEFAULT 0,
  rejected INTEGER NOT NULL DEFAULT 0,
  cursor_fixtime TEXT,
  cursor_id INTEGER NOT NULL DEFAULT 0,
  inflight_id INTEGER NOT NULL DEFAULT 0,
  inflight_attempts INTEGER NOT NULL DEFAULT 0,
  done INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (job_id, device_id)
);
CREATE TABLE IF NOT EXISTS job_responses (
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  outcome TEXT NOT NULL,
  status INTEGER NOT NULL,
  message TEXT NOT NULL,
  count INTEGER NOT NULL,
  PRIMARY KEY (job_id, outcome, status, message)
);
CREATE TABLE IF NOT EXISTS job_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  at TEXT NOT NULL,
  level TEXT NOT NULL,
  message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS job_events_job ON job_events(job_id, id);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer; SQLite serializes anyway
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	// databases created before these columns existed
	for _, ddl := range []string{
		"ALTER TABLE job_devices ADD COLUMN inflight_id INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE job_devices ADD COLUMN inflight_attempts INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE jobs ADD COLUMN mode TEXT NOT NULL DEFAULT 'forward'",
	} {
		if _, err := db.Exec(ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const timeFmt = time.RFC3339Nano

func ts(t time.Time) string { return t.UTC().Format(timeFmt) }

func parseTS(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(timeFmt, s.String)
	if err != nil {
		return nil
	}
	return &t
}

// ---- settings ----

func (s *Store) GetSetting(ctx context.Context, key string, v any) (bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

func (s *Store) PutSetting(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, string(raw))
	return err
}

// ---- jobs ----

const (
	StatusPending   = "pending"
	StatusCounting  = "counting"
	StatusRunning   = "running"
	StatusPaused    = "paused"
	StatusCompleted = "completed"
	StatusCancelled = "cancelled"
	StatusFailed    = "failed"
)

// Delivery modes.
const (
	// ModeForward posts one position per request, exactly as Traccar's forward.type=json.
	ModeForward = "forward"
	// ModeImport posts batches to the PowerFleet Server history import (POST /api/server/import),
	// which stores history only and skips positions it already has.
	ModeImport = "import"
)

type Job struct {
	ID                 int64             `json:"id"`
	Name               string            `json:"name"`
	Status             string            `json:"status"`
	TargetURL          string            `json:"targetUrl"`
	Mode               string            `json:"mode"` // ModeForward or ModeImport
	Headers            map[string]string `json:"headers"`
	From               time.Time         `json:"from"`
	To                 time.Time         `json:"to"`
	Concurrency        int               `json:"concurrency"`
	RateLimit          int               `json:"rateLimit"`
	TimeoutSeconds     int               `json:"timeoutSeconds"`
	Total              int64             `json:"total"`
	Sent               int64             `json:"sent"`
	Rejected           int64             `json:"rejected"`
	Retries            int64             `json:"retries"`
	PossibleDuplicates int64             `json:"possibleDuplicates"`
	WaitingSince       *time.Time        `json:"waitingSince"`
	LastError          string            `json:"lastError"`
	CreatedAt          time.Time         `json:"createdAt"`
	StartedAt          *time.Time        `json:"startedAt"`
	FinishedAt         *time.Time        `json:"finishedAt"`
	Devices            int64             `json:"devices"`
}

type JobDevice struct {
	JobID    int64      `json:"-"`
	DeviceID int64      `json:"deviceId"`
	Name     string     `json:"name"`
	UniqueID string     `json:"uniqueId"`
	Total    int64      `json:"total"`
	Sent     int64      `json:"sent"`
	Rejected int64      `json:"rejected"`
	CursorAt *time.Time `json:"cursorAt"`
	CursorID int64      `json:"cursorId"`
	// InflightID is the position being delivered; non-zero after an unclean stop
	InflightID int64 `json:"-"`
	// InflightAttempts counts earlier attempts for that position that may have been stored
	InflightAttempts int64 `json:"-"`
	Done             bool  `json:"done"`
}

func (s *Store) CreateJob(ctx context.Context, j Job, devices []JobDevice) (int64, error) {
	headers, _ := json.Marshal(j.Headers)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if j.Mode == "" {
		j.Mode = ModeForward
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO jobs (name, status, target_url, mode, headers, range_from, range_to, concurrency,
		rate_limit, timeout_seconds, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.Name, StatusPending, j.TargetURL, j.Mode, string(headers), ts(j.From), ts(j.To), j.Concurrency, j.RateLimit,
		j.TimeoutSeconds, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	for _, d := range devices {
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_devices (job_id, device_id, name, unique_id, total) VALUES (?, ?, ?, ?, ?)`,
			id, d.DeviceID, d.Name, d.UniqueID, d.Total); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

const jobColumns = `j.id, j.name, j.status, j.target_url, j.mode, j.headers, j.range_from, j.range_to, j.concurrency, j.rate_limit,
	j.timeout_seconds, j.total,
	(SELECT COALESCE(SUM(d.sent), 0) FROM job_devices d WHERE d.job_id = j.id),
	(SELECT COALESCE(SUM(d.rejected), 0) FROM job_devices d WHERE d.job_id = j.id), j.retries, j.possible_duplicates, j.waiting_since, j.last_error,
	j.created_at, j.started_at, j.finished_at, (SELECT COUNT(*) FROM job_devices d WHERE d.job_id = j.id)`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var (
		j                          Job
		headers, from, to, created string
		waiting, started, finished sql.NullString
	)
	err := row.Scan(&j.ID, &j.Name, &j.Status, &j.TargetURL, &j.Mode, &headers, &from, &to, &j.Concurrency, &j.RateLimit,
		&j.TimeoutSeconds, &j.Total, &j.Sent, &j.Rejected, &j.Retries, &j.PossibleDuplicates, &waiting, &j.LastError,
		&created, &started, &finished, &j.Devices)
	if err != nil {
		return j, err
	}
	_ = json.Unmarshal([]byte(headers), &j.Headers)
	j.From, _ = time.Parse(timeFmt, from)
	j.To, _ = time.Parse(timeFmt, to)
	j.CreatedAt, _ = time.Parse(timeFmt, created)
	j.WaitingSince, j.StartedAt, j.FinishedAt = parseTS(waiting), parseTS(started), parseTS(finished)
	return j, nil
}

func (s *Store) Job(ctx context.Context, id int64) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs j WHERE j.id = ?", id))
}

func (s *Store) Jobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs j ORDER BY j.id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) JobsWithStatus(ctx context.Context, statuses ...string) ([]Job, error) {
	all, err := s.Jobs(ctx)
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, j := range all {
		for _, st := range statuses {
			if j.Status == st {
				out = append(out, j)
			}
		}
	}
	return out, nil
}

// UpdateJobTarget changes where and how a job delivers (URL, headers incl. the API key, pacing).
// Progress and checkpoints are untouched, so a resumed job continues where it stopped.
func (s *Store) UpdateJobTarget(ctx context.Context, id int64, targetURL string, headers map[string]string, concurrency, rateLimit, timeoutSeconds int) error {
	h, _ := json.Marshal(headers)
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET target_url = ?, headers = ?, concurrency = ?, rate_limit = ?, timeout_seconds = ? WHERE id = ?`,
		targetURL, string(h), concurrency, rateLimit, timeoutSeconds, id)
	return err
}

func (s *Store) SetStatus(ctx context.Context, id int64, status string) error {
	q := "UPDATE jobs SET status = ?"
	args := []any{status}
	switch status {
	case StatusRunning, StatusCounting:
		q += ", started_at = COALESCE(started_at, ?), finished_at = NULL"
		args = append(args, ts(time.Now()))
	case StatusCompleted, StatusCancelled, StatusFailed:
		q += ", finished_at = ?, waiting_since = NULL"
		args = append(args, ts(time.Now()))
	case StatusPaused:
		q += ", waiting_since = NULL"
	}
	_, err := s.db.ExecContext(ctx, q+" WHERE id = ?", append(args, id)...)
	return err
}

func (s *Store) SetTotal(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE jobs SET total = (SELECT COALESCE(SUM(MAX(total, 0)), 0) FROM job_devices WHERE job_id = ?) WHERE id = ?", id, id)
	return err
}

func (s *Store) SetDeviceTotal(ctx context.Context, jobID, deviceID, total int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE job_devices SET total = ? WHERE job_id = ? AND device_id = ?", total, jobID, deviceID)
	return err
}

func (s *Store) JobDevices(ctx context.Context, jobID int64) ([]JobDevice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, device_id, name, unique_id, total, sent, rejected, cursor_fixtime,
		cursor_id, inflight_id, inflight_attempts, done FROM job_devices WHERE job_id = ? ORDER BY name, device_id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobDevice
	for rows.Next() {
		var d JobDevice
		var cursor sql.NullString
		if err := rows.Scan(&d.JobID, &d.DeviceID, &d.Name, &d.UniqueID, &d.Total, &d.Sent, &d.Rejected, &cursor,
			&d.CursorID, &d.InflightID, &d.InflightAttempts, &d.Done); err != nil {
			return nil, err
		}
		d.CursorAt = parseTS(cursor)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Progress is a batch of state changes flushed in one transaction.
// Progress is a batch of job-level counters; per-device checkpoints are written immediately.
type Progress struct {
	JobID              int64
	Retries            int64
	PossibleDuplicates int64
	Responses          map[ResponseKey]int64
	WaitingSince       *time.Time
	ClearWaiting       bool
	LastError          *string
}

type ResponseKey struct {
	Outcome string // "sent" or "rejected"
	Status  int
	Message string
}

func (s *Store) Flush(ctx context.Context, p Progress) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET retries = retries + ?, possible_duplicates = possible_duplicates + ? WHERE id = ?`,
		p.Retries, p.PossibleDuplicates, p.JobID); err != nil {
		return err
	}
	if p.WaitingSince != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET waiting_since = COALESCE(waiting_since, ?) WHERE id = ?", ts(*p.WaitingSince), p.JobID); err != nil {
			return err
		}
	} else if p.ClearWaiting {
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET waiting_since = NULL WHERE id = ?", p.JobID); err != nil {
			return err
		}
	}
	if p.LastError != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET last_error = ? WHERE id = ?", *p.LastError, p.JobID); err != nil {
			return err
		}
	}
	for k, n := range p.Responses {
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_responses (job_id, outcome, status, message, count) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(job_id, outcome, status, message) DO UPDATE SET count = count + excluded.count`,
			p.JobID, k.Outcome, k.Status, k.Message, n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkInflight records the position about to be sent, before the request goes out.
func (s *Store) MarkInflight(ctx context.Context, jobID, deviceID, positionID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE job_devices SET inflight_id = ?, inflight_attempts = 0 WHERE job_id = ? AND device_id = ?", positionID, jobID, deviceID)
	return err
}

// AddAmbiguousAttempt records a failed attempt that the target may nevertheless have stored.
func (s *Store) AddAmbiguousAttempt(ctx context.Context, jobID, deviceID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE job_devices SET inflight_attempts = inflight_attempts + 1 WHERE job_id = ? AND device_id = ?", jobID, deviceID)
	return err
}

// StopInflight records that no request is in flight any more; earlier ambiguous attempts stay counted.
func (s *Store) StopInflight(ctx context.Context, jobID, deviceID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE job_devices SET inflight_id = 0 WHERE job_id = ? AND device_id = ?", jobID, deviceID)
	return err
}

// Advance checkpoints a position the target answered: the device resumes after it.
func (s *Store) Advance(ctx context.Context, jobID, deviceID int64, fixTime time.Time, positionID int64, rejected bool) error {
	sent, rej := 1, 0
	if rejected {
		sent, rej = 0, 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE job_devices SET sent = sent + ?, rejected = rejected + ?, cursor_fixtime = ?,
		cursor_id = ?, inflight_id = 0, inflight_attempts = 0 WHERE job_id = ? AND device_id = ?`, sent, rej, ts(fixTime), positionID, jobID, deviceID)
	return err
}

// AdvanceBatch checkpoints a batch the target answered: sent and rejected are its counts and
// the device resumes after positionID.
func (s *Store) AdvanceBatch(ctx context.Context, jobID, deviceID int64, fixTime time.Time, positionID, sent, rejected int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE job_devices SET sent = sent + ?, rejected = rejected + ?, cursor_fixtime = ?,
		cursor_id = ?, inflight_id = 0, inflight_attempts = 0 WHERE job_id = ? AND device_id = ?`, sent, rejected, ts(fixTime), positionID, jobID, deviceID)
	return err
}

func (s *Store) ClearInflight(ctx context.Context, jobID, deviceID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE job_devices SET inflight_id = 0, inflight_attempts = 0 WHERE job_id = ? AND device_id = ?", jobID, deviceID)
	return err
}

func (s *Store) MarkDeviceDone(ctx context.Context, jobID, deviceID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE job_devices SET done = 1, inflight_id = 0 WHERE job_id = ? AND device_id = ?", jobID, deviceID)
	return err
}

type Response struct {
	Outcome string `json:"outcome"`
	Status  int    `json:"status"`
	Message string `json:"message"`
	Count   int64  `json:"count"`
}

func (s *Store) Responses(ctx context.Context, jobID int64) ([]Response, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT outcome, status, message, count FROM job_responses WHERE job_id = ? ORDER BY count DESC", jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Response
	for rows.Next() {
		var r Response
		if err := rows.Scan(&r.Outcome, &r.Status, &r.Message, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Event struct {
	At      time.Time `json:"at"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

func (s *Store) AddEvent(ctx context.Context, jobID int64, level, message string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO job_events (job_id, at, level, message) VALUES (?, ?, ?, ?)", jobID, ts(time.Now()), level, message)
	return err
}

func (s *Store) Events(ctx context.Context, jobID int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT at, level, message FROM job_events WHERE job_id = ? ORDER BY id DESC LIMIT ?", jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at string
		if err := rows.Scan(&at, &e.Level, &e.Message); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(timeFmt, at)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", id)
	return err
}

// DB exposes the handle for tests.
func (s *Store) DB() *sql.DB { return s.db }
