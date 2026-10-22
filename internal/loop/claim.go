package loop

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/qoryai/hive/internal/adapter"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/machines"
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
	// ReasonPool is the reason of a claim for another pool than the provisioner's.
	ReasonPool = "the machine was not made: the claim's pool is not the provisioner's pool"
	// ReasonJournal is the reason of a claim that the journal could not record.
	ReasonJournal = "the machine was not made: writing the provisioner's journal failed"
	// ReasonStopped is the reason of a claim when the provisioner stops before its make.
	ReasonStopped = "the provisioner stopped before a machine was made"
)

// makeFailure is the release reason, the wait before the next claim and the log level
// of a make that failed with the kind k.
func makeFailure(k machines.Kind) (reason string, wait time.Duration, level slog.Level) {
	switch k {
	case machines.KindLimit:
		return ReasonLimit, LongWait, slog.LevelWarn
	case machines.KindRetry:
		return ReasonRetry, ShortWait, slog.LevelWarn
	case machines.KindDefect:
		return ReasonDefect, LongWait, slog.LevelError
	}
	return ReasonOther, ShortWait, slog.LevelWarn
}

// kindOf is the kind of a failed make. An error that is not an *adapter.Error is a
// request that fails a rule of the protocol, a defect.
func kindOf(err error) machines.Kind {
	var ae *adapter.Error
	if errors.As(err, &ae) {
		return ae.Kind
	}
	return machines.KindDefect
}

// claim is the claim path. It claims one dispatch at a time, while the journal has
// fewer entries than MaxMachines, until ctx ends. It returns an error only when the
// journal cannot be written before or after a make.
func (l *loop) claim(ctx context.Context) error {
	defer l.Log.Info("stopped claiming")
	for {
		for len(l.Journal.List()) >= l.MaxMachines {
			select {
			case <-ctx.Done():
				return nil
			case <-l.freed:
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		c, ok, err := l.Apiary.Claim(ctx)
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

// handle makes the machine of the claim c and sends dispatched. It returns an error
// only when the journal cannot be written before or after the make.
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
	if ctx.Err() != nil {
		l.release(ctx, id, ReasonStopped)
		l.remove(id)
		return nil
	}
	size := c.Machine
	if size == "" {
		size = DefaultSize
	}
	// A make in flight finishes after ctx ends, so that its machine is recorded.
	made, err := l.Machines.Make(context.WithoutCancel(ctx), id, c.PoolID, size, data)
	madeAt := l.Clock.Now()
	if err != nil {
		kind := kindOf(err)
		reason, wait, level := makeFailure(kind)
		l.Log.Log(ctx, level, "making a machine failed", slog.String("dispatch_id", id), slog.String("kind", string(kind)), errorAttr(err, data))
		l.release(ctx, id, reason)
		l.remove(id)
		l.wait(ctx, wait)
		return nil
	}
	e := journal.Entry{DispatchID: id, Phase: journal.PhaseMade, MachineID: made.ID, MaxLifetimeMinutes: c.MaxLifetimeMinutes}
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

// dispatched sends dispatched for the entry e, which is made or dispatched. On 204, a
// made entry becomes dispatched. On 410 gone or 409 lease_expired, the machine is
// cleaned up. Any other failure is logged, and the next watch sends dispatched again.
func (l *loop) dispatched(ctx context.Context, e journal.Entry) {
	attrs := []any{slog.String("dispatch_id", e.DispatchID), slog.String("machine_id", e.MachineID)}
	err := l.Apiary.Dispatched(ctx, e.DispatchID, e.MachineID)
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
		l.cleanUp(ctx, e.DispatchID, e.MachineID, true)
	case errors.Is(err, ErrLeaseExpired):
		l.Log.Warn("the lease of the dispatch expired before dispatched", attrs...)
		l.cleanUp(ctx, e.DispatchID, e.MachineID, true)
	default:
		l.Log.Warn("sending dispatched failed", append(attrs, errorAttr(err, nil))...)
	}
}
