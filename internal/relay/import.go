package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/A-REDA-EL/powerfleet-relay/internal/store"
	"github.com/A-REDA-EL/powerfleet-relay/internal/traccar"
)

// Import mode posts each device's positions in batches to the PowerFleet Server history import
// (POST /api/server/import). The Server stores history only and skips positions it already has,
// so a batch can be sent again after any failure without creating duplicates: there is no
// in-flight marker and nothing is counted as a possible duplicate.

// importAnswer is the Server's reply to one batch.
type importAnswer struct {
	Message    string `json:"message"`
	Inserted   int64  `json:"inserted"`
	Duplicates int64  `json:"duplicates"`
	Rejected   []struct {
		Index  int    `json:"index"`
		Reason string `json:"reason"`
	} `json:"rejected"`
}

// batchOutcome is what one delivered batch did.
type batchOutcome struct {
	tooLarge    bool             // HTTP 413: send fewer positions per request
	inserted    int64            // stored
	duplicates  int64            // already present on the target
	rejected    map[string]int64 // not stored, by reason
	status      int              // HTTP status of the final answer
	allRejected string           // the whole batch was refused with this message (unknown vehicle, 4xx)
}

func (r *run) deviceImport(src Source, client *http.Client, limiter *rate.Limiter, job store.Job, d store.JobDevice, dev traccar.Device) error {
	ctx := context.Background()
	st := r.m.store
	cursor := traccar.Cursor{ID: d.CursorID}
	if d.CursorAt != nil {
		cursor.FixTime = *d.CursorAt
	}
	batchSize := max(1, r.m.ImportBatchSize)
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
		for len(page) > 0 {
			if r.stopped() {
				return nil
			}
			body, n, err := r.importBody(dev, page, batchSize)
			if err != nil {
				return err
			}
			if limiter != nil {
				if err := waitLimiterN(limiter, n, r.stop); err != nil {
					return nil
				}
			}
			out, ok, err := r.deliverImport(client, job, body)
			if err != nil {
				return err
			}
			if !ok {
				return nil // stopped while retrying; the batch is sent again on resume
			}
			if out.tooLarge {
				if n == 1 {
					return fmt.Errorf("target refused a single position as too large (HTTP 413)")
				}
				batchSize = max(1, n/2)
				r.event("warn", "Target answered 413 (request too large): sending %d positions per request", batchSize)
				continue
			}
			batch := page[:n]
			last := batch[n-1]
			rejected := int64(0)
			for _, c := range out.rejected {
				rejected += c
			}
			if out.allRejected != "" {
				rejected = int64(n)
			}
			fix := time.Time{}
			if last.FixTime != nil {
				fix = last.FixTime.Time
			}
			if err := st.AdvanceBatch(ctx, r.jobID, d.DeviceID, fix, last.ID, int64(n)-rejected, rejected); err != nil {
				return err
			}
			cursor = traccar.Cursor{FixTime: fix, ID: last.ID}
			page = page[n:]

			r.mu.Lock()
			if out.allRejected != "" {
				r.progress.Responses[store.ResponseKey{Outcome: "rejected", Status: out.status, Message: out.allRejected}] += int64(n)
			} else {
				if out.inserted > 0 {
					r.progress.Responses[store.ResponseKey{Outcome: "sent", Status: out.status, Message: "Imported"}] += out.inserted
				}
				if out.duplicates > 0 {
					r.progress.Responses[store.ResponseKey{Outcome: "sent", Status: out.status, Message: "Already present"}] += out.duplicates
				}
				for reason, c := range out.rejected {
					r.progress.Responses[store.ResponseKey{Outcome: "rejected", Status: out.status, Message: reason}] += c
				}
			}
			r.mu.Unlock()
		}
	}
	return nil
}

// importBody encodes up to batchSize positions from the start of page, fewer if the body would
// exceed ImportMaxBytes. It returns the body and how many positions it holds.
func (r *run) importBody(dev traccar.Device, page []traccar.Position, batchSize int) ([]byte, int, error) {
	n := min(batchSize, len(page))
	for {
		body, err := traccar.ImportPayload(dev, page[:n])
		if err != nil {
			return nil, 0, fmt.Errorf("encode positions of %s: %w", dev.UniqueID, err)
		}
		if len(body) <= r.m.ImportMaxBytes || n == 1 {
			return body, n, nil
		}
		n = max(1, n*r.m.ImportMaxBytes/len(body)*9/10)
	}
}

// deliverImport posts one batch until the target answers definitively. ok=false means the job
// was stopped first; err is a configuration problem that would fail every batch (wrong API key,
// import disabled, wrong URL), which fails the job.
func (r *run) deliverImport(client *http.Client, job store.Job, body []byte) (out batchOutcome, ok bool, err error) {
	backoff := r.m.MinBackoff
	waiting := false
	defer func() {
		if waiting {
			r.endWait()
		}
	}()
	for {
		status, raw, postErr := postRaw(client, job, body)
		var reason string
		switch {
		case postErr != nil:
			reason = postErr.Error()
		case status >= 200 && status < 300:
			var a importAnswer
			if json.Unmarshal(raw, &a) != nil {
				return out, true, fmt.Errorf("target answered HTTP %d without an import result: is the URL the PowerFleet import (/api/server/import)?", status)
			}
			out = batchOutcome{status: status, inserted: a.Inserted, duplicates: a.Duplicates, rejected: map[string]int64{}}
			for _, rej := range a.Rejected {
				out.rejected[rej.Reason]++
			}
			return out, true, nil
		case status == http.StatusRequestEntityTooLarge:
			return batchOutcome{tooLarge: true, status: status}, true, nil
		case status == 408 || status == 425 || status == 429 || status >= 500:
			reason = fmt.Sprintf("HTTP %d %s", status, summarize(raw))
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return out, true, fmt.Errorf("target refused the API key (HTTP %d): check the X-Api-Key header and GPS_IMPORT_API_KEY on the Server", status)
		case status == http.StatusNotFound && summarize(raw) == "Unknown object":
			return batchOutcome{status: status, allRejected: "Unknown object"}, true, nil
		case status == http.StatusNotFound && !jsonObject(raw):
			// a plain-text 404 comes from a reverse proxy while the Server container restarts
			reason = "HTTP 404 " + summarize(raw) + " (the Server may be restarting)"
		case status == http.StatusNotFound:
			return out, true, fmt.Errorf("import endpoint not found or disabled (HTTP 404 %s): check the URL and GPS_IMPORT_API_KEY on the Server", summarize(raw))
		default: // 400, 422…: this batch cannot be stored
			return batchOutcome{status: status, allRejected: summarize(raw)}, true, nil
		}
		r.mu.Lock()
		r.progress.Retries++
		r.mu.Unlock()
		if !waiting {
			waiting = true
			r.beginWait("Target: " + reason)
		}
		if !r.sleep(jitter(backoff)) {
			return out, false, nil
		}
		backoff = min(backoff*2, r.m.MaxBackoff)
	}
}

// jsonObject reports whether a response body is a JSON object, as the Server's own errors are.
func jsonObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{' && json.Valid(raw)
}

func waitLimiterN(l *rate.Limiter, n int, stop <-chan struct{}) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return l.WaitN(ctx, n)
}
