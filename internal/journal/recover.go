package journal

// The states of an EC2 instance that end it. A machine in either state cannot stop
// again, so a restart finishes its dispatch.
const (
	// StateShuttingDown is an instance that is being terminated.
	StateShuttingDown = "shutting-down"
	// StateTerminated is a terminated instance.
	StateTerminated = "terminated"
)

// The reasons of the releases that [Recover] returns. Each is 1 to 200 characters, as
// the provisioner contract requires.
const (
	// ReasonNoMachine is a dispatch released with retry true: the provisioner
	// restarted, and its run has no machine.
	ReasonNoMachine = "the provisioner restarted, and the run has no machine"
	// ReasonTerminated is a dispatch released with retry false: the provisioner
	// restarted, and its run's machine was terminated.
	ReasonTerminated = "the provisioner restarted, and the run's machine was terminated"
)

// Lookup is the result of the lookup of a dispatch's machine by its client token, the
// dispatch id. Found is false when no instance has that client token. InstanceID and
// State are the instance's id and its state name, such as running or stopped.
type Lookup struct {
	Found             bool
	InstanceID, State string
}

// Kind is what to do with a journal entry after a restart.
type Kind int

const (
	// Release releases the run with retry true and Action.Reason.
	Release Kind = iota + 1
	// Resume resumes the watch of the machine Action.InstanceID.
	Resume
	// Finish releases the run with retry false and Action.Reason.
	Finish
)

// Action is what to do with one journal entry after a restart. Retry is the release's
// retry: true for [Release] and false for [Finish]. Reason is set for both releases.
// InstanceID is set for [Resume].
type Action struct {
	Kind       Kind
	Retry      bool
	Reason     string
	InstanceID string
}

// Recover returns what to do with the entry e after a restart, given the lookup l of
// its machine by client token:
//   - before handed_over: release with retry true. No machine was created, and the run
//     key or run code, which the journal never contains, is lost, so the run starts
//     again with a new one. l is not used.
//   - from handed_over on, with no machine found: release with retry true.
//   - from handed_over on, with a machine found that is shutting down or terminated:
//     finish, which releases with retry false and [ReasonTerminated].
//   - from handed_over on, with any other machine found: resume the watch of the
//     machine found.
func Recover(e Entry, l Lookup) Action {
	release := Action{Kind: Release, Retry: true, Reason: ReasonNoMachine}
	switch {
	case e.Phase == PhaseClaimed || e.Phase == PhaseDispatched || e.Phase == PhaseHandover:
		return release
	case !l.Found:
		return release
	case l.State == StateShuttingDown || l.State == StateTerminated:
		return Action{Kind: Finish, Retry: false, Reason: ReasonTerminated}
	default:
		return Action{Kind: Resume, InstanceID: l.InstanceID}
	}
}
