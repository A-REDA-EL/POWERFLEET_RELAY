package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/A-REDA-EL/powerfleet-relay/internal/store"
)

// importTarget mimics POST /api/server/import: it stores each position once (skipping ones it
// already has), rejects every position whose id is a multiple of 17 ("Invalid speed"), and
// answers 404 "Unknown object" for unknownIMEI.
type importTarget struct {
	mu          sync.Mutex
	stored      map[string]map[int64]bool
	order       map[string][]int64 // first-time stores, in arrival order
	requests    atomic.Int64
	maxBatch    int               // >0: answer 413 above this many positions
	behave      func(n int64) int // optional status override per request (e.g. 503)
	unknownIMEI string
	key         string
}

func newImportTarget() (*importTarget, *httptest.Server) {
	t := &importTarget{stored: map[string]map[int64]bool{}, order: map[string][]int64{}, key: "k"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := t.requests.Add(1)
		if r.Header.Get("X-Api-Key") != t.key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Invalid API key"}`))
			return
		}
		if t.behave != nil {
			if code := t.behave(n); code != 0 {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"message":"Database unavailable"}`))
				return
			}
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Device struct {
				UniqueID string `json:"uniqueId"`
			} `json:"device"`
			Positions []struct {
				ID int64 `json:"id"`
			} `json:"positions"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if t.maxBatch > 0 && len(req.Positions) > t.maxBatch {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		if req.Device.UniqueID == t.unknownIMEI {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Unknown object","error":"Not Found","statusCode":404}`))
			return
		}
		t.mu.Lock()
		if t.stored[req.Device.UniqueID] == nil {
			t.stored[req.Device.UniqueID] = map[int64]bool{}
		}
		var inserted, duplicates int
		var rejected []string
		for i, p := range req.Positions {
			switch {
			case p.ID%17 == 0:
				rejected = append(rejected, fmt.Sprintf(`{"index":%d,"reason":"Invalid speed"}`, i))
			case t.stored[req.Device.UniqueID][p.ID]:
				duplicates++
			default:
				t.stored[req.Device.UniqueID][p.ID] = true
				t.order[req.Device.UniqueID] = append(t.order[req.Device.UniqueID], p.ID)
				inserted++
			}
		}
		t.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"message":"Imported","inserted":%d,"duplicates":%d,"rejected":[%s],"vehicleUpdated":false}`,
			inserted, duplicates, strings.Join(rejected, ","))
	}))
	return t, srv
}

func createImportJob(t *testing.T, st *store.Store, src *fakeSource, url, key string, concurrency int) int64 {
	t.Helper()
	var devices []store.JobDevice
	for _, d := range src.devices {
		devices = append(devices, store.JobDevice{DeviceID: d.ID, Name: d.Name, UniqueID: d.UniqueID, Total: -1})
	}
	id, err := st.CreateJob(context.Background(), store.Job{Name: "import", TargetURL: url, Mode: store.ModeImport,
		Headers: map[string]string{"X-Api-Key": key}, From: t0, To: t0.Add(1e12), Concurrency: concurrency, TimeoutSeconds: 5}, devices)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// expectStored checks every non-rejected position was stored exactly once, in order.
func expectStored(t *testing.T, src *fakeSource, tg *importTarget, skipIMEI string) (stored, rejected int64) {
	t.Helper()
	tg.mu.Lock()
	defer tg.mu.Unlock()
	for _, d := range src.devices {
		var want []int64
		for _, p := range src.positions[d.ID] {
			if d.UniqueID == skipIMEI || p.ID%17 == 0 {
				rejected++
				continue
			}
			want = append(want, p.ID)
		}
		got := tg.order[d.UniqueID]
		if d.UniqueID == skipIMEI {
			if len(got) != 0 {
				t.Fatalf("%s: unknown vehicle stored %d positions", d.UniqueID, len(got))
			}
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: stored %v\nwant %v", d.UniqueID, got, want)
		}
		stored += int64(len(want))
	}
	return stored, rejected
}

func TestImportStoresEveryPositionOnceThroughFailures(t *testing.T) {
	src := newFakeSource(4, 53)
	tg, srv := newImportTarget()
	defer srv.Close()
	tg.behave = func(n int64) int {
		if n%4 == 0 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	m, st := setup(t, src)
	m.ImportBatchSize = 10
	id := createImportJob(t, st, src, srv.URL, "k", 2)
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	j := waitStatus(t, st, id, store.StatusCompleted)
	stored, rejected := expectStored(t, src, tg, "")
	if j.Total != 212 || j.Sent != stored || j.Rejected != rejected || j.PossibleDuplicates != 0 || j.Retries == 0 {
		t.Fatalf("job counts: %+v (stored %d, rejected %d)", j, stored, rejected)
	}
	responses, _ := st.Responses(context.Background(), id)
	got := map[string]int64{}
	for _, r := range responses {
		got[r.Outcome+" "+r.Message] = r.Count
	}
	if got["sent Imported"] != stored || got["rejected Invalid speed"] != rejected {
		t.Fatalf("responses %v", got)
	}
}

func TestImportHalvesBatchesOn413(t *testing.T) {
	src := newFakeSource(2, 40)
	tg, srv := newImportTarget()
	defer srv.Close()
	tg.maxBatch = 6
	m, st := setup(t, src)
	m.ImportBatchSize = 25
	id := createImportJob(t, st, src, srv.URL, "k", 1)
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, id, store.StatusCompleted)
	expectStored(t, src, tg, "")
}

func TestImportRejectsUnknownVehicleAndContinues(t *testing.T) {
	src := newFakeSource(3, 30)
	tg, srv := newImportTarget()
	defer srv.Close()
	tg.unknownIMEI = src.devices[1].UniqueID
	m, st := setup(t, src)
	m.ImportBatchSize = 8
	id := createImportJob(t, st, src, srv.URL, "k", 2)
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	j := waitStatus(t, st, id, store.StatusCompleted)
	stored, rejected := expectStored(t, src, tg, tg.unknownIMEI)
	if j.Sent != stored || j.Rejected != rejected {
		t.Fatalf("job counts: sent %d rejected %d, want %d %d", j.Sent, j.Rejected, stored, rejected)
	}
}

func TestImportWrongKeyFailsTheJob(t *testing.T) {
	src := newFakeSource(2, 30)
	tg, srv := newImportTarget()
	defer srv.Close()
	m, st := setup(t, src)
	id := createImportJob(t, st, src, srv.URL, "wrong", 2)
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	j := waitStatus(t, st, id, store.StatusFailed)
	if !strings.Contains(j.LastError, "API key") || j.Sent != 0 || j.Rejected != 0 {
		t.Fatalf("job: %+v", j)
	}
	if n := tg.requests.Load(); n > 2 {
		t.Fatalf("%d requests after a 401; the job should stop at once", n)
	}
}

func TestImportPauseResumeStoresEachOnce(t *testing.T) {
	src := newFakeSource(3, 60)
	tg, srv := newImportTarget()
	defer srv.Close()
	m, st := setup(t, src)
	m.ImportBatchSize = 5
	paused := make(chan struct{})
	var once sync.Once
	id := createImportJob(t, st, src, srv.URL, "k", 2)
	tg.behave = func(n int64) int {
		if n == 6 {
			once.Do(func() { go func() { m.Stop(id, store.StatusPaused); close(paused) }() })
		}
		return 0
	}
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	<-paused
	waitStatus(t, st, id, store.StatusPaused)
	for m.Running(id) {
		waitStatus(t, st, id, store.StatusPaused)
	}
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	j := waitStatus(t, st, id, store.StatusCompleted)
	stored, rejected := expectStored(t, src, tg, "")
	if j.Sent != stored || j.Rejected != rejected || j.PossibleDuplicates != 0 {
		t.Fatalf("job counts: %+v", j)
	}
}
