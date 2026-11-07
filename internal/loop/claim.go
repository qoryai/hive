package loop

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/qoryai/hive/internal/adapter"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/integrations/contracts"
)

// The reasons of the loop's releases. Each is 1 to 200 characters, as the provisioner
// contract requires, and contains no text of the adapter.
const (
	// ReasonLimit is the reason after a make that failed with the kind limit.
	ReasonLimit = "the machine was not made: the vendor reported a limit, such as a quota"
	// ReasonRetry is the reason after a make that failed with the kind retry.
	ReasonRetry = "the machine was not made: the vendor reported a failure that a later try may clear"
	// ReasonDefect is the reason after a make that failed with the kind defect.
	ReasonDefect = "the machine was not made: the machine adapter's settings or the requested machine type are wrong"
	// ReasonOther is the reason after a make that failed with the kind other.
	ReasonOther = "the machine was not made: the machine adapter failed"
	// ReasonStartData is the reason of a claim whose start data could not be built.
	ReasonStartData = "the machine was not made: the claim's run code, gateway URL or lifetime does not fit in the start data"
	// ReasonNoSpec is the reason of a claim with no machine spec.
	ReasonNoSpec = "the machine was not made: the claim has no machine spec, so the provisioner cannot choose a machine"
	// ReasonPool is the reason of a claim for another pool than the provisioner's.
	ReasonPool = "the machine was not made: the claim's pool is not the provisioner's pool"
	// ReasonJournal is the reason of a claim that the journal could not record.
	ReasonJournal = "the machine was not made: writing the provisioner's journal failed"
	// ReasonStopped is the reason of a claim when the provisioner stops before its make.
	ReasonStopped = "the provisioner stopped before a machine was made"
)

// makeFailure is the release reason, the wait before the next claim and the log level
// of a make that failed with the kind k.
func makeFailure(k string) (reason string, wait time.Duration, level slog.Level) {
	switch k {
	case contracts.FailureLimit:
		return ReasonLimit, LongWait, slog.LevelWarn
	case contracts.FailureRetry:
		return ReasonRetry, ShortWait, slog.LevelWarn
	case contracts.FailureDefect:
		return ReasonDefect, LongWait, slog.LevelError
	}
	return ReasonOther, ShortWait, slog.LevelWarn
}

// kindOf is the kind of a failed make. An error that is not an *adapter.Error is a
// request that fails a rule of the protocol, a defect.
func kindOf(err error) string {
	var ae *adapter.Error
	if errors.As(err, &ae) {
		return ae.Kind
	}
	return contracts.FailureDefect
}

// claim is the claim path. It claims one dispatch at a time, while the journal has
// fewer machines than MaxMachines, until ctx ends. It returns an error only when the
// journal cannot be written before or after a make.
func (l *loop) claim(ctx context.Context) error {
	defer l.Log.Info("stopped claiming")
	for {
		for l.machines() >= l.MaxMachines {
			select {
			case <-ctx.Done():
				return nil
			case <-l.freed:
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		c, ok, err := l.Apiary.Claim(ctx, l.offers)
		switch {
		case ok:
			if err := l.handle(ctx, c); err != nil {
				return err
			}
		case ctx.Err() != nil:
			return nil
		case err != nil:
			l.Log.Warn("claiming a dispatch failed", errorAttr(err, nil))
			l.wait(ctx, ShortWait)
		}
	}
}

// machines is the number of journal entries that count toward MaxMachines: all but the
// ended ones, whose machine is gone.
func (l *loop) machines() int {
	n := 0
	for _, e := range l.Journal.List() {
		if e.Phase != journal.PhaseEnded {
			n++
		}
	}
	return n
}

// handle makes the machine of the claim c and sends dispatched. When the claim has no
// machine spec or the make fails, it releases the dispatch. When the release after a
// make returns 204 or 409 lease_expired, it then uploads the make's error as the
// dispatch's machine log. It returns an error only when the journal cannot be written
// before or after the make.
func (l *loop) handle(ctx context.Context, c Claim) error {
	id := c.DispatchID
	l.Log.Info("claimed a dispatch", slog.String("dispatch_id", id))
	if ctx.Err() != nil {
		l.release(ctx, id, ReasonStopped)
		return nil
	}
	if c.PoolID != l.PoolID {
		l.Log.Error("the claim's pool is not the provisioner's pool", slog.String("dispatch_id", id))
		l.release(ctx, id, ReasonPool)
		l.wait(ctx, LongWait)
		return nil
	}
	if c.MachineSpec == nil {
		l.Log.Error("the claim has no machine spec, though the claim had offers", slog.String("dispatch_id", id))
		l.release(ctx, id, ReasonNoSpec)
		l.wait(ctx, LongWait)
		return nil
	}
	data, err := buildStartData(c, l.GatewayAddress)
	if err != nil {
		l.Log.Error("building the start data failed", slog.String("dispatch_id", id), errorAttr(err, nil))
		l.release(ctx, id, ReasonStartData)
		l.wait(ctx, LongWait)
		return nil
	}
	defer clear(data)
	l.setBusy(id, true)
	defer l.setBusy(id, false)
	if err := l.Journal.Set(journal.Entry{DispatchID: id, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: c.MaxLifetimeMinutes}); err != nil {
		l.Log.Error("writing the journal failed", slog.String("dispatch_id", id), errorAttr(err, nil))
		l.release(ctx, id, ReasonJournal)
		return err
	}
	l.setTime(l.claimed, id)
	if ctx.Err() != nil {
		l.releaseEntry(ctx, id, ReasonStopped)
		return nil
	}
	// A make in flight finishes after ctx ends, so that its machine is recorded.
	made, err := l.Machines.Make(context.WithoutCancel(ctx), id, c.PoolID, *c.MachineSpec, data)
	madeAt := l.Clock.Now()
	if err != nil {
		kind := kindOf(err)
		reason, wait, level := makeFailure(kind)
		l.Log.Log(ctx, level, "making a machine failed", slog.String("dispatch_id", id), slog.String("kind", string(kind)), errorAttr(err, data))
		// Only 204 and 409 lease_expired show that no machine redeemed the run code.
		// After any other answer a machine may exist, and its log needs the dispatch's
		// one machine log.
		if rerr := l.releaseEntry(ctx, id, reason); rerr == nil || errors.Is(rerr, ErrLeaseExpired) {
			l.uploadMakeError(ctx, id, shown(err, data), data)
		}
		// An entry kept by 409 machine_exists is the watch's during the wait.
		l.setBusy(id, false)
		l.wait(ctx, wait)
		return nil
	}
	machineType := made.MachineType
	if machineType != "" && !journal.IsMachineType(machineType) {
		// The contract allows a type that the journal cannot keep, such as - or one
		// with the start of a secret. The log line leaves the type out.
		l.Log.Warn("the machine type from make is - or contains the start of a secret, so the journal keeps no type and dispatched sends none",
			slog.String("dispatch_id", id), slog.String("machine_id", made.ID))
		machineType = ""
	}
	e := journal.Entry{DispatchID: id, Phase: journal.PhaseMade, MachineID: made.ID, MaxLifetimeMinutes: c.MaxLifetimeMinutes, MachineType: machineType}
	if err := l.Journal.Set(e); err != nil {
		// The journal has the claimed entry, so a restart finds the machine by its key.
		l.Log.Error("writing the journal failed", slog.String("dispatch_id", id), slog.String("machine_id", made.ID), errorAttr(err, nil))
		return err
	}
	l.mu.Lock()
	l.made[id] = madeAt
	l.mu.Unlock()
	l.Log.Info("made a machine", slog.String("dispatch_id", id), slog.String("machine_id", made.ID))
	if ctx.Err() != nil {
		return nil
	}
	l.dispatched(ctx, e)
	return nil
}

// uploadMakeError uploads text, the error of a make that made no machine as [shown]
// gives it, as the machine log of the dispatch id. The machine log is for instance
// admins, and the release reason, which organisation members read, stays a fixed text.
// The upload runs also after ctx ends, as the release before it does. A failed upload
// is logged, with startData hidden from its error.
func (l *loop) uploadMakeError(ctx context.Context, id, text string, startData []byte) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := l.Apiary.MachineLog(ctx, id, text, false); err != nil {
		l.Log.Warn("uploading the make's error as the machine log failed", slog.String("dispatch_id", id), errorAttr(err, startData))
		return
	}
	l.Log.Info("uploaded the make's error as the machine log", slog.String("dispatch_id", id))
}

// dispatched sends dispatched for the entry e, which is made or dispatched, with the
// entry's machine type. On 204, a made entry becomes dispatched. On 410 gone or 409
// lease_expired, the machine is cleaned up. Any other failure is logged, and the next
// watch sends dispatched again.
func (l *loop) dispatched(ctx context.Context, e journal.Entry) {
	attrs := []any{slog.String("dispatch_id", e.DispatchID), slog.String("machine_id", e.MachineID)}
	err := l.Apiary.Dispatched(ctx, e.DispatchID, e.MachineID, e.MachineType)
	switch {
	case err == nil && e.Phase == journal.PhaseMade:
		e.Phase = journal.PhaseDispatched
		if err := l.Journal.Set(e); err != nil {
			l.Log.Error("writing the journal failed", append(attrs, errorAttr(err, nil))...)
			return
		}
		l.Log.Info("sent dispatched", attrs...)
	case err == nil:
	case errors.Is(err, ErrGone):
		l.Log.Info("the dispatch ended", attrs...)
		l.cleanUp(ctx, e)
	case errors.Is(err, ErrLeaseExpired):
		l.Log.Warn("the lease of the dispatch expired before dispatched", attrs...)
		l.cleanUp(ctx, e)
	default:
		l.Log.Warn("sending dispatched failed", append(attrs, errorAttr(err, nil))...)
	}
}
