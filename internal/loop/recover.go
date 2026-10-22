package loop

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/qoryai/hive/internal/journal"
)

// recoverJournal runs once before the claim path and the watch. It lists the pool's
// machines once and acts on each journal entry as [journal.Recover] returns:
//   - Release: the dispatch is released and its entry removed.
//   - Drop: the entry is removed.
//   - Resume: a claimed entry becomes made with the machine id found, and the
//     lifetime [journal.MaxLifetime], since a claimed entry has no lifetime. Each made
//     entry then gets dispatched. A dispatched entry stays for the watch.
//
// It returns an error when the list fails or a resumed entry cannot be written.
func (l *loop) recoverJournal(ctx context.Context) error {
	list, err := l.Machines.List(ctx, l.PoolID)
	if err != nil {
		return fmt.Errorf("Check the machine adapter, then start the provisioner again: listing the pool's machines to recover the journal of live dispatches failed: %s", shown(err, nil))
	}
	byKey := machinesByKey(list.Machines)
	for _, e := range l.Journal.List() {
		m, found := byKey[e.DispatchID]
		action := journal.Recover(e, journal.Lookup{Found: found, MachineID: m.ID, State: string(m.State)})
		attrs := []any{slog.String("dispatch_id", e.DispatchID)}
		switch action.Kind {
		case journal.Release:
			l.release(ctx, e.DispatchID, action.Reason)
			l.remove(e.DispatchID)
		case journal.Drop:
			l.Log.Info("dropped a dispatch whose machine is deleting or not listed", attrs...)
			l.remove(e.DispatchID)
		case journal.Resume:
			next := e
			next.MachineID = action.MachineID
			if e.Phase == journal.PhaseClaimed {
				next.Phase, next.MaxLifetimeMinutes = journal.PhaseMade, journal.MaxLifetime
			}
			if next != e {
				if err := l.Journal.Set(next); err != nil {
					return err
				}
			}
			l.Log.Info("resumed a dispatch", append(attrs, slog.String("machine_id", next.MachineID), slog.String("state", string(m.State)))...)
			if next.Phase == journal.PhaseMade {
				l.dispatched(ctx, next)
			}
		}
	}
	return nil
}
