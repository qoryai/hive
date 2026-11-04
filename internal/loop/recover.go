package loop

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/qoryai/hive/internal/journal"
)

// recoverJournal runs once before the claim path and the watch. It lists the pool's
// machines once and acts on each journal entry as [journal.Recover] returns:
//   - Release: the dispatch is released and its entry removed. When the release
//     returns 409 machine_exists, the entry stays claimed for the watch.
//   - Deleting: the entry moves to the phase deleting with the machine id found, and
//     the machine counts as logged. When the write fails, the entry stays as it is
//     for the watch.
//   - Resume: a claimed entry becomes made with the machine id found. Each made
//     entry then gets dispatched. A dispatched, deleting or ended entry stays for the
//     watch.
//     An entry whose machine is not listed stays too, and the watch cleans it up
//     once [ListGrace] has passed since the start.
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
			l.releaseEntry(ctx, e.DispatchID, action.Reason)
		case journal.Deleting:
			next := e
			next.Phase, next.MachineID = journal.PhaseDeleting, action.MachineID
			attrs = append(attrs, slog.String("machine_id", next.MachineID))
			l.setLogged(next.MachineID, true)
			if err := l.Journal.Set(next); err != nil {
				l.Log.Error("writing the journal failed", append(attrs, errorAttr(err, nil))...)
				continue
			}
			l.Log.Info("the machine of a recovered dispatch is deleting", attrs...)
		case journal.Resume:
			next := e
			next.MachineID = action.MachineID
			if e.Phase == journal.PhaseClaimed {
				next.Phase = journal.PhaseMade
			}
			if next != e {
				if err := l.Journal.Set(next); err != nil {
					return err
				}
			}
			attrs = append(attrs, slog.String("machine_id", next.MachineID))
			if found {
				attrs = append(attrs, slog.String("state", string(m.State)))
			}
			l.Log.Info("resumed a dispatch", attrs...)
			if next.Phase == journal.PhaseMade {
				l.dispatched(ctx, next)
			}
		}
	}
	return nil
}
