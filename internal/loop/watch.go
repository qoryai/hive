package loop

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
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
//   - An ended entry gets gone again, until the post is done, with the waits of
//     [loop.goneFailed] between failed posts. It gets no delete and no dispatched.
//   - A claimed entry stays after a release that did not return 204, or after a
//     release when Journal.Remove failed before the entry left the journal.
//     When the list has a machine with its key, the entry adopts the machine: it
//     becomes made with the machine's id, and the rules of a made entry apply. When
//     the machine is deleting, the entry is cleaned up with the machine's id. When no
//     machine is listed once the entry's lifetime and 15 minutes have passed since the
//     claim, or since the start of Run for an entry that the journal had at the start,
//     the entry gets gone with no delete, as [loop.sendGone] sends it.
//   - A made or dispatched entry whose machine is not listed is cleaned up only after
//     [ListGrace]. Inside the grace, it gets dispatched as usual.
//
// Then the watch acts on each pending stray, as [loop.watchPending] does. Last, it acts
// on each listed machine whose key had no journal entry, neither before the list nor
// after it: a make that starts during the list has a claimed entry after it. The
// watch takes such a machine back for its dispatch when [loop.takeBack] does, and
// cleans it up otherwise. A key gets at most one dispatched per watch. The
// watch skips a machine whose key is a pending stray, and one whose key had gone
// done in the last [ListGrace], because a vendor's list can show a machine for a while
// after its delete returned gone. It also skips a deleting machine whose key is not a
// dispatch id, because it has no dispatch to send machine-end for.
//
// Each machine-end is one try. After the first failure, the watch sends no more
// machine-end, and its deletes and dispatched go on. So a Qory Apiary with no
// machine-end route costs one failed post per watch.
func (l *loop) watchOnce(ctx context.Context) {
	l.setEndsPaused(false)
	before := l.Journal.List()
	list, err := l.Machines.List(ctx, l.PoolID)
	if err != nil {
		l.Log.Warn("listing the pool's machines failed", errorAttr(err, nil))
		return
	}
	// The no-record gone of a claimed entry counts from the time the list arrived.
	listedAt := l.Clock.Now()
	byKey := machinesByKey(list.Machines)
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
			l.watchEnded(ctx, e)
			continue
		}
		m, found := byKey[e.DispatchID]
		if e.Phase == journal.PhaseClaimed {
			var ok bool
			if e, ok = l.adopt(ctx, e, m, found, listedAt); !ok {
				continue
			}
		}
		l.watchEntry(ctx, e, m, found)
	}
	known := map[string]bool{}
	for _, e := range append(before, l.Journal.List()...) {
		known[e.DispatchID] = true
	}
	l.watchPending(ctx, known)
	l.pruneGoneDone()
	for _, m := range list.Machines {
		if ctx.Err() != nil {
			return
		}
		if known[m.Key] || l.isPending(m.Key) || l.goneIsRecent(m.Key) || m.State == contracts.MachineDeleting && !journal.IsDispatchID(m.Key) {
			continue
		}
		l.Log.Warn("found a machine with no journal entry", slog.String("dispatch_id", m.Key), slog.String("machine_id", m.ID), slog.String("state", m.State))
		// A second machine with this key is listed as a stray once the entry has left.
		if l.takeBack(ctx, m) || l.cleanUpStray(ctx, m) {
			known[m.Key] = true
		}
	}
}

// adopt acts on the claimed entry e, given the lookup of its machine m in the list that
// arrived at listedAt. It returns the entry made with the machine's id and true when
// the machine is listed and not deleting. Otherwise it cleans up the machine, sends
// gone for the entry or leaves it, and returns false. Gone with no machine needs a
// list that arrived more than the entry's lifetime and 15 minutes after the claim.
func (l *loop) adopt(ctx context.Context, e journal.Entry, m contracts.Machine, found bool, listedAt time.Time) (journal.Entry, bool) {
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
	case !found && listedAt.Sub(l.claimedAt(e.DispatchID)) > lifetime:
		// The machine powered off at the end of its lifetime, if it ever started, so
		// the dispatch ends with gone and no delete.
		l.Log.Info("no machine is listed for the claimed dispatch after its lifetime and 15 minutes", attrs...)
		l.sendGone(ctx, e, attrs)
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
// delete returns gone, cleanUp sends gone as [loop.sendGone] does. When the delete
// returns deleting or fails, the entry moves to the phase deleting, cleanUp sends that
// state, and the next watch calls cleanUp again. After a failed write of the journal,
// the entry stays as it is and the next watch acts on it again.
func (l *loop) cleanUp(ctx context.Context, e journal.Entry) {
	attrs := []any{slog.String("dispatch_id", e.DispatchID), slog.String("machine_id", e.MachineID)}
	state, err := l.deleteMachine(ctx, e.DispatchID, e.MachineID, attrs)
	end := contracts.MachineEndDeleting
	switch {
	case err != nil:
		end = contracts.MachineEndDeleteFailed
	case state == contracts.MachineGone:
		l.sendGone(ctx, e, attrs)
		return
	}
	if l.setDeleting(e, attrs) {
		l.sendMachineEnd(ctx, e.DispatchID, end, attrs)
	}
}

// sendGone sends gone for the entry e, whose machine is gone or was never listed, and
// removes the entry once the post is done. When the post fails or is paused, or the
// removal fails, the entry moves to the phase ended, and the watch sends gone again
// until the post is done. A failed post starts the waits of [loop.goneFailed].
func (l *loop) sendGone(ctx context.Context, e journal.Entry, attrs []any) {
	sent := !l.endsArePaused()
	if l.sendMachineEnd(ctx, e.DispatchID, contracts.MachineEndGone, attrs) {
		if l.removeEnded(e) {
			return
		}
		sent = false
	}
	if l.setEnded(e, attrs) && sent {
		l.goneFailed(e.DispatchID)
	}
}

// watchEnded acts on the ended entry e. It sends gone and removes the entry once the
// post is done. After a failed removal, the entry stays, and the next watch acts on it
// again. After a failed post, the entry stays, and the watch sends gone again once the
// wait of [loop.goneFailed] has passed. Gone has no limit, so the entry stays until
// the post is done.
func (l *loop) watchEnded(ctx context.Context, e journal.Entry) {
	attrs := []any{slog.String("dispatch_id", e.DispatchID)}
	if e.MachineID != "" {
		attrs = append(attrs, slog.String("machine_id", e.MachineID))
	}
	if l.Clock.Now().Before(l.nextGone(e.DispatchID)) {
		return
	}
	sent := !l.endsArePaused()
	switch {
	case l.sendMachineEnd(ctx, e.DispatchID, contracts.MachineEndGone, attrs):
		l.removeEnded(e)
	case sent:
		l.goneFailed(e.DispatchID)
	}
}

// goneFailed counts a failed post of gone for the dispatch id, and sets the time of its
// next post. The first wait is [WatchInterval], and each later failure doubles it, up
// to [GoneRetryMax]: 1, 2, 4, 8, 16 and 32 minutes, and then 1 hour. A post that the
// pause skipped is not counted, so it goes out at the next watch.
func (l *loop) goneFailed(id string) {
	now := l.Clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.goneRetry[id]
	r.failures++
	wait := WatchInterval
	for i := 1; i < r.failures && wait < GoneRetryMax; i++ {
		wait *= 2
	}
	r.next = now.Add(min(wait, GoneRetryMax))
	l.goneRetry[id] = r
}

// nextGone is the time before which the watch sends no gone for the dispatch id, or
// the zero time.
func (l *loop) nextGone(id string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.goneRetry[id].next
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

// takeBack sends dispatched with no machine type for the listed machine m, whose key
// has no journal entry, as the journal may have been lost while the machine runs a
// dispatch. It returns whether the machine stays. A machine that is deleting or
// stopped, or whose key is not a dispatch id, gets no dispatched and does not stay.
//   - On 204, the machine gets a dispatched entry with the lifetime
//     [journal.MaxLifetime], because the claim's lifetime is not known, and its make
//     counts from now. The watch then acts on it as on any dispatched entry.
//   - On 410 gone, 409 lease_expired or a signed 403 forbidden, the dispatch ended or
//     is not the pool's, and the machine does not stay.
//   - After any other failure, such as no answer, a server error or an answer with no
//     valid signature, the machine stays and the next watch sends dispatched again,
//     until the machine is older than [journal.MaxLifetime] by its start time in the
//     list. Then it does not stay.
//
// After a failed write of the journal, the machine stays, and the next watch sends
// dispatched again.
func (l *loop) takeBack(ctx context.Context, m contracts.Machine) bool {
	if !journal.IsDispatchID(m.Key) || m.State == contracts.MachineDeleting || m.State == contracts.MachineStopped {
		return false
	}
	attrs := []any{slog.String("dispatch_id", m.Key), slog.String("machine_id", m.ID)}
	err := l.Apiary.Dispatched(ctx, m.Key, m.ID, "")
	switch {
	case err == nil:
	case errors.Is(err, ErrGone), errors.Is(err, ErrLeaseExpired), errors.Is(err, ErrForbidden):
		l.Log.Info("the dispatch of the machine ended or is not the pool's", append(attrs, errorAttr(err, nil))...)
		return false
	case l.Clock.Now().Sub(adapterio.Started(m)) > time.Duration(journal.MaxLifetime)*time.Minute:
		l.Log.Warn("sending dispatched failed, and the machine is older than the longest lifetime", append(attrs, errorAttr(err, nil))...)
		return false
	default:
		l.Log.Warn("sending dispatched failed, and the machine stays until the next watch", append(attrs, errorAttr(err, nil))...)
		return true
	}
	e := journal.Entry{DispatchID: m.Key, Phase: journal.PhaseDispatched, MachineID: m.ID, MaxLifetimeMinutes: journal.MaxLifetime}
	if err := l.Journal.Set(e); err != nil {
		l.Log.Error("writing the journal failed", append(attrs, errorAttr(err, nil))...)
		return true
	}
	l.setTime(l.made, m.Key)
	l.Log.Info("took back the machine for its dispatch", attrs...)
	return true
}

// cleanUpStray cleans up the listed machine m, whose key has no journal entry, and
// returns whether the journal has an entry for it. A machine that is deleting gets no
// read of its log. A key in the shape of a dispatch id gets a deleting entry, and
// [loop.cleanUp] does the rest. After a failed write of the journal, such a key gets
// one delete with no entry, and then one try of machine-end: gone when the delete
// returns gone, deleting when it returns deleting, and delete_failed when it fails.
// Unless gone is done, the key becomes a pending stray, as [loop.watchPending]
// describes. Any other key has no dispatch, so it gets one delete and no machine-end,
// and the next watch finds the machine again if it is still listed and not deleting.
func (l *loop) cleanUpStray(ctx context.Context, m contracts.Machine) bool {
	attrs := []any{slog.String("dispatch_id", m.Key), slog.String("machine_id", m.ID)}
	if m.State == contracts.MachineDeleting {
		l.setLogged(m.ID, true)
	}
	dispatch := journal.IsDispatchID(m.Key)
	if dispatch {
		e := journal.Entry{DispatchID: m.Key, Phase: journal.PhaseDeleting, MachineID: m.ID, MaxLifetimeMinutes: journal.MinLifetime}
		err := l.Journal.Set(e)
		if err == nil {
			l.cleanUp(ctx, e)
			return true
		}
		l.Log.Error("writing the journal failed", append(attrs, errorAttr(err, nil))...)
	}
	state, err := l.deleteMachine(ctx, m.Key, m.ID, attrs)
	if err == nil {
		// The machine has no entry that would end its record. While its delete
		// fails, the record stays, so a later watch reads its log no more.
		l.setLogged(m.ID, false)
	}
	if !dispatch {
		return false
	}
	end := contracts.MachineEndDeleting
	switch {
	case err != nil:
		end = contracts.MachineEndDeleteFailed
	case state == contracts.MachineGone:
		end = contracts.MachineEndGone
	}
	sent := !l.endsArePaused()
	done := l.sendMachineEnd(ctx, m.Key, end, attrs)
	// The key has no entry, so the post leaves no record.
	l.forgetEnds(m.Key)
	switch {
	case end != contracts.MachineEndGone:
		l.setPending(m.Key, m.ID)
	case !done:
		l.setPending(m.Key, "")
		if sent {
			l.goneFailed(m.Key)
		}
	}
	return false
}

// watchPending acts on each pending stray: a stray with a dispatch key whose journal
// write failed, and whose gone is not done. A pending stray with a machine id gets a
// delete with no read of its log at each watch, also when the list no longer shows the
// machine, until the delete returns gone. Then it gets gone with the waits of
// [loop.goneFailed], until the post is done, which includes a signed 400, 403 or 404.
// A pending stray whose dispatch id is known, because the journal has an entry for it,
// is dropped: the entry does the rest. Once gone is done, [loop.goneIsRecent] makes the
// stray loop skip its machine, also in the same watch.
//
// Pending strays are kept in memory only, so a restart loses them. A machine that is
// still listed is then found again as a stray. Otherwise Qory Apiary frees the
// dispatch's slot by its time limit for a slot: the dispatch's max_lifetime_minutes and
// 60 minutes after the latest of the claim and the dispatch's machine events, as
// claim.schema.json of the provisioner contract states.
func (l *loop) watchPending(ctx context.Context, known map[string]bool) {
	for _, id := range l.pendingIDs() {
		if ctx.Err() != nil {
			return
		}
		if known[id] {
			l.dropPending(id)
			continue
		}
		if machineID := l.pendingMachine(id); machineID != "" {
			attrs := []any{slog.String("dispatch_id", id), slog.String("machine_id", machineID)}
			l.setLogged(machineID, true)
			state, err := l.deleteMachine(ctx, id, machineID, attrs)
			l.setLogged(machineID, false)
			if err != nil || state != contracts.MachineGone {
				continue
			}
			l.setPending(id, "")
		}
		if l.Clock.Now().Before(l.nextGone(id)) {
			continue
		}
		sent := !l.endsArePaused()
		switch {
		case l.sendMachineEnd(ctx, id, contracts.MachineEndGone, []any{slog.String("dispatch_id", id)}):
			l.dropPending(id)
		case sent:
			l.goneFailed(id)
		}
	}
}

// pendingIDs is the dispatch ids of the pending strays, in sorted order.
func (l *loop) pendingIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Sorted(maps.Keys(l.pending))
}

// setPending makes the dispatch id a pending stray with the machine id to delete, or
// with "" when only its gone is left.
func (l *loop) setPending(id, machineID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending[id] = machineID
}

// isPending is whether the dispatch id is a pending stray.
func (l *loop) isPending(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.pending[id]
	return ok
}

// pendingMachine is the machine id that the pending stray of the dispatch id still
// deletes, or "".
func (l *loop) pendingMachine(id string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pending[id]
}

// dropPending ends the pending stray of the dispatch id, with its waits and the record
// of its posts.
func (l *loop) dropPending(id string) {
	l.mu.Lock()
	delete(l.pending, id)
	delete(l.goneRetry, id)
	l.mu.Unlock()
	l.forgetEnds(id)
}

// goneIsRecent is whether gone was done for the dispatch id in the last [ListGrace].
func (l *loop) goneIsRecent(id string) bool {
	now := l.Clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	at, ok := l.goneDone[id]
	return ok && now.Sub(at) < ListGrace
}

// pruneGoneDone drops each time of goneDone that is [ListGrace] old or older.
func (l *loop) pruneGoneDone() {
	now := l.Clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	maps.DeleteFunc(l.goneDone, func(_ string, at time.Time) bool { return now.Sub(at) >= ListGrace })
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

// setEnded writes the entry e in the phase ended, and returns whether the entry counts
// as ended. A failed write is logged. When the entry is still as it was, it stays so.
// A write can fail after the journal changed, as when syncing the folder fails, and
// then the entry counts as ended.
func (l *loop) setEnded(e journal.Entry, attrs []any) bool {
	e.Phase = journal.PhaseEnded
	if err := l.Journal.Set(e); err != nil {
		l.Log.Error("writing the journal failed", append(attrs, errorAttr(err, nil))...)
		return l.inJournal(e)
	}
	return true
}

// sendMachineEnd sends state for the dispatch, with one try, and returns whether the
// post is done. It sends a state only when it differs from the last one done for the
// dispatch. A dispatch whose posts stopped gets none. A post that is not sent for those
// reasons ends the count of any failing state. A post is done on 204 and
// [ErrMachineEndStopped], and on [ErrMachineEndUnusable] for deleting and
// delete_failed. Gone is done only on 204 or [ErrMachineEndStopped], as the contract
// requires, so [ErrMachineEndUnusable] to gone is a failure. Any other failure pauses
// machine-end until the next watch starts. While they are paused, a post is not sent
// and counts as failed. The next watch sends the state again.
func (l *loop) sendMachineEnd(ctx context.Context, dispatchID, state string, attrs []any) bool {
	last := l.lastMachineEnd(dispatchID)
	if last == machineEndStopped || last == state {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.failedEnd, dispatchID)
		return true
	}
	attrs = append(attrs, slog.String("state", state))
	if l.endsArePaused() {
		l.machineEndFailed(dispatchID, state, attrs)
		return false
	}
	err := l.Apiary.MachineEnd(ctx, dispatchID, state)
	record := state
	switch {
	case err == nil:
		l.Log.Info("sent machine-end", attrs...)
	case errors.Is(err, ErrMachineEndStopped):
		l.Log.Warn("sending machine-end failed, and the dispatch gets no more machine-end", append(attrs, errorAttr(err, nil))...)
		record = machineEndStopped
	case errors.Is(err, ErrMachineEndUnusable) && state != contracts.MachineEndGone:
		l.Log.Warn("sending machine-end failed, and a later try cannot change the answer", append(attrs, errorAttr(err, nil))...)
	default:
		l.Log.Warn("sending machine-end failed", append(attrs, errorAttr(err, nil))...)
		l.setEndsPaused(true)
		l.machineEndFailed(dispatchID, state, attrs)
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sentEnd[dispatchID] = record
	delete(l.failedEnd, dispatchID)
	if state == contracts.MachineEndGone {
		l.goneDone[dispatchID] = l.Clock.Now()
	}
	return true
}

// machineEndFailed counts a post of state that failed or was not sent. A deleting or
// delete_failed that has failed for [MachineEndLimit] since its first failure is logged
// once and counts as done, so it is dropped and a later state goes out as usual. Gone
// is never dropped: [loop.watchEnded] sends it again until the post is done.
func (l *loop) machineEndFailed(dispatchID, state string, attrs []any) {
	if state == contracts.MachineEndGone {
		return
	}
	now := l.Clock.Now()
	l.mu.Lock()
	f, ok := l.failedEnd[dispatchID]
	if !ok || f.state != state {
		l.failedEnd[dispatchID] = endFailure{state, now}
		l.mu.Unlock()
		return
	}
	if now.Sub(f.at) < MachineEndLimit {
		l.mu.Unlock()
		return
	}
	delete(l.failedEnd, dispatchID)
	l.sentEnd[dispatchID] = state
	l.mu.Unlock()
	l.Log.Error("stopped sending this machine-end state after 24 hours", attrs...)
}

// setEndsPaused sets whether machine-end is paused until the next watch starts.
func (l *loop) setEndsPaused(paused bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.endsPaused = paused
}

// endsArePaused is whether machine-end is paused.
func (l *loop) endsArePaused() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.endsPaused
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
