package loop

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/integrations/contracts"
)

// machineEndStopped is the record of a dispatch that gets no more machine-end. It is
// not a state of the contract.
const machineEndStopped = "stopped"

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
//   - A deleting entry is cleaned up again, until the adapter's delete returns gone.
//     It gets no dispatched, and its lifetime does not count.
//   - An ended entry gets gone again with one try, until the post is done or
//     [GoneLimit] has passed. After the first failure, the watch sends gone for no
//     other ended entry, so a Qory Apiary with no machine-end route costs one post per
//     watch and delays no dispatched. An ended entry gets no delete and no dispatched.
//   - A claimed entry stays after a release that returned 409 machine_exists, or
//     after a release when Journal.Remove failed before the entry left the journal.
//     When the list has a machine with its key, the entry adopts the machine: it
//     becomes made with the machine's id, and the rules of a made entry apply. When
//     the machine is deleting, the entry is cleaned up with the machine's id. The
//     entry is removed when no machine is listed once the entry's lifetime and 15
//     minutes have passed since the claim, or since the start of Run, for an entry
//     that the journal had at the start.
//   - A made or dispatched entry whose machine is not listed is cleaned up only after
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
	sendGone := true
	for _, e := range before {
		if ctx.Err() != nil {
			return
		}
		if l.isBusy(e.DispatchID) || !l.inJournal(e) {
			continue
		}
		switch e.Phase {
		case journal.PhaseDeleting:
			l.cleanUp(ctx, e)
			continue
		case journal.PhaseEnded:
			sendGone = l.watchEnded(ctx, e, sendGone)
			continue
		}
		m, found := byKey[e.DispatchID]
		if e.Phase == journal.PhaseClaimed {
			var ok bool
			if e, ok = l.adopt(ctx, e, m, found); !ok {
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
		// A second machine with this key is listed as a stray once the entry has left.
		if l.cleanUpStray(ctx, m) {
			known[m.Key] = true
		}
	}
}

// adopt acts on the claimed entry e, given the lookup of its machine m. It returns
// the entry made with the machine's id and true when the machine is listed and not
// deleting. Otherwise it cleans up the machine, removes the entry or leaves it, and
// returns false.
func (l *loop) adopt(ctx context.Context, e journal.Entry, m contracts.Machine, found bool) (journal.Entry, bool) {
	attrs := []any{slog.String("dispatch_id", e.DispatchID)}
	if found {
		attrs = append(attrs, slog.String("machine_id", m.ID), slog.String("state", m.State))
	}
	lifetime := time.Duration(e.MaxLifetimeMinutes)*time.Minute + LifetimeGrace
	switch {
	case found && m.State == contracts.MachineDeleting:
		l.Log.Info("the machine is deleting", attrs...)
		e.MachineID = m.ID
		l.cleanUpLogged(ctx, e)
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
		l.cleanUpLogged(ctx, e)
	case !found && l.Clock.Now().Sub(l.madeAt(e.DispatchID)) > ListGrace:
		l.Log.Info("the machine is still not listed 5 minutes after its make", attrs...)
		l.cleanUpLogged(ctx, e)
	case !found:
		// Inside the list grace, the machine may exist.
		l.dispatched(ctx, e)
	case m.State == contracts.MachineStopped:
		l.Log.Info("the machine stopped", attrs...)
		l.cleanUp(ctx, e)
	case l.Clock.Now().After(adapterio.Started(m).Add(time.Duration(e.MaxLifetimeMinutes)*time.Minute + LifetimeGrace)):
		l.Log.Warn("the machine ran longer than its lifetime and 15 minutes", attrs...)
		l.cleanUp(ctx, e)
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

// cleanUp reads the log of the entry's machine, uploads it, deletes the machine and
// sends machine-end. Each step runs after a failure of the step before it. When the
// delete returns gone, cleanUp sends gone and removes the entry. When that post must be
// retried, the entry moves to the phase ended. When the delete returns deleting or
// fails, the entry moves to the phase deleting, cleanUp sends that state, and the next
// watch calls cleanUp again. After a failed write of the journal, the entry stays as it
// is and the next watch acts on it again.
func (l *loop) cleanUp(ctx context.Context, e journal.Entry) {
	attrs := []any{slog.String("dispatch_id", e.DispatchID), slog.String("machine_id", e.MachineID)}
	state, err := l.deleteMachine(ctx, e.DispatchID, e.MachineID, attrs)
	end := contracts.MachineEndDeleting
	switch {
	case err != nil:
		end = contracts.MachineEndDeleteFailed
	case state == contracts.MachineGone:
		if !l.sendMachineEnd(ctx, l.Apiary.MachineEnd, e.DispatchID, contracts.MachineEndGone, attrs) || !l.removeEnded(e) {
			l.setEnded(e, attrs)
		}
		return
	}
	if l.setDeleting(e, attrs) {
		l.sendMachineEnd(ctx, l.Apiary.MachineEnd, e.DispatchID, end, attrs)
	}
}

// watchEnded acts on the ended entry e. When send is true, it sends gone with one try
// and removes the entry once the post is done. An entry that stays and has reached
// [GoneLimit] is removed with no gone done. It returns false after a failed post, and
// send otherwise.
func (l *loop) watchEnded(ctx context.Context, e journal.Entry, send bool) bool {
	attrs := []any{slog.String("dispatch_id", e.DispatchID), slog.String("machine_id", e.MachineID)}
	if send {
		send = l.sendMachineEnd(ctx, l.Apiary.MachineEndOnce, e.DispatchID, contracts.MachineEndGone, attrs)
		if send && l.removeEnded(e) {
			return true
		}
	}
	if l.Clock.Now().Sub(l.goneAt(e.DispatchID)) >= GoneLimit {
		l.Log.Error("stopped sending machine-end gone after 24 hours, so Qory Apiary shows no removal of this machine", attrs...)
		l.removeEnded(e)
	}
	return send
}

// removeEnded removes the entry e, whose cleanup is over, and returns whether it left
// the journal.
func (l *loop) removeEnded(e journal.Entry) bool {
	if !l.remove(e.DispatchID) {
		return false
	}
	l.setLogged(e.MachineID, false)
	return true
}

// cleanUpLogged is [loop.cleanUp] with no read of the log, for a machine that an entry
// reaches with no log read: it is deleting, or it is not listed.
func (l *loop) cleanUpLogged(ctx context.Context, e journal.Entry) {
	l.setLogged(e.MachineID, true)
	l.cleanUp(ctx, e)
}

// cleanUpStray cleans up the listed machine m, whose key has no journal entry, and
// returns whether the journal has an entry for it. A key in the shape of a dispatch id
// gets a deleting entry, and [loop.cleanUp] does the rest. Any other key, or a failed
// write of the journal, leaves one delete with no entry and no machine-end, and the
// next watch finds the machine again if it is still listed and not deleting.
func (l *loop) cleanUpStray(ctx context.Context, m contracts.Machine) bool {
	attrs := []any{slog.String("dispatch_id", m.Key), slog.String("machine_id", m.ID)}
	if journal.IsDispatchID(m.Key) {
		e := journal.Entry{DispatchID: m.Key, Phase: journal.PhaseDeleting, MachineID: m.ID, MaxLifetimeMinutes: journal.MinLifetime}
		err := l.Journal.Set(e)
		if err == nil {
			l.cleanUp(ctx, e)
			return true
		}
		l.Log.Error("writing the journal failed", append(attrs, errorAttr(err, nil))...)
	}
	if _, err := l.deleteMachine(ctx, m.Key, m.ID, attrs); err == nil {
		l.setLogged(m.ID, false)
	}
	return false
}

// deleteMachine reads and uploads the log of the machine, unless it counts as logged,
// and deletes the machine. It returns the state of the delete's answer. After an
// accepted delete, the machine counts as logged.
func (l *loop) deleteMachine(ctx context.Context, dispatchID, machineID string, attrs []any) (string, error) {
	if !l.wasLogged(machineID) {
		l.uploadLog(ctx, dispatchID, machineID, attrs)
	}
	deleted, err := l.Machines.Delete(ctx, machineID)
	if err != nil {
		l.Log.Warn("deleting a machine failed", append(attrs, errorAttr(err, nil))...)
		return "", err
	}
	l.Log.Info("deleted a machine", append(attrs, slog.String("state", string(deleted.State)))...)
	l.setLogged(machineID, true)
	return deleted.State, nil
}

// setDeleting writes the entry e in the phase deleting, and returns false when the
// write fails. An entry in that phase writes nothing.
func (l *loop) setDeleting(e journal.Entry, attrs []any) bool {
	if e.Phase == journal.PhaseDeleting {
		return true
	}
	e.Phase = journal.PhaseDeleting
	if err := l.Journal.Set(e); err != nil {
		l.Log.Error("writing the journal failed", append(attrs, errorAttr(err, nil))...)
		return false
	}
	return true
}

// setEnded writes the entry e in the phase ended, which frees its place in
// MaxMachines, and starts its [GoneLimit]. A failed write is logged, and the entry stays
// as it is.
func (l *loop) setEnded(e journal.Entry, attrs []any) {
	e.Phase = journal.PhaseEnded
	if err := l.Journal.Set(e); err != nil {
		l.Log.Error("writing the journal failed", append(attrs, errorAttr(err, nil))...)
		return
	}
	l.setTime(l.gone, e.DispatchID)
	l.free()
}

// sendMachineEnd posts state for the dispatch with post and returns whether the post is
// done. It sends gone every time, and any other state only when it differs from the
// last one done for the dispatch. A dispatch whose posts stopped gets none. A post is
// done on 204, [ErrMachineEndDone] and [ErrMachineEndStopped]. After any other failure,
// the next watch sends the state again.
func (l *loop) sendMachineEnd(ctx context.Context, post func(context.Context, string, string) error, dispatchID, state string, attrs []any) bool {
	last := l.lastMachineEnd(dispatchID)
	if last == machineEndStopped || (state != contracts.MachineEndGone && last == state) {
		return true
	}
	attrs = append(attrs, slog.String("state", state))
	err := post(ctx, dispatchID, state)
	record := state
	switch {
	case err == nil:
		l.Log.Info("sent machine-end", attrs...)
	case errors.Is(err, ErrMachineEndStopped):
		l.Log.Warn("sending machine-end failed, and the dispatch gets no more machine-end", append(attrs, errorAttr(err, nil))...)
		record = machineEndStopped
	case errors.Is(err, ErrMachineEndDone):
		l.Log.Warn("sending machine-end failed, and a later try cannot change the answer", append(attrs, errorAttr(err, nil))...)
	default:
		l.Log.Warn("sending machine-end failed", append(attrs, errorAttr(err, nil))...)
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sentEnd[dispatchID] = record
	return true
}

// lastMachineEnd is the record of the dispatch in sentEnd, or "".
func (l *loop) lastMachineEnd(dispatchID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sentEnd[dispatchID]
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

// wasLogged is whether the machine id gets no more log reads.
func (l *loop) wasLogged(machineID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logged[machineID]
}
