package loop

import (
	"context"
	"log/slog"
	"time"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/integrations/contracts"
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

// watchOnce lists the pool's machines once and acts on each entry that was in the
// journal before the list, except the dispatches of the claim path:
//   - A claimed entry stays after a release that returned 409 machine_exists, or
//     after a release when Journal.Remove failed before the entry left the journal.
//     When the list has a machine with its key, the entry adopts the machine: it
//     becomes made with the machine's id, and the rules of a made entry apply. The
//     entry is removed when the machine is deleting, or when no machine is listed
//     once the entry's lifetime and 15 minutes have passed since the claim, or since
//     the start of Run, for an entry that the journal had at the start.
//   - A made or dispatched entry whose machine is not listed is removed only after
//     [ListGrace]. Inside the grace, it gets dispatched as usual.
//
// Then the watch cleans up each listed machine whose key had no journal entry, neither
// before the list nor after it: a make that starts during the list has a claimed
// entry after it.
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
		if l.isBusy(e.DispatchID) || !l.inJournal(e) {
			continue
		}
		m, found := byKey[e.DispatchID]
		if e.Phase == journal.PhaseClaimed {
			var ok bool
			if e, ok = l.adopt(e, m, found); !ok {
				continue
			}
		}
		l.watchEntry(ctx, e, m, found)
	}
	known := map[string]bool{}
	for _, e := range append(before, l.Journal.List()...) {
		known[e.DispatchID] = true
	}
	for _, m := range list.Machines {
		if ctx.Err() != nil {
			return
		}
		if known[m.Key] || m.State == contracts.MachineDeleting {
			continue
		}
		l.Log.Warn("found a machine with no journal entry", slog.String("dispatch_id", m.Key), slog.String("machine_id", m.ID), slog.String("state", m.State))
		l.cleanUp(ctx, m.Key, m.ID, false)
	}
}

// adopt acts on the claimed entry e, given the lookup of its machine m. It returns
// the entry made with the machine's id and true when the machine is listed and not
// deleting. Otherwise it removes the entry or leaves it, and returns false.
func (l *loop) adopt(e journal.Entry, m contracts.Machine, found bool) (journal.Entry, bool) {
	attrs := []any{slog.String("dispatch_id", e.DispatchID)}
	if found {
		attrs = append(attrs, slog.String("machine_id", m.ID), slog.String("state", m.State))
	}
	lifetime := time.Duration(e.MaxLifetimeMinutes)*time.Minute + LifetimeGrace
	switch {
	case found && m.State == contracts.MachineDeleting:
		l.Log.Info("the machine is deleting", attrs...)
		l.remove(e.DispatchID)
		return e, false
	case !found && l.Clock.Now().Sub(l.claimedAt(e.DispatchID)) > lifetime:
		l.Log.Info("no machine is listed for the claimed dispatch after its lifetime and 15 minutes", attrs...)
		l.remove(e.DispatchID)
		return e, false
	case !found:
		return e, false
	}
	e.Phase, e.MachineID = journal.PhaseMade, m.ID
	if err := l.Journal.Set(e); err != nil {
		l.Log.Error("writing the journal failed", append(attrs, errorAttr(err, nil))...)
		return e, false
	}
	l.setTime(l.made, e.DispatchID)
	l.Log.Info("adopted the machine of a claimed dispatch", attrs...)
	return e, true
}

// watchEntry acts on the made or dispatched entry e, given the lookup of its machine
// m.
func (l *loop) watchEntry(ctx context.Context, e journal.Entry, m contracts.Machine, found bool) {
	attrs := []any{slog.String("dispatch_id", e.DispatchID), slog.String("machine_id", e.MachineID)}
	if found {
		attrs = append(attrs, slog.String("state", m.State))
	}
	switch {
	case found && m.State == contracts.MachineDeleting:
		l.Log.Info("the machine is deleting", attrs...)
		l.remove(e.DispatchID)
	case !found && l.Clock.Now().Sub(l.madeAt(e.DispatchID)) > ListGrace:
		l.Log.Info("the machine is still not listed 5 minutes after its make", attrs...)
		l.remove(e.DispatchID)
	case !found:
		// Inside the list grace, the machine may exist.
		l.dispatched(ctx, e)
	case m.State == contracts.MachineStopped:
		l.Log.Info("the machine stopped", attrs...)
		l.cleanUp(ctx, e.DispatchID, e.MachineID, true)
	case l.Clock.Now().After(adapterio.Started(m).Add(time.Duration(e.MaxLifetimeMinutes)*time.Minute + LifetimeGrace)):
		l.Log.Warn("the machine ran longer than its lifetime and 15 minutes", attrs...)
		l.cleanUp(ctx, e.DispatchID, e.MachineID, true)
	default:
		l.dispatched(ctx, e)
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
func machinesByKey(list []contracts.Machine) map[string]contracts.Machine {
	byKey := make(map[string]contracts.Machine, len(list))
	for _, m := range list {
		if old, ok := byKey[m.Key]; !ok || old.State == contracts.MachineDeleting {
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
	l.setLogged(machineID, false)
	if entry {
		l.remove(dispatchID)
	}
}

// uploadLog reads the log of the machine and uploads it for the dispatch. A log at the
// adapter's bound of [adapterio.MaxLog] bytes is uploaded as truncated. Once the read
// succeeds, the machine counts as logged, also when the upload fails.
func (l *loop) uploadLog(ctx context.Context, dispatchID, machineID string, attrs []any) {
	got, err := l.Machines.Log(ctx, machineID)
	if err != nil {
		l.Log.Warn("reading a machine's log failed", append(attrs, errorAttr(err, nil))...)
		return
	}
	l.setLogged(machineID, true)
	err = l.Apiary.MachineLog(ctx, dispatchID, got.Log, len(got.Log) >= adapterio.MaxLog)
	if err != nil {
		l.Log.Warn("uploading a machine's log failed", append(attrs, errorAttr(err, nil))...)
		return
	}
	l.Log.Info("uploaded a machine's log", attrs...)
}

// setLogged marks the machine id as logged, or no longer.
func (l *loop) setLogged(machineID string, logged bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if logged {
		l.logged[machineID] = true
	} else {
		delete(l.logged, machineID)
	}
}

// wasLogged is whether the log of the machine id is uploaded in a cleanup whose delete
// has not succeeded yet.
func (l *loop) wasLogged(machineID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logged[machineID]
}
