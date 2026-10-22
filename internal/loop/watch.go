package loop

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/machines"
)

// watch runs one watch every [WatchInterval] until ctx ends.
func (l *loop) watch(ctx context.Context) {
	for {
		l.wait(ctx, WatchInterval)
		if ctx.Err() != nil {
			return
		}
		l.watchOnce(ctx)
	}
}

// watchOnce lists the pool's machines once and acts on each made or dispatched entry
// that was in the journal before the list. It skips the claimed entries and the
// dispatches of the claim path. Then it cleans up each listed machine whose key had no
// journal entry, neither before the list nor after it: a make that starts during the
// list has a claimed entry after it.
func (l *loop) watchOnce(ctx context.Context) {
	before := l.Journal.List()
	list, err := l.Machines.List(ctx, l.PoolID)
	if err != nil {
		l.Log.Warn("listing the pool's machines failed", errorAttr(err, nil))
		return
	}
	byKey := machinesByKey(list.Machines)
	for _, e := range before {
		if ctx.Err() != nil {
			return
		}
		if e.Phase == journal.PhaseClaimed || l.isBusy(e.DispatchID) || !l.inJournal(e) {
			continue
		}
		attrs := []any{slog.String("dispatch_id", e.DispatchID), slog.String("machine_id", e.MachineID)}
		m, found := byKey[e.DispatchID]
		if found {
			attrs = append(attrs, slog.String("state", string(m.State)))
		}
		switch {
		case !found || m.State == machines.StateDeleting:
			l.Log.Info("the machine is deleting or not listed", attrs...)
			l.remove(e.DispatchID)
		case m.State == machines.StateStopped:
			l.Log.Info("the machine stopped", attrs...)
			l.cleanUp(ctx, e.DispatchID, e.MachineID, true)
		case l.Clock.Now().After(m.Started.Add(time.Duration(e.MaxLifetimeMinutes)*time.Minute + LifetimeGrace)):
			l.Log.Warn("the machine ran longer than its lifetime and 15 minutes", attrs...)
			l.cleanUp(ctx, e.DispatchID, e.MachineID, true)
		default:
			l.dispatched(ctx, e)
		}
	}
	known := map[string]bool{}
	for _, e := range append(before, l.Journal.List()...) {
		known[e.DispatchID] = true
	}
	for _, m := range list.Machines {
		if ctx.Err() != nil {
			return
		}
		if known[m.Key] || m.State == machines.StateDeleting {
			continue
		}
		l.Log.Warn("found a machine with no journal entry", slog.String("dispatch_id", m.Key), slog.String("machine_id", m.ID), slog.String("state", string(m.State)))
		l.cleanUp(ctx, m.Key, m.ID, false)
	}
}

// inJournal is whether the journal has the entry e, unchanged.
func (l *loop) inJournal(e journal.Entry) bool {
	for _, x := range l.Journal.List() {
		if x == e {
			return true
		}
	}
	return false
}

// machinesByKey maps each key to its machine. When two machines have one key, a
// machine that is not deleting comes first.
func machinesByKey(list []machines.Machine) map[string]machines.Machine {
	byKey := make(map[string]machines.Machine, len(list))
	for _, m := range list {
		if old, ok := byKey[m.Key]; !ok || old.State == machines.StateDeleting {
			byKey[m.Key] = m
		}
	}
	return byKey
}

// cleanUp reads the log of the machine id through the adapter, uploads it for the
// dispatch, deletes the machine and, when entry is true, removes the dispatch's
// journal entry. Each step runs after a failure of the step before it. After a failed
// delete, the entry stays, so the next watch tries the delete again, and the log is
// not uploaded again.
func (l *loop) cleanUp(ctx context.Context, dispatchID, machineID string, entry bool) {
	attrs := []any{slog.String("dispatch_id", dispatchID), slog.String("machine_id", machineID)}
	if !l.wasLogged(machineID) {
		l.uploadLog(ctx, dispatchID, machineID, attrs)
	}
	deleted, err := l.Machines.Delete(ctx, machineID)
	if err != nil {
		l.Log.Warn("deleting a machine failed", append(attrs, errorAttr(err, nil))...)
		return
	}
	l.Log.Info("deleted a machine", append(attrs, slog.String("state", string(deleted.State)))...)
	l.mu.Lock()
	delete(l.logged, machineID)
	l.mu.Unlock()
	if entry {
		l.remove(dispatchID)
	}
}

// uploadLog reads the log of the machine and uploads it for the dispatch. A log at the
// adapter's bound of [machines.MaxLog] bytes is uploaded as truncated. Once the read
// succeeds, the machine counts as logged, also when the upload fails.
func (l *loop) uploadLog(ctx context.Context, dispatchID, machineID string, attrs []any) {
	got, err := l.Machines.Log(ctx, machineID)
	if err != nil {
		l.Log.Warn("reading a machine's log failed", append(attrs, errorAttr(err, nil))...)
		return
	}
	l.mu.Lock()
	l.logged[machineID] = true
	l.mu.Unlock()
	err = l.Apiary.MachineLog(ctx, dispatchID, got.Log, len(got.Log) >= machines.MaxLog)
	switch {
	case err == nil:
		l.Log.Info("uploaded a machine's log", attrs...)
	case errors.Is(err, ErrGone):
	default:
		l.Log.Warn("uploading a machine's log failed", append(attrs, errorAttr(err, nil))...)
	}
}

// wasLogged is whether the log of the machine id is uploaded in a cleanup whose delete
// has not succeeded yet.
func (l *loop) wasLogged(machineID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logged[machineID]
}
