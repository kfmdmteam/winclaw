// Package scheduler provides a lightweight cron-style task scheduler that
// polls the database every 30 seconds and executes due tasks via a caller-
// supplied run function. No external scheduling libraries are required.
package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Task mirrors the scheduled_tasks database row.
type Task struct {
	ID        string
	SessionID string
	Name      string
	Schedule  string
	NextRun   time.Time
	LastRun   time.Time
	Status    string // "active" | "paused" | "cancelled"
	Prompt    string
}

// Scheduler polls the database for due tasks and executes them concurrently.
type Scheduler struct {
	db      *sql.DB
	runTask func(ctx context.Context, sessionID, prompt string) error
	ticker  *time.Ticker
	quit    chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex // guards quit/ticker lifecycle
}

const pollInterval = 30 * time.Second

// NewScheduler creates a Scheduler that delegates task execution to runTask.
// runTask is called in its own goroutine; it must respect the provided context.
func NewScheduler(db *sql.DB, runTask func(ctx context.Context, sessionID, prompt string) error) *Scheduler {
	return &Scheduler{
		db:      db,
		runTask: runTask,
		quit:    make(chan struct{}),
	}
}

// Start begins the poll loop. It blocks until ctx is cancelled or Stop is
// called. Intended to be run in a dedicated goroutine.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	s.ticker = time.NewTicker(pollInterval)
	s.mu.Unlock()

	// Fire once immediately so the first poll does not wait 30 seconds.
	s.poll(ctx)

	for {
		select {
		case <-s.ticker.C:
			s.poll(ctx)
		case <-s.quit:
			s.ticker.Stop()
			s.wg.Wait()
			return
		case <-ctx.Done():
			s.mu.Lock()
			if s.ticker != nil {
				s.ticker.Stop()
			}
			s.mu.Unlock()
			s.wg.Wait()
			return
		}
	}
}

// Stop signals the poll loop to exit cleanly and waits for in-flight tasks
// to complete before returning.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	select {
	case <-s.quit:
		// Already closed.
	default:
		close(s.quit)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// poll queries for tasks whose next_run is in the past, fires them, and
// advances their next_run timestamp (or marks them done for @once tasks).
func (s *Scheduler) poll(ctx context.Context) {
	now := time.Now().UTC()

	const q = `
		SELECT id, session_id, name, schedule, next_run, last_run, status, prompt
		FROM scheduled_tasks
		WHERE status = 'active' AND next_run <= ?`

	rows, err := s.db.QueryContext(ctx, q, now.Unix())
	if err != nil {
		// Log but do not crash; the next poll will retry.
		return
	}
	defer rows.Close()

	type dueTask struct {
		task   Task
		nextRun time.Time
	}

	var due []dueTask
	for rows.Next() {
		var (
			t           Task
			nextRunUnix int64
			lastRunUnix sql.NullInt64
		)
		if err := rows.Scan(
			&t.ID,
			&t.SessionID,
			&t.Name,
			&t.Schedule,
			&nextRunUnix,
			&lastRunUnix,
			&t.Status,
			&t.Prompt,
		); err != nil {
			continue
		}
		t.NextRun = time.Unix(nextRunUnix, 0).UTC()
		if lastRunUnix.Valid {
			t.LastRun = time.Unix(lastRunUnix.Int64, 0).UTC()
		}

		next, err := nextRun(t.Schedule, now)
		due = append(due, dueTask{task: t, nextRun: next})
		_ = err // nextRun errors handled below per task
	}
	if err := rows.Err(); err != nil {
		return
	}

	for _, dt := range due {
		dt := dt // capture loop variable
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.executeTask(ctx, dt.task, dt.nextRun, now)
		}()
	}
}

// executeTask runs a single task, then updates its database record.
func (s *Scheduler) executeTask(ctx context.Context, t Task, next time.Time, runAt time.Time) {
	_ = s.runTask(ctx, t.SessionID, t.Prompt)

	if t.Schedule == "@once" {
		// Mark @once tasks as cancelled so they never re-fire.
		// The schema CHECK constraint allows: active, paused, cancelled.
		const q = `UPDATE scheduled_tasks SET status = 'cancelled', last_run = ? WHERE id = ?`
		_, _ = s.db.ExecContext(ctx, q, runAt.Unix(), t.ID)
		return
	}

	var nextRunVal int64
	if next.IsZero() || next.Before(runAt) {
		// Schedule could not be parsed or next run is already in the past
		// (e.g., a one-shot expression). Advance by the poll interval to avoid
		// an infinite tight loop.
		nextRunVal = runAt.Add(pollInterval).Unix()
	} else {
		nextRunVal = next.Unix()
	}

	const q = `UPDATE scheduled_tasks SET last_run = ?, next_run = ? WHERE id = ?`
	_, _ = s.db.ExecContext(ctx, q, runAt.Unix(), nextRunVal, t.ID)
}

// Schedule inserts a new task into the database and returns its ID.
// schedule must be one of:
//   - a 5-field cron expression ("* * * * *")
//   - "@every <duration>" (e.g., "@every 1h", "@every 30m", "@every 90s")
//   - "@once" (runs once as soon as possible)
const (
	maxTaskNameLen = 128
	maxScheduleLen = 64
	maxPromptLen   = 10_000
)

func (s *Scheduler) Schedule(sessionID, name, schedule, prompt string) (string, error) {
	if len(name) == 0 || len(name) > maxTaskNameLen {
		return "", fmt.Errorf("scheduler: task name must be 1–%d characters", maxTaskNameLen)
	}
	if len(schedule) == 0 || len(schedule) > maxScheduleLen {
		return "", fmt.Errorf("scheduler: schedule expression must be 1–%d characters", maxScheduleLen)
	}
	if len(prompt) == 0 || len(prompt) > maxPromptLen {
		return "", fmt.Errorf("scheduler: prompt must be 1–%d characters", maxPromptLen)
	}

	now := time.Now().UTC()
	next, err := nextRun(schedule, now)
	if err != nil {
		return "", fmt.Errorf("scheduler: parse schedule %q: %w", schedule, err)
	}

	id := uuid.New().String()
	const q = `
		INSERT INTO scheduled_tasks
			(id, session_id, name, schedule, next_run, last_run, status, prompt, created_at)
		VALUES (?, ?, ?, ?, ?, 0, 'active', ?, ?)`
	if _, err := s.db.Exec(q, id, sessionID, name, schedule, next.Unix(), prompt, now.Unix()); err != nil {
		return "", fmt.Errorf("scheduler: insert task: %w", err)
	}
	return id, nil
}

// Pause sets a task's status to "paused" so it is skipped during polling.
func (s *Scheduler) Pause(id string) error {
	return s.setStatus(id, "paused")
}

// Resume re-activates a paused task. It recomputes next_run from now so the
// task fires at the next scheduled interval rather than immediately catching up.
func (s *Scheduler) Resume(id string) error {
	t, err := s.loadTask(id)
	if err != nil {
		return err
	}

	next, err := nextRun(t.Schedule, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("scheduler: recompute next_run for %q: %w", id, err)
	}

	const q = `UPDATE scheduled_tasks SET status = 'active', next_run = ? WHERE id = ?`
	res, err := s.db.Exec(q, next.Unix(), id)
	if err != nil {
		return fmt.Errorf("scheduler: resume %q: %w", id, err)
	}
	return checkRowsAffected(res, id)
}

// Cancel permanently deactivates a task.
func (s *Scheduler) Cancel(id string) error {
	return s.setStatus(id, "cancelled")
}

// List returns all tasks for sessionID, regardless of status.
func (s *Scheduler) List(sessionID string) ([]*Task, error) {
	const q = `
		SELECT id, session_id, name, schedule, next_run, last_run, status, prompt
		FROM scheduled_tasks
		WHERE session_id = ?
		ORDER BY next_run ASC`

	rows, err := s.db.Query(q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list tasks for %q: %w", sessionID, err)
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		var (
			t           Task
			nextRunUnix int64
			lastRunUnix sql.NullInt64
		)
		if err := rows.Scan(
			&t.ID,
			&t.SessionID,
			&t.Name,
			&t.Schedule,
			&nextRunUnix,
			&lastRunUnix,
			&t.Status,
			&t.Prompt,
		); err != nil {
			return nil, fmt.Errorf("scheduler: scan task: %w", err)
		}
		t.NextRun = time.Unix(nextRunUnix, 0).UTC()
		if lastRunUnix.Valid {
			t.LastRun = time.Unix(lastRunUnix.Int64, 0).UTC()
		}
		tasks = append(tasks, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduler: list rows: %w", err)
	}
	return tasks, nil
}

// setStatus is a shared helper for Pause and Cancel.
func (s *Scheduler) setStatus(id, status string) error {
	const q = `UPDATE scheduled_tasks SET status = ? WHERE id = ?`
	res, err := s.db.Exec(q, status, id)
	if err != nil {
		return fmt.Errorf("scheduler: set status %q on %q: %w", status, id, err)
	}
	return checkRowsAffected(res, id)
}

// loadTask fetches a single task by ID for internal use.
func (s *Scheduler) loadTask(id string) (*Task, error) {
	const q = `
		SELECT id, session_id, name, schedule, next_run, last_run, status, prompt
		FROM scheduled_tasks WHERE id = ?`

	var (
		t           Task
		nextRunUnix int64
		lastRunUnix sql.NullInt64
	)
	err := s.db.QueryRow(q, id).Scan(
		&t.ID, &t.SessionID, &t.Name, &t.Schedule,
		&nextRunUnix, &lastRunUnix, &t.Status, &t.Prompt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("scheduler: task not found: %q", id)
		}
		return nil, fmt.Errorf("scheduler: load task %q: %w", id, err)
	}
	t.NextRun = time.Unix(nextRunUnix, 0).UTC()
	if lastRunUnix.Valid {
		t.LastRun = time.Unix(lastRunUnix.Int64, 0).UTC()
	}
	return &t, nil
}

func checkRowsAffected(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("scheduler: task not found: %q", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Schedule expression parser
// ---------------------------------------------------------------------------

// nextRun parses schedule and returns the next time after from when the task
// should fire. It supports three forms:
//
//  1. "@once"               — returns from itself (fire immediately)
//  2. "@every <duration>"   — returns from + duration
//  3. Five-field cron       — "min hour dom mon dow" with * wildcards
func nextRun(schedule string, from time.Time) (time.Time, error) {
	schedule = strings.TrimSpace(schedule)

	switch {
	case schedule == "@once":
		return from, nil

	case strings.HasPrefix(schedule, "@every "):
		raw := strings.TrimPrefix(schedule, "@every ")
		d, err := parseDuration(raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("@every: %w", err)
		}
		if d <= 0 {
			return time.Time{}, errors.New("@every: duration must be positive")
		}
		return from.Add(d), nil

	default:
		return parseCron(schedule, from)
	}
}

// parseDuration extends time.ParseDuration to handle plain integer suffixes
// for days (d) and weeks (w) that the standard library does not support.
// All other values are forwarded to time.ParseDuration.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration string")
	}

	// Handle plain integer days ("2d") and weeks ("1w").
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid day count in %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	if strings.HasSuffix(s, "w") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "w"))
		if err != nil {
			return 0, fmt.Errorf("invalid week count in %q", s)
		}
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}

	return time.ParseDuration(s)
}

// cronField holds the parsed set of allowed values for one cron field.
// A nil set means wildcard (any value).
type cronField struct {
	values map[int]struct{}
}

// matches returns true when v is allowed by this field.
func (f cronField) matches(v int) bool {
	if f.values == nil {
		return true // wildcard
	}
	_, ok := f.values[v]
	return ok
}

// parseCronField converts a single cron field token (e.g., "5", "*/2",
// "1-5", "1,3,5") into a cronField. lo and hi define the valid range.
func parseCronField(token string, lo, hi int) (cronField, error) {
	token = strings.TrimSpace(token)
	if token == "*" {
		return cronField{}, nil // wildcard
	}

	vals := make(map[int]struct{})

	// Handle step expressions: "*/n" or "lo-hi/n"
	if strings.Contains(token, "/") {
		parts := strings.SplitN(token, "/", 2)
		step, err := strconv.Atoi(parts[1])
		if err != nil || step <= 0 {
			return cronField{}, fmt.Errorf("invalid step in %q", token)
		}
		rangeStart, rangeEnd := lo, hi
		if parts[0] != "*" {
			rangeParts := strings.SplitN(parts[0], "-", 2)
			rangeStart, err = strconv.Atoi(rangeParts[0])
			if err != nil {
				return cronField{}, fmt.Errorf("invalid range start in %q", token)
			}
			if len(rangeParts) == 2 {
				rangeEnd, err = strconv.Atoi(rangeParts[1])
				if err != nil {
					return cronField{}, fmt.Errorf("invalid range end in %q", token)
				}
			} else {
				rangeEnd = hi
			}
		}
		for v := rangeStart; v <= rangeEnd; v += step {
			vals[v] = struct{}{}
		}
		return cronField{values: vals}, nil
	}

	// Handle comma-separated lists and ranges.
	for _, part := range strings.Split(token, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			a, err1 := strconv.Atoi(bounds[0])
			b, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil {
				return cronField{}, fmt.Errorf("invalid range %q", part)
			}
			if a > b || a < lo || b > hi {
				return cronField{}, fmt.Errorf("range %q out of bounds [%d,%d]", part, lo, hi)
			}
			for v := a; v <= b; v++ {
				vals[v] = struct{}{}
			}
		} else {
			v, err := strconv.Atoi(part)
			if err != nil {
				return cronField{}, fmt.Errorf("invalid value %q: %w", part, err)
			}
			if v < lo || v > hi {
				return cronField{}, fmt.Errorf("value %d out of bounds [%d,%d]", v, lo, hi)
			}
			vals[v] = struct{}{}
		}
	}
	return cronField{values: vals}, nil
}

// parsedCron holds all five parsed cron fields.
type parsedCron struct {
	minute  cronField // 0-59
	hour    cronField // 0-23
	dom     cronField // 1-31 (day of month)
	month   cronField // 1-12
	dow     cronField // 0-6 (Sunday=0)
}

// parseCron tokenises a 5-field cron expression and returns a parsedCron.
func parseCronExpr(expr string) (parsedCron, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return parsedCron{}, fmt.Errorf("cron: expected 5 fields, got %d in %q", len(fields), expr)
	}

	var (
		pc  parsedCron
		err error
	)
	pc.minute, err = parseCronField(fields[0], 0, 59)
	if err != nil {
		return parsedCron{}, fmt.Errorf("cron minute: %w", err)
	}
	pc.hour, err = parseCronField(fields[1], 0, 23)
	if err != nil {
		return parsedCron{}, fmt.Errorf("cron hour: %w", err)
	}
	pc.dom, err = parseCronField(fields[2], 1, 31)
	if err != nil {
		return parsedCron{}, fmt.Errorf("cron dom: %w", err)
	}
	pc.month, err = parseCronField(fields[3], 1, 12)
	if err != nil {
		return parsedCron{}, fmt.Errorf("cron month: %w", err)
	}
	pc.dow, err = parseCronField(fields[4], 0, 6)
	if err != nil {
		return parsedCron{}, fmt.Errorf("cron dow: %w", err)
	}
	return pc, nil
}

// parseCron computes the next firing time after from for the given cron
// expression by iterating minute-by-minute up to a maximum search window of
// four years (to handle leap-year edge cases and wide month/dom combinations).
func parseCron(expr string, from time.Time) (time.Time, error) {
	pc, err := parseCronExpr(expr)
	if err != nil {
		return time.Time{}, err
	}

	// Start searching from the next whole minute after from.
	candidate := from.UTC().Truncate(time.Minute).Add(time.Minute)

	// Safety limit: search at most ~2 years of minutes to avoid infinite loops
	// for unsatisfiable expressions (e.g., "* * 31 2 *").
	limit := candidate.Add(2 * 365 * 24 * time.Hour)

	for candidate.Before(limit) {
		m := int(candidate.Month())
		if !pc.month.matches(m) {
			// Advance to the first day of the next candidate month.
			candidate = firstMinuteOfMonth(candidate.Year(), int(candidate.Month())+1)
			continue
		}

		d := candidate.Day()
		dow := int(candidate.Weekday()) // 0=Sunday
		if !pc.dom.matches(d) || !pc.dow.matches(dow) {
			// Advance to the start of the next day.
			candidate = firstMinuteOfDay(candidate.Year(), int(candidate.Month()), d+1)
			continue
		}

		h := candidate.Hour()
		if !pc.hour.matches(h) {
			// Advance to the next hour.
			candidate = time.Date(candidate.Year(), candidate.Month(), candidate.Day(),
				h+1, 0, 0, 0, time.UTC)
			continue
		}

		if pc.minute.matches(candidate.Minute()) {
			return candidate, nil
		}

		// Try the next minute.
		candidate = candidate.Add(time.Minute)
	}

	return time.Time{}, fmt.Errorf("cron: no valid time found within search window for %q", expr)
}

// firstMinuteOfMonth returns midnight on the first day of year/month.
func firstMinuteOfMonth(year, month int) time.Time {
	// time.Date normalises out-of-range month values (e.g., month=13 → Jan next year).
	return time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
}

// firstMinuteOfDay returns midnight on year/month/day.
func firstMinuteOfDay(year, month, day int) time.Time {
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}
