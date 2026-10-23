package journal

// stateDeleting is the adapter state of a machine that is being deleted. A restart
// drops the entry of such a machine.
const stateDeleting = "deleting"

// ReasonRestarted is the reason of the release that [Recover] returns. It is 1 to 200
// characters, as the provisioner contract requires.
const ReasonRestarted = "the provisioner restarted before a machine was made"

// Lookup is the result of the lookup of a dispatch's machine by its key, the dispatch
// id, in the adapter's list of machines. Found is false when no machine in the list
// has that key. MachineID and State are the machine's id and its adapter state:
// starting, running, stopped or deleting.
type Lookup struct {
	Found            bool
	MachineID, State string
}

// Kind is what to do with a journal entry after a restart.
type Kind int

const (
	// Release releases the dispatch with Action.Reason. A release has no retry.
	Release Kind = iota + 1
	// Resume continues the dispatch with the machine Action.MachineID.
	Resume
	// Drop removes the entry from the journal, with no release.
	Drop
)

// Action is what to do with one journal entry after a restart. Reason is set for
// [Release]. MachineID is set for [Resume].
type Action struct {
	Kind      Kind
	Reason    string
	MachineID string
}

// Recover returns what to do with the entry e after a restart, given the lookup l of
// its machine by its key:
//   - claimed, with no machine found: release with [ReasonRestarted]. A release
//     exists only before a machine exists.
//   - made or dispatched, with no machine found: resume with the entry's machine id.
//     A vendor's list can lag behind its make, so a machine missing from the list may
//     exist.
//   - any phase, with a machine found that is deleting: drop.
//   - any phase, with a machine found in another state: resume with the machine id
//     found. For claimed, the make succeeded before the restart.
func Recover(e Entry, l Lookup) Action {
	switch {
	case !l.Found && e.Phase == PhaseClaimed:
		return Action{Kind: Release, Reason: ReasonRestarted}
	case !l.Found:
		return Action{Kind: Resume, MachineID: e.MachineID}
	case l.State == stateDeleting:
		return Action{Kind: Drop}
	default:
		return Action{Kind: Resume, MachineID: l.MachineID}
	}
}
