package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/A-REDA-EL/powerfleet-relay/internal/store"
)

// After a power cut MariaDB can refuse queries for a while: counting must wait, not fail the job.
func TestCountErrorsAreRetriedInsteadOfFailingTheJob(t *testing.T) {
	src := newFakeSource(3, 20)
	src.countFail.Store(4)
	tg, srv := newTarget(nil)
	defer srv.Close()
	m, st := setup(t, src)
	id := createJob(t, st, src, srv.URL, 2)
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	j := waitStatus(t, st, id, store.StatusCompleted)
	if j.Total != 60 || j.Sent != 60 {
		t.Fatalf("job: %+v", j)
	}
	assertExactlyOnceInOrder(t, src, tg)
}

// A reverse proxy answers a plain-text 404 while the Server container restarts: wait. The Server's
// own JSON 404 ("History import is disabled") is a configuration problem: fail.
func TestImportProxy404IsRetriedButServer404FailsTheJob(t *testing.T) {
	src := newFakeSource(2, 30)
	tg, inner := newImportTarget()
	defer inner.Close()
	var calls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 3 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("404 page not found"))
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	m, st := setup(t, src)
	id := createImportJob(t, st, src, proxy.URL, "k", 2)
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	j := waitStatus(t, st, id, store.StatusCompleted)
	stored, rejected := expectStored(t, src, tg, "")
	if j.Sent != stored || j.Rejected != rejected || j.Retries == 0 {
		t.Fatalf("job: %+v (want sent %d rejected %d, some retries)", j, stored, rejected)
	}

	disabled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"History import is disabled","error":"Not Found","statusCode":404}`))
	}))
	defer disabled.Close()
	id = createImportJob(t, st, src, disabled.URL, "k", 2)
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	j = waitStatus(t, st, id, store.StatusFailed)
	if !strings.Contains(j.LastError, "disabled") {
		t.Fatalf("last error: %q", j.LastError)
	}
}

// Cancelling is not final: "Continue" picks up after the last acknowledged batch.
func TestCancelledJobCanBeResumed(t *testing.T) {
	src := newFakeSource(3, 60)
	tg, srv := newImportTarget()
	defer srv.Close()
	m, st := setup(t, src)
	m.ImportBatchSize = 5
	cancelled := make(chan struct{})
	var once sync.Once
	id := createImportJob(t, st, src, srv.URL, "k", 2)
	tg.behave = func(n int64) int {
		if n == 6 {
			once.Do(func() { go func() { m.Stop(id, store.StatusCancelled); close(cancelled) }() })
		}
		return 0
	}
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	<-cancelled
	waitStatus(t, st, id, store.StatusCancelled)
	for m.Running(id) {
		waitStatus(t, st, id, store.StatusCancelled)
	}
	before := tg.requests.Load()
	if err := m.Start(id); err != nil {
		t.Fatalf("cancelled job cannot be resumed: %v", err)
	}
	j := waitStatus(t, st, id, store.StatusCompleted)
	stored, rejected := expectStored(t, src, tg, "")
	if j.Sent != stored || j.Rejected != rejected {
		t.Fatalf("job counts: %+v", j)
	}

	// the same job run uninterrupted shows how many requests starting over would take
	tg2, srv2 := newImportTarget()
	defer srv2.Close()
	id2 := createImportJob(t, st, src, srv2.URL, "k", 2)
	if err := m.Start(id2); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, id2, store.StatusCompleted)
	if resumed, full := tg.requests.Load()-before, tg2.requests.Load(); resumed >= full {
		t.Fatalf("resume sent %d requests, an uninterrupted run %d: it should continue after the checkpoint", resumed, full)
	}
}

// A vehicle that was not registered on the Server is rejected; once it is, "Retry rejected" stores it.
func TestRetryRejectedStoresWhatWasRejected(t *testing.T) {
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
	waitStatus(t, st, id, store.StatusCompleted)
	if _, err := st.RewindRejected(context.Background(), id); err != nil {
		t.Fatal(err)
	}

	tg.mu.Lock()
	tg.unknownIMEI = "" // the vehicle has been registered
	tg.mu.Unlock()
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	j := waitStatus(t, st, id, store.StatusCompleted)
	stored, rejected := expectStored(t, src, tg, "")
	if j.Sent != stored || j.Rejected != rejected {
		t.Fatalf("job counts: sent %d rejected %d, want %d %d", j.Sent, j.Rejected, stored, rejected)
	}
}

func TestRewindRejectedLeavesAJobWithoutRejectionsAlone(t *testing.T) {
	src := newFakeSource(1, 10) // ids 1..10: none is a multiple of 17
	_, srv := newImportTarget()
	defer srv.Close()
	m, st := setup(t, src)
	id := createImportJob(t, st, src, srv.URL, "k", 1)
	if err := m.Start(id); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, id, store.StatusCompleted)
	n, err := st.RewindRejected(context.Background(), id)
	if err != nil || n != 0 {
		t.Fatalf("rewound %d devices, err %v", n, err)
	}
	if j, _ := st.Job(context.Background(), id); j.Status != store.StatusCompleted || j.Sent != 10 {
		t.Fatalf("job changed: %+v", j)
	}
}
