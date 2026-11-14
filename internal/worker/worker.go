// Package worker runs Sextant's single-item queue loop against Muster:
// flushing spooled outcomes, leasing one recorded item at a time, sending
// lease heartbeats while passively fetching the public target, running
// deterministic sub-tool extraction, staging sanitized outcomes to local disk,
// and acknowledging the step to Muster.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ClearedStack/sextant/internal/derive"
	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/fetch"
	"github.com/ClearedStack/sextant/internal/muster"
	"github.com/ClearedStack/sextant/internal/observation"
	"github.com/ClearedStack/sextant/internal/spool"
	"github.com/ClearedStack/sextant/internal/subtool"
	"github.com/ClearedStack/sextant/tools"
)

const (
	defaultPollInterval      = 2 * time.Second
	defaultHeartbeatInterval = 20 * time.Second
)

// Config configures a Sextant queue Worker.
type Config struct {
	Muster            muster.Client
	Fetcher           *fetch.Fetcher
	Registry          *tools.Registry
	Tables            derive.Tables // zero value: derive.Default()
	Spool             *spool.Spool
	Log               io.Writer
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	ObservedOn        string // optional YYYY-MM-DD override; otherwise UTC date of lease expiry
	Drain             bool   // exit cleanly once spool is empty and Muster returns 204 No Content
}

// Worker processes one leased item at a time within Sextant's memory budget.
type Worker struct {
	muster            muster.Client
	fetcher           *fetch.Fetcher
	registry          *tools.Registry
	tables            derive.Tables
	spool             *spool.Spool
	log               io.Writer
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	observedOn        string
	drain             bool
}

// New validates cfg and builds a Worker.
func New(cfg Config) (*Worker, error) {
	if cfg.Muster == nil {
		return nil, errors.New("worker: Muster client is required")
	}
	if cfg.Fetcher == nil {
		return nil, errors.New("worker: Fetcher is required")
	}
	if cfg.Registry == nil {
		return nil, errors.New("worker: Registry is required")
	}
	if cfg.Spool == nil {
		return nil, errors.New("worker: Spool is required")
	}
	if cfg.ObservedOn != "" && !observation.ValidDate(cfg.ObservedOn) {
		return nil, errors.New("worker: ObservedOn must be YYYY-MM-DD")
	}
	tables := cfg.Tables
	if tables.Citations == nil && tables.Templates == nil {
		tables = derive.Default()
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	hb := cfg.HeartbeatInterval
	if hb <= 0 {
		hb = defaultHeartbeatInterval
	}
	logOut := cfg.Log
	if logOut == nil {
		logOut = io.Discard
	}
	return &Worker{
		muster:            cfg.Muster,
		fetcher:           cfg.Fetcher,
		registry:          cfg.Registry,
		tables:            tables,
		spool:             cfg.Spool,
		log:               logOut,
		pollInterval:      poll,
		heartbeatInterval: hb,
		observedOn:        cfg.ObservedOn,
		drain:             cfg.Drain,
	}, nil
}

type logEvent struct {
	Event       string         `json:"event"`
	ItemID      string         `json:"item_id,omitempty"`
	Outcome     spool.Outcome  `json:"outcome,omitempty"`
	SubTool     string         `json:"sub_tool,omitempty"`
	Code        errcode.Code   `json:"code,omitempty"`
	Reason      errcode.Reason `json:"reason,omitempty"`
	Fingerprint string         `json:"fingerprint,omitempty"`
}

// Run executes the single-item queue loop until ctx is canceled or (when Drain
// is enabled) until the spool is empty and Muster reports 204 No Content.
func (w *Worker) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		worked, err := w.StepOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !sleepContext(ctx, w.pollInterval) {
				return nil
			}
			continue
		}
		if worked {
			continue
		}
		if w.drain {
			return nil
		}
		if !sleepContext(ctx, w.pollInterval) {
			return nil
		}
	}
}

// StepOnce flushes any spooled outcomes and, if the spool is clear, leases and
// processes at most one item from Muster. It reports whether a step was leased.
func (w *Worker) StepOnce(ctx context.Context) (bool, error) {
	if err := w.flushSpool(ctx); err != nil {
		return false, err
	}

	step, err := w.muster.Lease(ctx)
	if err != nil {
		w.emitLog(logEvent{
			Event:  "lease_poll_failed",
			Code:   classifyHubCode(err),
			Reason: errcode.UpstreamUnavailable,
		})
		return false, err
	}
	if step == nil {
		return false, nil
	}

	safeID := safeItemToken(step.ID)
	address, observedOn, valid := w.extractTarget(step)
	if !valid {
		coded := errcode.New(errcode.Parse, errcode.BrokenLease)
		if _, stageErr := w.spool.Stage(spool.Entry{
			Lease:       step.Lease,
			ItemID:      safeID,
			Outcome:     spool.OutcomeFail,
			Code:        coded.Code,
			Reason:      coded.Reason,
			Fingerprint: coded.Fingerprint(),
		}); stageErr != nil {
			return true, stageErr
		}
		w.emitLog(logEvent{
			Event:       "stopped",
			ItemID:      safeID,
			Outcome:     spool.OutcomeFail,
			Code:        coded.Code,
			Reason:      coded.Reason,
			Fingerprint: coded.Fingerprint(),
		})
		return true, w.flushSpool(ctx)
	}

	workCtx, workCancel := context.WithCancel(ctx)
	hbCtx, hbCancel := context.WithCancel(ctx)
	var leaseLost atomic.Bool
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(w.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				_, hbErr := w.muster.Heartbeat(hbCtx, step.Lease)
				if hbErr != nil {
					var ref *muster.Refusal
					if errors.As(hbErr, &ref) && ref.LeaseNotHeld() {
						leaseLost.Store(true)
						workCancel()
						return
					}
				}
			}
		}
	}()

	res, procErr := w.observeTarget(workCtx, step.ID, address, observedOn)

	hbCancel()
	<-hbDone
	workCancel()

	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if leaseLost.Load() {
		w.emitLog(logEvent{
			Event:  "lease_lost",
			ItemID: step.ID,
		})
		return true, nil
	}

	if procErr == nil {
		if _, err := w.spool.Stage(spool.Entry{
			Lease:   step.Lease,
			ItemID:  step.ID,
			Outcome: spool.OutcomeComplete,
			State:   "observed",
			Result:  res,
		}); err != nil {
			return true, err
		}
		w.emitLog(logEvent{
			Event:   "observed",
			ItemID:  step.ID,
			Outcome: spool.OutcomeComplete,
			SubTool: res.SubTool,
		})
		return true, w.flushSpool(ctx)
	}

	coded, ok := errcode.From(procErr)
	if !ok {
		coded = errcode.New(errcode.Parse, errcode.EmptyPage)
	}
	outcome := spool.OutcomeFail
	if coded.Code == errcode.NeedsSubtool {
		outcome = spool.OutcomeNeedsSubtool
	}
	if _, err := w.spool.Stage(spool.Entry{
		Lease:       step.Lease,
		ItemID:      step.ID,
		Outcome:     outcome,
		Code:        coded.Code,
		Reason:      coded.Reason,
		Fingerprint: coded.Fingerprint(),
	}); err != nil {
		return true, err
	}
	w.emitLog(logEvent{
		Event:       "stopped",
		ItemID:      step.ID,
		Outcome:     outcome,
		Code:        coded.Code,
		Reason:      coded.Reason,
		Fingerprint: coded.Fingerprint(),
	})
	return true, w.flushSpool(ctx)
}

func (w *Worker) observeTarget(ctx context.Context, itemID, address, observedOn string) (*derive.Result, error) {
	res, err := w.fetcher.Fetch(ctx, address)
	if res != nil {
		defer res.Wipe()
	}
	if err != nil {
		return nil, err
	}
	rec, err := w.registry.Observe(subtool.Target{
		ItemID:     itemID,
		URL:        res.FinalURL,
		Page:       res.Body,
		ObservedOn: observedOn,
	})
	if err != nil {
		return nil, err
	}
	// The observation record stays inside Sextant. Only the derived result,
	// of closed words and numbers, is staged and sent.
	return w.tables.Derive(rec)
}

func (w *Worker) flushSpool(ctx context.Context) error {
	pending, err := w.spool.Pending()
	if err != nil {
		return err
	}
	for _, entry := range pending {
		var sendErr error
		switch entry.Outcome {
		case spool.OutcomeComplete:
			_, sendErr = w.muster.Complete(ctx, entry.Lease, entry.State, entry.Result)
		case spool.OutcomeNeedsSubtool:
			_, sendErr = w.muster.NeedsSubtool(ctx, entry.Lease)
		case spool.OutcomeFail:
			_, sendErr = w.muster.Fail(ctx, entry.Lease, entry.Code)
		}
		if sendErr == nil {
			if err := w.spool.Remove(entry); err != nil {
				return err
			}
			continue
		}

		var ref *muster.Refusal
		if errors.As(sendErr, &ref) {
			if ref.LeaseNotHeld() {
				if err := w.spool.Remove(entry); err != nil {
					return err
				}
				w.emitLog(logEvent{
					Event:   "dropped_expired_lease",
					ItemID:  entry.ItemID,
					Outcome: entry.Outcome,
				})
				continue
			}
			if ref.Retryable() || ref.Status == 401 {
				return sendErr
			}
			// Permanent hub refusal (400, 403, 409 move_not_allowed, 413, 422):
			// quarantine so subsequent items are not blocked.
			if err := w.spool.Quarantine(entry); err != nil {
				return err
			}
			w.emitLog(logEvent{
				Event:   "quarantined",
				ItemID:  entry.ItemID,
				Outcome: entry.Outcome,
				Code:    ref.Code,
			})
			continue
		}

		// Transport timeout or connection failure to Muster: keep in spool and retry.
		return sendErr
	}
	return nil
}

func (w *Worker) extractTarget(step *muster.Step) (address, observedOn string, ok bool) {
	if step == nil || step.Kind != "item" || step.State != "recorded" || !observation.ValidItemID(step.ID) {
		return "", "", false
	}
	if step.Found != nil && strings.TrimSpace(step.Found.Address) != "" {
		address = strings.TrimSpace(step.Found.Address)
	} else if rawTrawl, hasTrawl := step.Results["trawl"]; hasTrawl {
		var tr struct {
			Address string `json:"address"`
		}
		if json.Unmarshal(rawTrawl, &tr) == nil {
			address = strings.TrimSpace(tr.Address)
		}
	}
	if address == "" || len(address) > observation.MaxAddressBytes {
		return "", "", false
	}

	if w.observedOn != "" {
		return address, w.observedOn, true
	}
	exp, err := time.Parse(time.RFC3339, step.Expires)
	if err != nil {
		return "", "", false
	}
	date := exp.UTC().Format("2006-01-02")
	if !observation.ValidDate(date) {
		return "", "", false
	}
	return address, date, true
}

func (w *Worker) emitLog(ev logEvent) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	var buf bytes.Buffer
	buf.Write(raw)
	buf.WriteByte('\n')
	_, _ = w.log.Write(buf.Bytes())
}

func classifyHubCode(err error) errcode.Code {
	if coded, ok := errcode.From(err); ok {
		return coded.Code
	}
	var ref *muster.Refusal
	if errors.As(err, &ref) {
		return ref.Code
	}
	return errcode.Timeout
}

func safeItemToken(id string) string {
	if observation.ValidItemID(id) {
		return id
	}
	return "PA-0000"
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
