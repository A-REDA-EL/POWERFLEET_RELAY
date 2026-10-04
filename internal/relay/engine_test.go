package relay

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/A-REDA-EL/powerfleet-relay/internal/store"
	"github.com/A-REDA-EL/powerfleet-relay/internal/traccar"
)

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

type fakeSource struct {
	devices   []traccar.Device
	positions map[int64][]traccar.Position
}

func newFakeSource(devices, perDevice int) *fakeSource {
	s := &fakeSource{positions: map[int64][]traccar.Position{}}
	id := int64(0)
	for d := 1; d <= devices; d++ {
		s.devices = append(s.devices, traccar.Device{ID: int64(d), Name: "dev", UniqueID: "imei" + string(rune('A'+d)), Attributes: json.RawMessage("{}")})
		for i := 0; i < perDevice; i++ {
			id++
			// two positions share each fix second to exercise the (fixtime, id) keyset
			fix := traccar.Time{Time: t0.Add(time.Duration(i/2) * time.Second)}
			s.positions[int64(d)] = append(s.positions[int64(d)], traccar.Position{ID: id, DeviceID: int64(d), FixTime: &fix, Attributes: json.RawMessage("{}"), Network: json.RawMessage("null"), GeofenceIDs: json.RawMessage("null")})
		}
	}
	return s
}

func (s *fakeSource) Devices(context.Context) ([]traccar.Device, error) { return s.devices, nil }
func (s *fakeSource) Close() error                                      { return nil }
func (s *fakeSource) Count(_ context.Context, id int64, from, to time.Time) (int64, error) {
	return int64(len(s.positions[id])), nil
}
func (s *fakeSource) Page(_ context.Context, id int64, from, to time.Time, after traccar.Cursor, limit int) ([]traccar.Position, error) {
	var out []traccar.Position
	for _, p := range s.positions[id] {
		f := p.FixTime.Time
		if !after.IsZero() && !(f.After(after.FixTime) || (f.Equal(after.FixTime) && p.ID > after.ID)) {
			continue
		}
		out = append(out, p)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// target records every accepted position; behave() decides the response per request.
type target struct {
	mu       sync.Mutex
	received map[int64][]int64 // deviceId -> position ids, in arrival order
	requests atomic.Int64
	behave   func(n int64) int
	body     string
}

func newTarget(behave func(n int64) int) (*target, *httptest.Server) {
	t := &target{received: map[int64][]int64{}, behave: behave, body: `{"message":"Data inserted successfully"}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := t.requests.Add(1)
		code := 200
		if t.behave != nil {
			code = t.behave(n)
		}
		raw, _ := io.ReadAll(r.Body)
		if code >= 200 && code < 300 {
			var p struct {
				Position struct {
					ID       int64 `json:"id"`
					DeviceID int64 `json:"deviceId"`
				} `json:"position"`
			}
			_ = json.Unmarshal(raw, &p)
			t.mu.Lock()
			t.received[p.Position.DeviceID] = append(t.received[p.Position.DeviceID], p.Position.ID)
			t.mu.Unlock()
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(t.body))
	}))
	return t, srv
}

func setup(t *testing.T, src Source) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := NewManager(st, func(context.Context) (Source, error) { return src, nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.PageSize, m.FlushInterval, m.MinBackoff, m.MaxBackoff = 7, 20*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond
	return m, st
}

func createJob(t *testing.T, st *store.Store, src *fakeSource, url string, concurrency int) int64 {
	t.Helper()
	var devices []store.JobDevice
	for _, d := range src.devices {
		devices = append(devices, store.JobDevice{DeviceID: d.ID, Name: d.Name, UniqueID: d.UniqueID, Total: -1})
	}
	id, err := st.CreateJob(context.Background(), store.Job{Name: "t", TargetURL: url, From: t0, To: t0.Add(time.Hour),
		Concurrency: concurrency, TimeoutSeconds: 5}, devices)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func waitStatus(t *testing.T, st *store.Store, id int64, want string) store.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := st.Job(context.Background(), id)
		if j.Status == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := st.Job(context.Background(), id)
	t.Fatalf("job status %q, want %q", j.Status, want)
	return j
}

// assertExactlyOnceInOrder checks every position arrived once and in (fixtime, id) order per device.
func assertExactlyOnceInOrder(t *testing.T, src *fakeSource, tg *target) {
	t.Helper()
	tg.mu.Lock()
	defer tg.mu.Unlock()
	for dev, positions := range src.positions {
		got := tg.received[dev]
		if len(got) != len(positions) {
			t.Fatalf("device %d: received %d positions, want %d", dev, len(got), len(positions))
		}
		for i, p := range positions {
			if got[i] != p.ID {
				t.Fatalf("device %d: position #%d is %d, want %d (order or duplicate problem)", dev, i, got[i], p.ID)
			}
		}
	}
}

func TestDeliversEveryPositionOnceInOrder(t *testing.T) {
	src := newFakeSource(5, 53)
	tg, srv := newTarget(nil)
	defer srv.Close()
	m, st := setup(t, src)
	id := createJob(t, st, src, srv.URL, 3)
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	j := waitStatus(t, st, id, store.StatusCompleted)
	assertExactlyOnceInOrder(t, src, tg)
	if j.Total != 265 || j.Sent != 265 || j.Rejected != 0 || j.PossibleDuplicates != 0 {
		t.Fatalf("counters: %+v", j)
	}
	resp, _ := st.Responses(context.Background(), id)
	if len(resp) != 1 || resp[0].Message != "Data inserted successfully" || resp[0].Count != 265 {
		t.Fatalf("responses: %+v", resp)
	}
}

func TestRetriesUntilTargetAccepts(t *testing.T) {
	src := newFakeSource(2, 20)
	// every 3rd request fails with 503, plus a 502 burst
	tg, srv := newTarget(func(n int64) int {
		if n%3 == 0 || (n > 10 && n < 16) {
			return 503
		}
		return 201
	})
	defer srv.Close()
	m, st := setup(t, src)
	id := createJob(t, st, src, srv.URL, 2)
	_ = m.Start(id)
	j := waitStatus(t, st, id, store.StatusCompleted)
	assertExactlyOnceInOrder(t, src, tg)
	if j.Retries == 0 || j.PossibleDuplicates != 0 || j.WaitingSince != nil {
		t.Fatalf("counters: %+v", j)
	}
}

func TestTargetDownThenUp(t *testing.T) {
	src := newFakeSource(2, 10)
	tg, srv := newTarget(nil)
	addr := srv.Listener.Addr().String()
	srv.Close() // target offline: connections are refused
	m, st := setup(t, src)
	id := createJob(t, st, src, "http://"+addr, 2)
	_ = m.Start(id)
	time.Sleep(150 * time.Millisecond)
	j, _ := st.Job(context.Background(), id)
	if j.Status != store.StatusRunning || j.WaitingSince == nil || j.Sent != 0 {
		t.Fatalf("expected running and waiting while target is down: %+v", j)
	}
	// bring the target back on the same address
	srv2 := httptest.NewUnstartedServer(srv.Config.Handler)
	l, err := netListen(addr)
	if err != nil {
		t.Skip("cannot rebind test port:", err)
	}
	srv2.Listener = l
	srv2.Start()
	defer srv2.Close()
	j = waitStatus(t, st, id, store.StatusCompleted)
	assertExactlyOnceInOrder(t, src, tg)
	if j.PossibleDuplicates != 0 {
		t.Fatalf("refused connections must not count as possible duplicates: %+v", j)
	}
}

func TestRejectedPositionsAreCountedAndSkipped(t *testing.T) {
	src := newFakeSource(1, 10)
	tg, srv := newTarget(func(n int64) int {
		if n == 4 {
			return 400
		}
		return 201
	})
	tg.body = `{"message":"Unknown object"}`
	defer srv.Close()
	m, st := setup(t, src)
	id := createJob(t, st, src, srv.URL, 1)
	_ = m.Start(id)
	j := waitStatus(t, st, id, store.StatusCompleted)
	if j.Sent != 9 || j.Rejected != 1 {
		t.Fatalf("counters: %+v", j)
	}
	resp, _ := st.Responses(context.Background(), id)
	got := map[string]int64{}
	for _, r := range resp {
		got[r.Outcome+":"+r.Message] = r.Count
	}
	if got["sent:Unknown object"] != 9 || got["rejected:Unknown object"] != 1 {
		t.Fatalf("responses: %+v", resp)
	}
}

func TestPauseResumeAndRestartDeliverExactlyOnce(t *testing.T) {
	src := newFakeSource(4, 60)
	slow := func(int64) int { time.Sleep(2 * time.Millisecond); return 200 }
	tg, srv := newTarget(slow)
	defer srv.Close()
	m, st := setup(t, src)
	id := createJob(t, st, src, srv.URL, 2)
	_ = m.Start(id)
	time.Sleep(60 * time.Millisecond)
	m.Stop(id, store.StatusPaused)
	j := waitStatus(t, st, id, store.StatusPaused)
	if j.Sent == 0 || j.Sent >= 240 {
		t.Fatalf("pause should land mid-job, sent=%d", j.Sent)
	}
	_ = m.Start(id)
	time.Sleep(60 * time.Millisecond)
	// simulate a relay restart: graceful shutdown, then a new manager resumes interrupted jobs
	m.Shutdown(5 * time.Second)
	m2 := NewManager(st, m.openSource, m.log)
	m2.PageSize, m2.FlushInterval, m2.MinBackoff, m2.MaxBackoff = m.PageSize, m.FlushInterval, m.MinBackoff, m.MaxBackoff
	m2.ResumeInterrupted(context.Background(), true)
	j = waitStatus(t, st, id, store.StatusCompleted)
	assertExactlyOnceInOrder(t, src, tg)
	if j.Sent != 240 || j.PossibleDuplicates != 0 {
		t.Fatalf("counters: %+v", j)
	}
	devices, _ := st.JobDevices(context.Background(), id)
	sort.Slice(devices, func(a, b int) bool { return devices[a].DeviceID < devices[b].DeviceID })
	for _, d := range devices {
		if !d.Done || d.Sent != 60 || d.Total != 60 {
			t.Fatalf("device progress: %+v", d)
		}
	}
}

// A position left "in flight" by an unclean stop is resent once and counted as a possible duplicate.
func TestUncleanStopCountsInflightPositionAsPossibleDuplicate(t *testing.T) {
	src := newFakeSource(3, 15)
	tg, srv := newTarget(nil)
	defer srv.Close()
	m, st := setup(t, src)
	id := createJob(t, st, src, srv.URL, 2)
	ctx := context.Background()
	_ = st.SetDeviceTotal(ctx, id, 1, 15)
	_ = st.SetDeviceTotal(ctx, id, 2, 15)
	_ = st.SetDeviceTotal(ctx, id, 3, 15)
	_ = st.SetTotal(ctx, id)
	// as if the relay was killed while posting device 2's first position
	_ = st.MarkInflight(ctx, id, 2, src.positions[2][0].ID)
	_ = st.SetStatus(ctx, id, store.StatusRunning)
	m.ResumeInterrupted(ctx, false)
	j := waitStatus(t, st, id, store.StatusCompleted)
	assertExactlyOnceInOrder(t, src, tg)
	if j.PossibleDuplicates != 1 || j.Sent != 45 {
		t.Fatalf("counters: %+v", j)
	}
	devices, _ := st.JobDevices(ctx, id)
	for _, d := range devices {
		if d.InflightID != 0 {
			t.Fatalf("in-flight marker left on device %d", d.DeviceID)
		}
	}
}

// Waiting is reported once per outage, not once per worker.
func TestWaitingIsReportedOncePerOutage(t *testing.T) {
	src := newFakeSource(4, 10)
	var down atomic.Bool
	down.Store(true)
	tg, srv := newTarget(func(int64) int {
		if down.Load() {
			return 503
		}
		return 200
	})
	defer srv.Close()
	m, st := setup(t, src)
	id := createJob(t, st, src, srv.URL, 4)
	_ = m.Start(id)
	time.Sleep(150 * time.Millisecond)
	down.Store(false)
	waitStatus(t, st, id, store.StatusCompleted)
	assertExactlyOnceInOrder(t, src, tg)
	events, _ := st.Events(context.Background(), id, 100)
	waits := 0
	for _, e := range events {
		if strings.HasPrefix(e.Message, "Waiting:") {
			waits++
		}
	}
	if waits != 1 {
		t.Fatalf("expected one waiting event, got %d: %+v", waits, events)
	}
}

// Every timed-out attempt may be stored by a slow target, so each one is counted.
func TestEachAmbiguousAttemptIsCounted(t *testing.T) {
	src := newFakeSource(1, 3)
	// the first two requests are processed by the target but answered too late
	tg, srv := newTarget(func(n int64) int {
		if n <= 2 {
			time.Sleep(1500 * time.Millisecond)
		}
		return 200
	})
	defer srv.Close()
	m, st := setup(t, src)
	id := createJob(t, st, src, srv.URL, 1)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, "UPDATE jobs SET timeout_seconds = 1 WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	_ = m.Start(id)
	j := waitStatus(t, st, id, store.StatusCompleted)
	time.Sleep(2 * time.Second) // let the abandoned slow requests finish on the target
	tg.mu.Lock()
	extra := len(tg.received[1]) - 3
	tg.mu.Unlock()
	if extra != 2 || j.PossibleDuplicates != 2 {
		t.Fatalf("target got %d extra copies, relay counted %d possible duplicates", extra, j.PossibleDuplicates)
	}
}
