// Package relay runs replay jobs: it reads each selected device's positions from
// Traccar in (fixtime, id) order and posts them to a target URL, retrying until the
// target accepts them, with a durable per-position checkpoint.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/A-REDA-EL/powerfleet-relay/internal/store"
	"github.com/A-REDA-EL/powerfleet-relay/internal/traccar"
)

// Source is the part of the Traccar database the engine needs.
type Source interface {
	Devices(ctx context.Context) ([]traccar.Device, error)
	Count(ctx context.Context, deviceID int64, from, to time.Time) (int64, error)
	Page(ctx context.Context, deviceID int64, from, to time.Time, after traccar.Cursor, limit int) ([]traccar.Position, error)
	Close() error
}

type Manager struct {
	store      *store.Store
	openSource func(ctx context.Context) (Source, error)
	log        *slog.Logger

	// tunables, overridden in tests
	PageSize      int
	FlushInterval time.Duration
	// import mode: positions per batch and largest request body (the Server accepts 1 MiB)
	ImportBatchSize int
	ImportMaxBytes  int
	MinBackoff      time.Duration
	MaxBackoff      time.Duration

	mu   sync.Mutex
	runs map[int64]*run
	wg   sync.WaitGroup
}

func NewManager(st *store.Store, openSource func(ctx context.Context) (Source, error), log *slog.Logger) *Manager {
	return &Manager{
		store: st, openSource: openSource, log: log,
		PageSize: 500, FlushInterval: 500 * time.Millisecond,
		ImportBatchSize: 200, ImportMaxBytes: 512 << 10,
		MinBackoff: 500 * time.Millisecond, MaxBackoff: 30 * time.Second,
		runs: map[int64]*run{},
	}
}

// Start runs (or resumes) a job in the background. It is a no-op if the job is already running.
func (m *Manager) Start(jobID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[jobID]; ok {
		return nil
	}
	job, err := m.store.Job(context.Background(), jobID)
	if err != nil {
		return err
	}
	switch job.Status {
	case store.StatusCompleted, store.StatusCancelled:
		return fmt.Errorf("job %d is %s", jobID, job.Status)
	}
	r := &run{m: m, jobID: jobID, stop: make(chan struct{})}
	m.runs[jobID] = r
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		r.execute()
		m.mu.Lock()
		delete(m.runs, jobID)
		m.mu.Unlock()
	}()
	return nil
}

// Stop asks a running job to stop after its in-flight requests finish, ending in the given status.
func (m *Manager) Stop(jobID int64, final string) bool {
	m.mu.Lock()
	r, ok := m.runs[jobID]
	m.mu.Unlock()
	if !ok {
		return false
	}
	r.requestStop(final)
	return true
}

func (m *Manager) Running(jobID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.runs[jobID]
	return ok
}

// Shutdown stops every job gracefully; they stay "running" in the store and resume at next boot.
func (m *Manager) Shutdown(timeout time.Duration) {
	m.mu.Lock()
	for _, r := range m.runs {
		r.requestStop("")
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		m.log.Warn("shutdown timeout: some jobs did not stop in time")
	}
}

// ResumeInterrupted restarts jobs that were running when the process last stopped.
func (m *Manager) ResumeInterrupted(ctx context.Context, cleanShutdown bool) {
	jobs, err := m.store.JobsWithStatus(ctx, store.StatusRunning, store.StatusCounting)
	if err != nil {
		m.log.Error("cannot list interrupted jobs", "err", err)
		return
	}
	for _, j := range jobs {
		msg, level := "Resumed after relay restart", "info"
		if !cleanShutdown {
			msg, level = "Resumed after an unclean relay stop", "warn"
		}
		_ = m.store.AddEvent(ctx, j.ID, level, msg)
		if err := m.Start(j.ID); err != nil {
			m.log.Error("cannot resume job", "job", j.ID, "err", err)
		}
	}
}

type run struct {
	m        *Manager
	jobID    int64
	stop     chan struct{}
	stopOnce sync.Once
	final    atomic.Value // status to end in when stopped ("" = leave as running)

	mu           sync.Mutex
	progress     store.Progress
	waiting      int // workers currently retrying
	waitingStart time.Time
}

func (r *run) requestStop(final string) {
	r.final.Store(final)
	r.stopOnce.Do(func() { close(r.stop) })
}

func (r *run) stopped() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

// sleep waits for d or until the job is stopped; it reports whether the full delay elapsed.
func (r *run) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.stop:
		return false
	}
}

func (r *run) event(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	r.m.log.Info("job event", "job", r.jobID, "level", level, "msg", msg)
	_ = r.m.store.AddEvent(context.Background(), r.jobID, level, msg)
}

func (r *run) execute() {
	ctx := context.Background()
	st := r.m.store
	r.progress = store.Progress{JobID: r.jobID, Responses: map[store.ResponseKey]int64{}}
	job, err := st.Job(ctx, r.jobID)
	if err != nil {
		r.m.log.Error("load job", "job", r.jobID, "err", err)
		return
	}
	src, err := r.openSourceWithRetry()
	if err != nil {
		r.finish(err)
		return
	}
	defer src.Close()

	devices, err := st.JobDevices(ctx, r.jobID)
	if err != nil {
		r.finish(err)
		return
	}
	if err := r.count(src, job, devices); err != nil {
		r.finish(err)
		return
	}
	if r.stopped() {
		r.finish(nil)
		return
	}
	devices, _ = st.JobDevices(ctx, r.jobID)
	_ = st.SetStatus(ctx, r.jobID, store.StatusRunning)

	// Positions that were not acknowledged when the relay stopped are sent again. Each earlier
	// attempt that may have reached the target (plus the request in flight at an unclean stop)
	// can be an extra copy on the target.
	var extra int64
	for _, d := range devices {
		n := d.InflightAttempts
		if d.InflightID != 0 {
			n++
		}
		if n > 0 {
			extra += n
			_ = st.ClearInflight(ctx, r.jobID, d.DeviceID)
		}
	}
	if extra > 0 {
		_ = st.Flush(ctx, store.Progress{JobID: r.jobID, PossibleDuplicates: extra})
		r.event("warn", "%d earlier attempt(s) may have been stored before the relay stopped; the positions are sent again and counted as possible duplicates", extra)
	}

	traccarDevices := map[int64]traccar.Device{}
	if list, err := src.Devices(ctx); err == nil {
		for _, d := range list {
			traccarDevices[d.ID] = d
		}
	}

	flushDone := make(chan struct{})
	flushStop := make(chan struct{})
	go func() {
		defer close(flushDone)
		t := time.NewTicker(r.m.FlushInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.flush()
			case <-flushStop:
				return
			}
		}
	}()

	var limiter *rate.Limiter
	if job.RateLimit > 0 {
		burst := max(1, job.RateLimit/10)
		if job.Mode == store.ModeImport {
			burst = max(burst, r.m.ImportBatchSize) // a whole batch is taken at once
		}
		limiter = rate.NewLimiter(rate.Limit(job.RateLimit), burst)
	}
	client := &http.Client{
		Timeout: time.Duration(job.TimeoutSeconds) * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        job.Concurrency * 2,
			MaxIdleConnsPerHost: job.Concurrency * 2,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	defer client.CloseIdleConnections()

	queue := make(chan store.JobDevice)
	var workers sync.WaitGroup
	var failure atomic.Value
	for i := 0; i < max(1, job.Concurrency); i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for d := range queue {
				dev, ok := traccarDevices[d.DeviceID]
				if !ok { // device deleted from Traccar since: send what we know
					dev = traccar.Device{ID: d.DeviceID, Name: d.Name, UniqueID: d.UniqueID, Attributes: json.RawMessage("{}")}
				}
				if err := r.device(src, client, limiter, job, d, dev); err != nil {
					failure.CompareAndSwap(nil, err)
					r.requestStop(store.StatusFailed)
				}
			}
		}()
	}
feed:
	for _, d := range devices {
		if d.Done {
			continue
		}
		select {
		case queue <- d:
		case <-r.stop:
			break feed
		}
	}
	close(queue)
	workers.Wait()
	close(flushStop)
	<-flushDone
	r.flush()

	if err, _ := failure.Load().(error); err != nil {
		r.finish(err)
		return
	}
	r.finish(nil)
}

func (r *run) openSourceWithRetry() (Source, error) {
	backoff := r.m.MinBackoff
	waiting := false
	defer func() {
		if waiting {
			r.endWait()
		}
	}()
	for {
		src, err := r.m.openSource(context.Background())
		if err == nil {
			return src, nil
		}
		if !waiting {
			waiting = true
			r.beginWait("Traccar database: " + err.Error())
		}
		if !r.sleep(backoff) {
			return nil, errors.New("stopped while waiting for the Traccar database")
		}
		backoff = min(backoff*2, r.m.MaxBackoff)
	}
}

// count fills in per-device totals the first time a job runs.
func (r *run) count(src Source, job store.Job, devices []store.JobDevice) error {
	ctx := context.Background()
	pending := 0
	for _, d := range devices {
		if d.Total < 0 {
			pending++
		}
	}
	if pending == 0 {
		return nil
	}
	_ = r.m.store.SetStatus(ctx, r.jobID, store.StatusCounting)
	r.event("info", "Counting positions for %d devices", pending)
	for _, d := range devices {
		if d.Total >= 0 {
			continue
		}
		if r.stopped() {
			return nil
		}
		n, err := src.Count(ctx, d.DeviceID, job.From, job.To)
		if err != nil {
			return fmt.Errorf("count positions of %s: %w", d.UniqueID, err)
		}
		if err := r.m.store.SetDeviceTotal(ctx, r.jobID, d.DeviceID, n); err != nil {
			return err
		}
	}
	if err := r.m.store.SetTotal(ctx, r.jobID); err != nil {
		return err
	}
	j, _ := r.m.store.Job(ctx, r.jobID)
	r.event("info", "%d positions to send", j.Total)
	return nil
}

// device sends one device's positions in order, starting after its checkpoint.
// Each position is marked in flight before it is sent and checkpointed as soon as the target answered,
// so a crash resends at most that one position per device.
func (r *run) device(src Source, client *http.Client, limiter *rate.Limiter, job store.Job, d store.JobDevice, dev traccar.Device) error {
	if job.Mode == store.ModeImport {
		return r.deviceImport(src, client, limiter, job, d, dev)
	}
	ctx := context.Background()
	st := r.m.store
	cursor := traccar.Cursor{ID: d.CursorID}
	if d.CursorAt != nil {
		cursor.FixTime = *d.CursorAt
	}
	backoff := r.m.MinBackoff
	readWaiting := false
	defer func() {
		if readWaiting {
			r.endWait()
		}
	}()
	for !r.stopped() {
		page, err := src.Page(ctx, d.DeviceID, job.From, job.To, cursor, r.m.PageSize)
		if err != nil {
			if !readWaiting {
				readWaiting = true
				r.beginWait("Traccar database: " + err.Error())
			}
			if !r.sleep(backoff) {
				return nil
			}
			backoff = min(backoff*2, r.m.MaxBackoff)
			continue
		}
		if readWaiting {
			readWaiting = false
			r.endWait()
		}
		backoff = r.m.MinBackoff
		if len(page) == 0 {
			return st.MarkDeviceDone(ctx, r.jobID, d.DeviceID)
		}
		for _, pos := range page {
			if r.stopped() {
				return nil
			}
			if limiter != nil {
				if err := waitLimiter(limiter, r.stop); err != nil {
					return nil
				}
			}
			body, err := traccar.Payload(dev, pos)
			if err != nil {
				return fmt.Errorf("encode position %d: %w", pos.ID, err)
			}
			if err := st.MarkInflight(ctx, r.jobID, d.DeviceID, pos.ID); err != nil {
				return err
			}
			res, ok := r.deliver(client, job, body, func() { _ = st.AddAmbiguousAttempt(ctx, r.jobID, d.DeviceID) })
			if !ok {
				// stopped while retrying: nothing is in flight; ambiguous attempts stay counted for the resume
				_ = st.StopInflight(ctx, r.jobID, d.DeviceID)
				return nil
			}
			fix := time.Time{}
			if pos.FixTime != nil {
				fix = pos.FixTime.Time
			}
			if err := st.Advance(ctx, r.jobID, d.DeviceID, fix, pos.ID, res.rejected); err != nil {
				return err
			}
			cursor = traccar.Cursor{FixTime: fix, ID: pos.ID}
			r.mu.Lock()
			r.progress.PossibleDuplicates += res.ambiguousAttempts
			r.progress.Responses[res.key]++
			r.mu.Unlock()
		}
	}
	return nil
}

func waitLimiter(l *rate.Limiter, stop <-chan struct{}) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return l.Wait(ctx)
}

type result struct {
	key      store.ResponseKey
	rejected bool
	// failed attempts the target may nevertheless have stored: each can be an extra copy
	ambiguousAttempts int64
}

// deliver posts one position until the target accepts or definitively rejects it.
// ok=false means the job was stopped first. onAmbiguous is called for every failed attempt
// that may still have been stored (timeout, dropped connection, HTTP 500/504).
func (r *run) deliver(client *http.Client, job store.Job, body []byte, onAmbiguous func()) (res result, ok bool) {
	var ambiguous int64
	backoff := r.m.MinBackoff
	waiting := false
	defer func() {
		if waiting {
			r.endWait()
		}
	}()
	for {
		status, msg, err := post(client, job, body)
		var reason string
		switch {
		case err != nil:
			reason = err.Error()
			if !isDialError(err) {
				ambiguous++
				onAmbiguous()
			}
		case status >= 200 && status < 300:
			return result{key: store.ResponseKey{Outcome: "sent", Status: status, Message: msg}, ambiguousAttempts: ambiguous}, true
		case status == 408 || status == 425 || status == 429 || status >= 500:
			reason = fmt.Sprintf("HTTP %d %s", status, msg)
			if status == 500 || status == 504 {
				ambiguous++
				onAmbiguous()
			}
		default:
			return result{key: store.ResponseKey{Outcome: "rejected", Status: status, Message: msg}, rejected: true, ambiguousAttempts: ambiguous}, true
		}
		r.mu.Lock()
		r.progress.Retries++
		r.mu.Unlock()
		if !waiting {
			waiting = true
			r.beginWait("Target: " + reason)
		}
		if !r.sleep(jitter(backoff)) {
			return result{}, false
		}
		backoff = min(backoff*2, r.m.MaxBackoff)
	}
}

func post(client *http.Client, job store.Job, body []byte) (int, string, error) {
	status, raw, err := postRaw(client, job, body)
	return status, summarize(raw), err
}

func postRaw(client *http.Client, job store.Job, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, job.TargetURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "powerfleet-relay")
	for k, v := range job.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// summarize extracts a short, groupable message from a response body.
func summarize(raw []byte) string {
	var parsed struct {
		Message any `json:"message"`
		Error   any `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil {
		for _, v := range []any{parsed.Message, parsed.Error} {
			if v != nil {
				return truncate(fmt.Sprint(v), 120)
			}
		}
	}
	return truncate(strings.TrimSpace(string(raw)), 120)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isDialError reports whether the request certainly never reached the target.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// beginWait/endWait bracket a worker's retry loop; the job shows "waiting" while any worker retries.
func (r *run) beginWait(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waiting++
	if r.waiting == 1 {
		r.waitingStart = time.Now()
		since := r.waitingStart
		r.progress.WaitingSince, r.progress.ClearWaiting, r.progress.LastError = &since, false, &reason
		go r.event("warn", "Waiting: %s", reason)
	}
}

func (r *run) endWait() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waiting--
	if r.waiting == 0 {
		empty := ""
		r.progress.WaitingSince, r.progress.ClearWaiting, r.progress.LastError = nil, true, &empty
		// wall-clock duration: the monotonic clock pauses while a laptop/VM sleeps
		go r.event("info", "Target reachable again after %s, continuing", time.Now().Round(0).Sub(r.waitingStart.Round(0)).Round(time.Second))
	}
}

func (r *run) flush() {
	r.mu.Lock()
	p := r.progress
	r.progress = store.Progress{JobID: r.jobID, Responses: map[store.ResponseKey]int64{}}
	r.mu.Unlock()
	if p.Retries == 0 && p.PossibleDuplicates == 0 && len(p.Responses) == 0 && p.WaitingSince == nil && !p.ClearWaiting && p.LastError == nil {
		return
	}
	if err := r.m.store.Flush(context.Background(), p); err != nil {
		r.m.log.Error("flush progress", "job", r.jobID, "err", err)
		r.mu.Lock()
		merge(&r.progress, p) // keep the deltas so they are written next time
		r.mu.Unlock()
	}
}

func merge(dst *store.Progress, src store.Progress) {
	dst.Retries += src.Retries
	dst.PossibleDuplicates += src.PossibleDuplicates
	for k, n := range src.Responses {
		dst.Responses[k] += n
	}
	if dst.WaitingSince == nil && !dst.ClearWaiting {
		dst.WaitingSince, dst.ClearWaiting = src.WaitingSince, src.ClearWaiting
	}
	if dst.LastError == nil {
		dst.LastError = src.LastError
	}
}

func (r *run) finish(err error) {
	ctx := context.Background()
	st := r.m.store
	if err != nil {
		r.event("error", "Failed: %s", err)
		msg := err.Error()
		_ = st.Flush(ctx, store.Progress{JobID: r.jobID, LastError: &msg})
		_ = st.SetStatus(ctx, r.jobID, store.StatusFailed)
		return
	}
	if r.stopped() {
		final, _ := r.final.Load().(string)
		switch final {
		case store.StatusPaused:
			_ = st.SetStatus(ctx, r.jobID, store.StatusPaused)
			r.event("info", "Paused")
		case store.StatusCancelled:
			_ = st.SetStatus(ctx, r.jobID, store.StatusCancelled)
			r.event("info", "Cancelled")
		default:
			r.event("info", "Stopped by relay shutdown; will resume on next start")
		}
		return
	}
	j, _ := st.Job(ctx, r.jobID)
	_ = st.SetStatus(ctx, r.jobID, store.StatusCompleted)
	r.event("info", "Completed: %d sent, %d rejected, %d retries, %d possible duplicates", j.Sent, j.Rejected, j.Retries, j.PossibleDuplicates)
}
