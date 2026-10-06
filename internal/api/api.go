// Package api exposes the relay's JSON API and serves the web UI.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/A-REDA-EL/powerfleet-relay/internal/relay"
	"github.com/A-REDA-EL/powerfleet-relay/internal/secret"
	"github.com/A-REDA-EL/powerfleet-relay/internal/store"
	"github.com/A-REDA-EL/powerfleet-relay/internal/traccar"
)

const (
	dbSettingKey  = "traccar_db"
	sessionCookie = "relay_session"
	sessionTTL    = 12 * time.Hour
)

type Server struct {
	Store         *store.Store
	Manager       *relay.Manager
	Box           *secret.Box
	AdminPassword string
	UI            fs.FS
	Log           *slog.Logger
}

// storedDB is the persisted connection; the password is encrypted.
type storedDB struct {
	traccar.Config
	PasswordEnc string `json:"passwordEnc"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/session", s.session)

	authed := http.NewServeMux()
	authed.HandleFunc("GET /api/settings/database", s.getDatabase)
	authed.HandleFunc("PUT /api/settings/database", s.putDatabase)
	authed.HandleFunc("POST /api/settings/database/test", s.testDatabase)
	authed.HandleFunc("GET /api/traccar/info", s.traccarInfo)
	authed.HandleFunc("GET /api/devices", s.devices)
	authed.HandleFunc("GET /api/devices/ids", s.deviceIDs)
	authed.HandleFunc("POST /api/devices/lookup", s.deviceLookup)
	authed.HandleFunc("POST /api/estimate", s.estimate)
	authed.HandleFunc("POST /api/target/test", s.testTarget)
	authed.HandleFunc("GET /api/jobs", s.listJobs)
	authed.HandleFunc("POST /api/jobs", s.createJob)
	authed.HandleFunc("GET /api/jobs/{id}", s.getJob)
	authed.HandleFunc("PATCH /api/jobs/{id}", s.updateJobTarget)
	authed.HandleFunc("POST /api/jobs/{id}/{action}", s.jobAction)
	authed.HandleFunc("DELETE /api/jobs/{id}", s.deleteJob)
	mux.Handle("/api/", s.requireAuth(authed))

	mux.Handle("/", s.spa())
	return securityHeaders(mux)
}

// ---------- auth ----------

func (s *Server) authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.Box.ValidToken(c.Value)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r) {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct{ Password string }
	if !readJSON(w, r, &body) {
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.AdminPassword)) != 1 {
		time.Sleep(700 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.Box.Token(time.Now().Add(sessionTTL)), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge: int(sessionTTL.Seconds()),
	})
	writeJSON(w, 200, map[string]bool{"authenticated": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, 200, map[string]bool{"authenticated": false})
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"authenticated": s.authenticated(r)})
}

// ---------- database settings ----------

func (s *Server) loadDB(ctx context.Context) (traccar.Config, bool, error) {
	var stored storedDB
	ok, err := s.Store.GetSetting(ctx, dbSettingKey, &stored)
	if err != nil || !ok {
		return traccar.Config{}, false, err
	}
	cfg := stored.Config
	cfg.Password, err = s.Box.Decrypt(stored.PasswordEnc)
	return cfg, true, err
}

// SaveDB stores a connection; an empty password keeps the stored one.
func (s *Server) SaveDB(ctx context.Context, cfg traccar.Config) error {
	if cfg.Password == "" {
		if old, ok, _ := s.loadDB(ctx); ok {
			cfg.Password = old.Password
		}
	}
	enc, err := s.Box.Encrypt(cfg.Password)
	if err != nil {
		return err
	}
	stored := storedDB{Config: cfg, PasswordEnc: enc}
	stored.Password = ""
	return s.Store.PutSetting(ctx, dbSettingKey, stored)
}

// OpenSource opens the configured Traccar database (used by the job engine).
func (s *Server) OpenSource(ctx context.Context) (relay.Source, error) {
	cfg, ok, err := s.loadDB(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("Traccar database is not configured")
	}
	return traccar.Open(ctx, cfg)
}

func (s *Server) getDatabase(w http.ResponseWriter, r *http.Request) {
	cfg, ok, err := s.loadDB(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	hasPassword := cfg.Password != ""
	cfg.Password = ""
	writeJSON(w, 200, map[string]any{"configured": ok, "config": cfg, "hasPassword": hasPassword})
}

func (s *Server) dbFromRequest(w http.ResponseWriter, r *http.Request) (traccar.Config, bool) {
	var cfg traccar.Config
	if !readJSON(w, r, &cfg) {
		return cfg, false
	}
	if cfg.Password == "" {
		if old, ok, _ := s.loadDB(r.Context()); ok {
			cfg.Password = old.Password
		}
	}
	return cfg, true
}

func (s *Server) putDatabase(w http.ResponseWriter, r *http.Request) {
	cfg, ok := s.dbFromRequest(w, r)
	if !ok {
		return
	}
	if _, err := cfg.DSN(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.SaveDB(r.Context(), cfg); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"saved": true})
}

func (s *Server) testDatabase(w http.ResponseWriter, r *http.Request) {
	cfg, ok := s.dbFromRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	src, err := traccar.Open(ctx, cfg)
	if err != nil {
		writeError(w, 400, friendlyDBError(err))
		return
	}
	defer src.Close()
	info, err := src.Info(ctx)
	if err != nil {
		writeError(w, 400, friendlyDBError(err))
		return
	}
	writeJSON(w, 200, info)
}

func friendlyDBError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return msg + " — MySQL is not listening on that address. If MySQL runs on the host, use the socket mode or make it listen on the Docker bridge."
	case strings.Contains(msg, "no such file or directory") && strings.Contains(msg, "unix"):
		return msg + " — the socket is not mounted into the container (see docker-compose.yml)."
	case strings.Contains(msg, "Access denied"):
		return msg + " — check user/password and that the user is allowed from this host (containers connect from 172.x.x.x, not localhost, over TCP)."
	}
	return msg
}

func (s *Server) withSource(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, src relay.Source) (any, error)) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	src, err := s.OpenSource(ctx)
	if err != nil {
		writeError(w, 400, friendlyDBError(err))
		return
	}
	defer src.Close()
	out, err := fn(ctx, src)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) traccarInfo(w http.ResponseWriter, r *http.Request) {
	s.withSource(w, r, func(ctx context.Context, src relay.Source) (any, error) {
		return src.(*traccar.Source).Info(ctx)
	})
}

// maxSelectAll caps "select all matching" so one request cannot select an unbounded fleet.
const maxSelectAll = 100000

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := min(max(atoiDefault(q.Get("limit"), 50), 1), 200)
	offset := max(atoiDefault(q.Get("offset"), 0), 0)
	s.withSource(w, r, func(ctx context.Context, src relay.Source) (any, error) {
		return src.(*traccar.Source).SearchDevices(ctx, q.Get("q"), limit, offset)
	})
}

func (s *Server) deviceIDs(w http.ResponseWriter, r *http.Request) {
	s.withSource(w, r, func(ctx context.Context, src relay.Source) (any, error) {
		ids, err := src.(*traccar.Source).DeviceIDs(ctx, r.URL.Query().Get("q"), maxSelectAll)
		return map[string]any{"ids": ids, "capped": len(ids) >= maxSelectAll}, err
	})
}

// deviceLookup resolves ids to devices (the review step shows the selected ones by name).
func (s *Server) deviceLookup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.withSource(w, r, func(ctx context.Context, src relay.Source) (any, error) {
		list, err := src.DevicesByIDs(ctx, req.IDs)
		if list == nil {
			list = []traccar.Device{}
		}
		return list, err
	})
}

func atoiDefault(v string, def int) int {
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

type estimateRequest struct {
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
	DeviceIDs []int64   `json:"deviceIds"`
}

func (s *Server) estimate(w http.ResponseWriter, r *http.Request) {
	var req estimateRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := validRange(req.From, req.To); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	s.withSource(w, r, func(ctx context.Context, src relay.Source) (any, error) {
		counts := make(map[int64]int64, len(req.DeviceIDs))
		var mu sync.Mutex
		var firstErr error
		sem := make(chan struct{}, 4) // gentle on the production database
		var wg sync.WaitGroup
		for _, id := range req.DeviceIDs {
			wg.Add(1)
			sem <- struct{}{}
			go func(id int64) {
				defer wg.Done()
				defer func() { <-sem }()
				n, err := src.Count(ctx, id, req.From, req.To)
				mu.Lock()
				defer mu.Unlock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				counts[id] = n
			}(id)
		}
		wg.Wait()
		var total int64
		out := make([]map[string]int64, 0, len(counts))
		for id, n := range counts {
			total += n
			out = append(out, map[string]int64{"deviceId": id, "count": n})
		}
		return map[string]any{"total": total, "devices": out}, firstErr
	})
}

func validRange(from, to time.Time) error {
	if from.IsZero() || to.IsZero() {
		return errors.New("from and to are required")
	}
	if !from.Before(to) {
		return errors.New("from must be before to")
	}
	return nil
}

// testTarget checks the target without storing any position. Forward mode only opens a
// connection (OPTIONS); import mode posts an empty batch for a vehicle that cannot exist, which
// proves the URL, that the import is enabled and that the API key is accepted.
func (s *Server) testTarget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL     string            `json:"url"`
		Mode    string            `json:"mode"`
		Headers map[string]string `json:"headers"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := validURL(body.URL); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	start := time.Now()
	var req *http.Request
	if body.Mode == store.ModeImport {
		probe := `{"device":{"uniqueId":"RELAY-CHECK"},"positions":[]}`
		req, _ = http.NewRequestWithContext(r.Context(), http.MethodPost, body.URL, strings.NewReader(probe))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range body.Headers {
			req.Header.Set(k, v)
		}
	} else {
		req, _ = http.NewRequestWithContext(r.Context(), http.MethodOptions, body.URL, nil)
	}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, 200, map[string]any{"reachable": false, "error": err.Error()})
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	out := map[string]any{"reachable": true, "status": resp.StatusCode, "latencyMs": time.Since(start).Milliseconds()}
	if body.Mode == store.ModeImport {
		var parsed struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &parsed)
		switch {
		case resp.StatusCode == http.StatusNotFound && parsed.Message == "Unknown object":
			out["ready"], out["message"] = true, "Import endpoint ready, API key accepted"
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			out["ready"], out["message"] = false, "API key refused"
		case resp.StatusCode == http.StatusNotFound:
			out["ready"], out["message"] = false, "Import is disabled on the Server or the URL is wrong ("+parsed.Message+")"
		default:
			out["ready"], out["message"] = false, fmt.Sprintf("Unexpected answer HTTP %d %s", resp.StatusCode, parsed.Message)
		}
	}
	writeJSON(w, 200, out)
}

func validURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("target must be an http(s) URL")
	}
	return nil
}

// ---------- jobs ----------

type createJobRequest struct {
	Name           string            `json:"name"`
	From           time.Time         `json:"from"`
	To             time.Time         `json:"to"`
	DeviceIDs      []int64           `json:"deviceIds"`
	TargetURL      string            `json:"targetUrl"`
	Mode           string            `json:"mode"` // "forward" (default) or "import"
	Headers        map[string]string `json:"headers"`
	Concurrency    int               `json:"concurrency"`
	RateLimit      int               `json:"rateLimit"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if !readJSON(w, r, &req) {
		return
	}
	if err := validRange(req.From, req.To); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := validURL(req.TargetURL); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if len(req.DeviceIDs) == 0 {
		writeError(w, 400, "select at least one device")
		return
	}
	switch req.Mode {
	case "", store.ModeForward:
		req.Mode = store.ModeForward
		req.Concurrency = clamp(req.Concurrency, 1, 64, 4)
	case store.ModeImport:
		if !hasHeader(req.Headers, "X-Api-Key") {
			writeError(w, 400, "PowerFleet import needs the Server's API key (X-Api-Key)")
			return
		}
		// the Server imports a few batches at a time (GPS_IMPORT_CONCURRENCY, default 2)
		req.Concurrency = clamp(req.Concurrency, 1, 32, 2)
	default:
		writeError(w, 400, "mode must be forward or import")
		return
	}
	req.RateLimit = clamp(req.RateLimit, 0, 100000, 100)
	req.TimeoutSeconds = clamp(req.TimeoutSeconds, 1, 300, 20)
	if strings.TrimSpace(req.Name) == "" {
		req.Name = fmt.Sprintf("Relay %s → %s", req.From.UTC().Format("2006-01-02"), req.To.UTC().Format("2006-01-02"))
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	src, err := s.OpenSource(ctx)
	if err != nil {
		writeError(w, 400, friendlyDBError(err))
		return
	}
	all, err := src.DevicesByIDs(ctx, req.DeviceIDs)
	src.Close()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	byID := map[int64]traccar.Device{}
	for _, d := range all {
		byID[d.ID] = d
	}
	devices := make([]store.JobDevice, 0, len(req.DeviceIDs))
	for _, id := range req.DeviceIDs {
		d, ok := byID[id]
		if !ok {
			writeError(w, 400, fmt.Sprintf("device %d does not exist in Traccar", id))
			return
		}
		devices = append(devices, store.JobDevice{DeviceID: id, Name: d.Name, UniqueID: d.UniqueID, Total: -1})
	}
	id, err := s.Store.CreateJob(r.Context(), store.Job{
		Name: req.Name, TargetURL: req.TargetURL, Mode: req.Mode, Headers: req.Headers, From: req.From, To: req.To,
		Concurrency: req.Concurrency, RateLimit: req.RateLimit, TimeoutSeconds: req.TimeoutSeconds,
	}, devices)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	_ = s.Store.AddEvent(r.Context(), id, "info", fmt.Sprintf("Created: %d devices, %s → %s, %s to %s",
		len(devices), req.From.UTC().Format(time.RFC3339), req.To.UTC().Format(time.RFC3339), modeLabel(req.Mode), req.TargetURL))
	if err := s.Manager.Start(id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	job, _ := s.Store.Job(r.Context(), id)
	writeJSON(w, 201, job)
}

func hasHeader(h map[string]string, name string) bool {
	for k, v := range h {
		if strings.EqualFold(k, name) && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

func modeLabel(mode string) string {
	if mode == store.ModeImport {
		return "PowerFleet import (history only)"
	}
	return "Traccar forward"
}

func clamp(v, lo, hi, def int) int {
	if v == 0 && lo > 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

type jobView struct {
	store.Job
	Active bool `json:"active"`
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.Jobs(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobView{j, s.Manager.Running(j.ID)})
	}
	writeJSON(w, 200, out)
}

func jobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid job id")
		return 0, false
	}
	return id, true
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	job, err := s.Store.Job(r.Context(), id)
	if err != nil {
		writeError(w, 404, "job not found")
		return
	}
	devices, _ := s.Store.JobDevices(r.Context(), id)
	responses, _ := s.Store.Responses(r.Context(), id)
	events, _ := s.Store.Events(r.Context(), id, 100)
	sort.SliceStable(devices, func(a, b int) bool { return !devices[a].Done && devices[b].Done })
	writeJSON(w, 200, map[string]any{
		"job": jobView{job, s.Manager.Running(id)}, "devices": nonNil(devices), "responses": nonNil(responses), "events": nonNil(events),
	})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// updateJobTarget lets a job that is not running be fixed (wrong API key or URL) and resumed.
func (s *Server) updateJobTarget(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	var req struct {
		TargetURL      string            `json:"targetUrl"`
		Headers        map[string]string `json:"headers"`
		Concurrency    int               `json:"concurrency"`
		RateLimit      int               `json:"rateLimit"`
		TimeoutSeconds int               `json:"timeoutSeconds"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	job, err := s.Store.Job(r.Context(), id)
	if err != nil {
		writeError(w, 404, "job not found")
		return
	}
	if s.Manager.Running(id) {
		writeError(w, 409, "pause the job before editing it")
		return
	}
	switch job.Status {
	case store.StatusCompleted, store.StatusCancelled:
		writeError(w, 409, "a finished job cannot be edited; use Run again")
		return
	}
	if err := validURL(req.TargetURL); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if job.Mode == store.ModeImport {
		if !hasHeader(req.Headers, "X-Api-Key") {
			writeError(w, 400, "PowerFleet import needs the Server's API key (X-Api-Key)")
			return
		}
		req.Concurrency = clamp(req.Concurrency, 1, 32, 2)
	} else {
		req.Concurrency = clamp(req.Concurrency, 1, 64, 4)
	}
	req.RateLimit = clamp(req.RateLimit, 0, 100000, 100)
	req.TimeoutSeconds = clamp(req.TimeoutSeconds, 1, 300, 20)
	if err := s.Store.UpdateJobTarget(r.Context(), id, req.TargetURL, req.Headers, req.Concurrency, req.RateLimit, req.TimeoutSeconds); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	_ = s.Store.AddEvent(r.Context(), id, "info", "Target settings edited")
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) jobAction(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	job, err := s.Store.Job(r.Context(), id)
	if err != nil {
		writeError(w, 404, "job not found")
		return
	}
	switch r.PathValue("action") {
	case "pause":
		if !s.Manager.Stop(id, store.StatusPaused) {
			writeError(w, 409, "job is not running")
			return
		}
	case "resume":
		if job.Status != store.StatusPaused && job.Status != store.StatusFailed && job.Status != store.StatusCancelled {
			writeError(w, 409, "only paused, failed or cancelled jobs can be resumed")
			return
		}
		_ = s.Store.AddEvent(r.Context(), id, "info", "Resumed")
		if err := s.Manager.Start(id); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	case "retry-rejected":
		if s.Manager.Running(id) || job.Status == store.StatusPending || job.Status == store.StatusCounting || job.Status == store.StatusRunning {
			writeError(w, 409, "pause or wait for the job to finish before retrying its rejected positions")
			return
		}
		n, err := s.Store.RewindRejected(r.Context(), id)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if n == 0 {
			writeError(w, 409, "this job has no rejected positions")
			return
		}
		_ = s.Store.AddEvent(r.Context(), id, "info", fmt.Sprintf("Retrying the rejected positions of %d device(s)", n))
		if err := s.Manager.Start(id); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	case "cancel":
		if !s.Manager.Stop(id, store.StatusCancelled) {
			if job.Status == store.StatusCompleted || job.Status == store.StatusCancelled {
				writeError(w, 409, "job already finished")
				return
			}
			_ = s.Store.SetStatus(r.Context(), id, store.StatusCancelled)
			_ = s.Store.AddEvent(r.Context(), id, "info", "Cancelled")
		}
	default:
		writeError(w, 404, "unknown action")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	if s.Manager.Running(id) {
		writeError(w, 409, "stop the job before deleting it")
		return
	}
	if err := s.Store.DeleteJob(r.Context(), id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

// ---------- helpers ----------

func (s *Server) spa() http.Handler {
	files := http.FileServerFS(s.UI)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if f, err := s.UI.Open(path); err == nil {
				f.Close()
				if strings.HasPrefix(path, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		// client-side routes fall back to index.html
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		files.ServeHTTP(w, r2)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
