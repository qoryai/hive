package loop_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qoryai/hive/internal/adapter"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/internal/loop"
	"github.com/qoryai/integrations/contracts"
)

// equal fails the test when got is not want.
func equal[T comparable](t *testing.T, what string, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %+v\nwant %+v", what, got, want)
	}
}

// TestAClaimMakesAMachineAndSendsDispatched claims a dispatch, makes its machine with
// the start data, sends dispatched and records each phase. A claim with no machine
// type makes the size "standard".
func TestAClaimMakesAMachineAndSendsDispatched(t *testing.T) {
	for _, c := range []struct {
		name, machine, size, address, written string
	}{
		{"no machine type", "", loop.DefaultSize, "10.0.1.5", "10.0.1.5"},
		{"a machine type", "large", "large", "10.0.1.5", "10.0.1.5"},
		{"an IPv4 address mapped into IPv6", "", "standard", "::ffff:10.0.1.5", "10.0.1.5"},
		{"an IPv6 address", "", "standard", "fd00::5", "fd00::5"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.config.GatewayAddress = netip.MustParseAddr(c.address)
			h.start()
			claim := newClaim(dispatchA)
			claim.Machine = c.machine
			h.claim(claim)
			equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check", "make " + dispatchA + " " + pool + " " + c.size})
			want := `{"version":1,"run_code":"` + runCode + `","gateway":{"url":"` + gatewayURL + `","address":"` + c.written + `"},"max_lifetime_minutes":60}`
			if got := string(h.machines.lastStartData()); got != want {
				t.Errorf("the start data is\n%s\nwant\n%s", got, want)
			}
			equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"dispatched " + dispatchA + " " + machineID(1)})
			equal(t, "the journal", h.entries(), []journal.Entry{{DispatchID: dispatchA, Phase: journal.PhaseDispatched, MachineID: machineID(1), MaxLifetimeMinutes: lifetime}})
			for _, msg := range []string{"claimed a dispatch", "made a machine", "sent dispatched"} {
				if len(h.find(msg)) != 1 {
					t.Errorf("the log has no line %q", msg)
				}
			}
			if err := h.stop(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestTheDefaultSizeIsStandard pins the size of a claim with no machine type.
func TestTheDefaultSizeIsStandard(t *testing.T) {
	if loop.DefaultSize != "standard" {
		t.Errorf("the default size is %q", loop.DefaultSize)
	}
}

// TestAMakeFailureOfEachKind releases the dispatch with the kind's reason, then
// uploads the make's error as the machine log, removes the entry and waits before the
// next claim: 60 s for limit and defect, 15 s for retry and other. A defect is logged
// at the level error. No adapter text reaches the release.
func TestAMakeFailureOfEachKind(t *testing.T) {
	adapterError := func(k string) error {
		return &adapter.Error{Command: "make", Kind: k, Message: "Fix the vendor account: adapter text"}
	}
	for _, c := range []struct {
		name   string
		err    error
		kind   string
		reason string
		wait   time.Duration
		level  string
	}{
		{"limit", adapterError(contracts.FailureLimit), "limit", loop.ReasonLimit, 60 * time.Second, "WARN"},
		{"retry", adapterError(contracts.FailureRetry), "retry", loop.ReasonRetry, 15 * time.Second, "WARN"},
		{"defect", adapterError(contracts.FailureDefect), "defect", loop.ReasonDefect, 60 * time.Second, "ERROR"},
		{"other", adapterError(contracts.FailureOther), "other", loop.ReasonOther, 15 * time.Second, "WARN"},
		{"a request that fails a rule", errors.New("Report this defect in the provisioner"), "defect", loop.ReasonDefect, 60 * time.Second, "ERROR"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.machines.makeErr = c.err
			h.start()
			h.send(claimResult{claim: newClaim(dispatchA), ok: true})
			h.clock.waitPending(t, c.wait, loop.WatchInterval)
			equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"release " + dispatchA + " " + c.reason, "machine-log " + dispatchA + " false"})
			equal(t, "the machine logs", h.apiary.takeUploads(), []upload{{dispatchA, c.err.Error(), false}})
			if strings.Contains(c.reason, "adapter text") {
				t.Errorf("the reason %q contains the adapter's text", c.reason)
			}
			equal(t, "the journal", h.entries(), nil)
			lines := h.find("making a machine failed")
			if len(lines) != 1 || lines[0]["level"] != c.level || lines[0]["kind"] != c.kind || lines[0]["dispatch_id"] != dispatchA {
				t.Errorf("the log lines are %v", lines)
			}
			// The claim path waits the whole wait, and then claims again.
			h.clock.Advance(c.wait - time.Second)
			h.clock.waitPending(t, time.Second, loop.WatchInterval-c.wait+time.Second)
			h.clock.Advance(time.Second)
			h.idle()
			equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check", "make " + dispatchA + " " + pool + " standard"})
			if err := h.stop(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestAMakeErrorWithASecretIsHidden uploads a fixed text as the machine log in place
// of a make's error that contains the run code, a piece of the start data, a run key
// or an access key secret.
func TestAMakeErrorWithASecretIsHidden(t *testing.T) {
	h := newHarness(t)
	h.start()
	// The start data of each claim of newClaim.
	data := `{"version":1,"run_code":"` + runCode + `","gateway":{"url":"` + gatewayURL + `","address":"10.0.1.5"},"max_lifetime_minutes":60}`
	// The piece comes from the end of the start data, so it contains no qecr_, which
	// the check finds also without the start data.
	b64 := base64.StdEncoding.EncodeToString([]byte(data))
	piece := b64[len(b64)-24 : len(b64)-4]
	for i, text := range []string{
		"Fix it: " + runCode,
		"Fix it: " + piece,
		"Fix it: qrk_MARKERrunkey000000000000",
		"Fix it: QAK_MARKERaccesskey0000000000",
	} {
		id := []string{dispatchA, dispatchB, dispatchC, dispatchD}[i]
		if i > 0 {
			h.clock.Advance(loop.ShortWait)
		}
		h.machines.mu.Lock()
		h.machines.makeErr = &adapter.Error{Command: "make", Kind: contracts.FailureRetry, Message: text}
		h.machines.mu.Unlock()
		h.send(claimResult{claim: newClaim(id), ok: true})
		h.clock.waitPending(t, loop.ShortWait, loop.WatchInterval-time.Duration(i)*loop.ShortWait)
		if got := string(h.machines.lastStartData()); got != data {
			t.Fatalf("the start data is %s, want %s", got, data)
		}
		equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"release " + id + " " + loop.ReasonRetry, "machine-log " + id + " false"})
		uploads := h.apiary.takeUploads()
		if len(uploads) != 1 || !strings.Contains(uploads[0].log, "so the provisioner does not show it") || strings.Contains(uploads[0].log, "MARKER") {
			t.Errorf("the machine logs for %q are %+v, want the fixed text", text, uploads)
		}
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAFailedUploadOfAMakeErrorIsLoggedOnce releases the dispatch with the kind's
// reason, and logs the failed upload of the make's error once.
func TestAFailedUploadOfAMakeErrorIsLoggedOnce(t *testing.T) {
	h := newHarness(t)
	h.machines.makeErr = &adapter.Error{Command: "make", Kind: contracts.FailureLimit, Message: "Raise the quota"}
	h.apiary.logErr = errors.New("409 machine_log_exists")
	h.start()
	h.send(claimResult{claim: newClaim(dispatchA), ok: true})
	h.clock.waitPending(t, loop.LongWait, loop.WatchInterval)
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"release " + dispatchA + " " + loop.ReasonLimit, "machine-log " + dispatchA + " false"})
	equal(t, "the journal", h.entries(), nil)
	lines := h.find("uploading the make's error as the machine log failed")
	if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["dispatch_id"] != dispatchA || lines[0]["error"] != "409 machine_log_exists" {
		t.Errorf("the log lines are %v", lines)
	}
	if len(h.find("uploaded the make's error as the machine log")) != 0 || len(h.find("released a dispatch")) != 1 {
		t.Error("the log has a line for an upload, or no line for the release")
	}
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAMakeErrorIsUploadedOnlyWhenNoMachineExists releases the dispatch after a failed
// make, and uploads the make's error only when the release returns 204 or 409
// lease_expired. After 409 machine_exists, 410 gone or a failed release, a machine may
// exist, and its log needs the dispatch's one machine log, so no upload follows.
func TestAMakeErrorIsUploadedOnlyWhenNoMachineExists(t *testing.T) {
	for _, c := range []struct {
		name       string
		releaseErr error
		upload     bool
	}{
		{"204", nil, true},
		{"409 lease_expired", fmt.Errorf("Qory Apiary answered: %w", loop.ErrLeaseExpired), true},
		{"409 machine_exists", machineExists, false},
		{"410 gone", fmt.Errorf("Qory Apiary answered: %w", loop.ErrGone), false},
		{"a failed release", errors.New("Qory Apiary answered 503"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.machines.makeErr = &adapter.Error{Command: "make", Kind: contracts.FailureRetry, Message: "Try again later: the vendor's answer was lost"}
			h.apiary.releaseErr = c.releaseErr
			h.start()
			h.send(claimResult{claim: newClaim(dispatchA), ok: true})
			h.clock.waitPending(t, loop.ShortWait, loop.WatchInterval)
			want := []string{"release " + dispatchA + " " + loop.ReasonRetry}
			var uploads []upload
			if c.upload {
				want = append(want, "machine-log "+dispatchA+" false")
				uploads = []upload{{dispatchA, "Try again later: the vendor's answer was lost (machine adapter command make, kind retry)", false}}
			}
			equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), want)
			equal(t, "the machine logs", h.apiary.takeUploads(), uploads)
			if got := len(h.find("uploaded the make's error as the machine log")); got != len(uploads) {
				t.Errorf("the log has %d lines for an upload of the make's error, want %d", got, len(uploads))
			}
			if err := h.stop(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestAFirstDispatchedThatIsRefusedCleansUp sends dispatched, which returns 409
// lease_expired or 410 gone. The loop reads the machine's log, uploads it, deletes the
// machine and removes the entry. It sends no release after the make.
func TestAFirstDispatchedThatIsRefusedCleansUp(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		msg  string
	}{
		{"lease_expired", fmt.Errorf("Qory Apiary answered: %w", loop.ErrLeaseExpired), "the lease of the dispatch expired before dispatched"},
		{"gone", fmt.Errorf("Qory Apiary answered: %w", loop.ErrGone), "the dispatch ended"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.apiary.setDispatched(dispatchA, c.err)
			h.machines.logText = "boot\nqory run exited\n"
			h.start()
			h.claim(newClaim(dispatchA))
			equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{
				"dispatched " + dispatchA + " " + machineID(1),
				"machine-log " + dispatchA + " false",
			})
			if len(h.apiary.uploads) != 1 || h.apiary.uploads[0].log != "boot\nqory run exited\n" {
				t.Errorf("the uploads are %v", h.apiary.uploads)
			}
			equal(t, "the adapter's calls", h.machines.takeCalls(), []string{
				"check",
				"make " + dispatchA + " " + pool + " standard",
				"log " + machineID(1),
				"delete " + machineID(1),
			})
			equal(t, "the journal", h.entries(), nil)
			for _, msg := range []string{c.msg, "uploaded a machine's log", "deleted a machine"} {
				if len(h.find(msg)) != 1 {
					t.Errorf("the log has no line %q", msg)
				}
			}
			if err := h.stop(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestAFailedDispatchedIsSentAgain leaves the entry made when dispatched fails, and the
// next watch sends it again and records dispatched.
func TestAFailedDispatchedIsSentAgain(t *testing.T) {
	h := newHarness(t)
	h.apiary.setDispatched(dispatchA, errors.New("no answer from Qory Apiary"))
	h.start()
	h.claim(newClaim(dispatchA))
	made := journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseMade, MachineID: machineID(1), MaxLifetimeMinutes: lifetime}
	equal(t, "the journal", h.entries(), []journal.Entry{made})
	if len(h.find("sending dispatched failed")) != 1 {
		t.Error("the log has no line for the failed dispatched")
	}
	h.apiary.setDispatched(dispatchA, nil)
	h.apiary.takeCalls()
	h.tick()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"dispatched " + dispatchA + " " + machineID(1)})
	made.Phase = journal.PhaseDispatched
	equal(t, "the journal", h.entries(), []journal.Entry{made})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAClaimFailureWaits waits 15 s after a claim that failed.
func TestAClaimFailureWaits(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.send(claimResult{err: errors.New("no answer from Qory Apiary")})
	h.clock.waitPending(t, loop.ShortWait, loop.WatchInterval)
	if len(h.find("claiming a dispatch failed")) != 1 {
		t.Error("the log has no line for the failed claim")
	}
	h.clock.Advance(loop.ShortWait)
	h.idle()
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestAClaimThatCannotBeMadeIsReleased releases a claim whose values do not fit in the
// start data, or whose pool is not the provisioner's, makes no machine and waits 60 s.
func TestAClaimThatCannotBeMadeIsReleased(t *testing.T) {
	for _, c := range []struct {
		name   string
		edit   func(*loop.Claim)
		reason string
	}{
		{"a run code out of shape", func(c *loop.Claim) { c.RunCode = loop.NewRunCode("qecr_short") }, loop.ReasonStartData},
		{"no run code", func(c *loop.Claim) { c.RunCode = loop.RunCode{} }, loop.ReasonStartData},
		{"a lifetime of 14", func(c *loop.Claim) { c.MaxLifetimeMinutes = 14 }, loop.ReasonStartData},
		{"a lifetime of 1471", func(c *loop.Claim) { c.MaxLifetimeMinutes = 1471 }, loop.ReasonStartData},
		{"a backslash in the gateway URL", func(c *loop.Claim) { c.GatewayURL = `https://gate\way.example.com` }, loop.ReasonStartData},
		{"a quote in the gateway URL", func(c *loop.Claim) { c.GatewayURL = `https://gate"way.example.com` }, loop.ReasonStartData},
		{"a gateway URL of 16 KiB", func(c *loop.Claim) { c.GatewayURL = "https://" + strings.Repeat("a", 16<<10) }, loop.ReasonStartData},
		{"another pool", func(c *loop.Claim) { c.PoolID = "np_ghjkmnpqrstvwxyz" }, loop.ReasonPool},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.start()
			claim := newClaim(dispatchA)
			c.edit(&claim)
			h.send(claimResult{claim: claim, ok: true})
			h.clock.waitPending(t, loop.LongWait, loop.WatchInterval)
			equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"release " + dispatchA + " " + c.reason})
			equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check"})
			equal(t, "the journal", h.entries(), nil)
			if err := h.stop(); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestTheLoopClaimsOnlyBelowMaxMachines asks for no claim while the journal has
// max_machines entries, and claims again once an entry leaves the journal.
func TestTheLoopClaimsOnlyBelowMaxMachines(t *testing.T) {
	h := newHarness(t)
	h.config.MaxMachines = 1
	h.setEntries(journal.Entry{DispatchID: dispatchA, Phase: journal.PhaseDispatched, MachineID: "i-a", MaxLifetimeMinutes: lifetime})
	h.machines.add("i-a", dispatchA, contracts.MachineRunning, epoch)
	h.start()
	h.tick()
	if n := h.apiary.claimCount(); n != 0 {
		t.Fatalf("the loop asked for %d claims with a full journal", n)
	}
	h.machines.setState("i-a", contracts.MachineStopped)
	h.tick()
	h.idle()
	equal(t, "the journal", h.entries(), nil)
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}

// TestShutdownReleasesAClaimWithNoMake ends the context while a claim answer arrives.
// The claim is released, no machine is made, and Run returns nil.
func TestShutdownReleasesAClaimWithNoMake(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.send(claimResult{claim: newClaim(dispatchA), ok: true, before: h.cancel})
	if err := h.result(); err != nil {
		t.Error(err)
	}
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"release " + dispatchA + " " + loop.ReasonStopped})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check"})
	equal(t, "the journal", h.entries(), nil)
	if len(h.find("stopped claiming")) != 1 {
		t.Error("the log has no line for the stop")
	}
}

// TestShutdownLetsAMakeInFlightFinish ends the context during a make. The make
// finishes, its machine is recorded, and the loop sends nothing more and deletes
// nothing.
func TestShutdownLetsAMakeInFlightFinish(t *testing.T) {
	h := newHarness(t)
	h.machines.makeStarted = make(chan struct{})
	h.machines.makeGate = make(chan struct{})
	h.start()
	h.send(claimResult{claim: newClaim(dispatchA), ok: true})
	select {
	case <-h.machines.makeStarted:
	case <-time.After(patience):
		t.Fatal("the make did not start")
	}
	equal(t, "the journal", h.entries(), []journal.Entry{{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}})
	h.cancel()
	close(h.machines.makeGate)
	if err := h.result(); err != nil {
		t.Error(err)
	}
	equal(t, "the journal", h.entries(), []journal.Entry{{DispatchID: dispatchA, Phase: journal.PhaseMade, MachineID: machineID(1), MaxLifetimeMinutes: lifetime}})
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check", "make " + dispatchA + " " + pool + " standard"})
}

// TestAJournalThatCannotBeWrittenStopsTheLoop releases the claim and returns the
// journal's error when the claimed entry cannot be written.
func TestAJournalThatCannotBeWrittenStopsTheLoop(t *testing.T) {
	h := newHarness(t)
	h.start()
	// A folder that is not empty at the journal's new file blocks each write.
	if err := os.MkdirAll(filepath.Join(h.dir, journal.File+".new", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.send(claimResult{claim: newClaim(dispatchA), ok: true})
	err := h.result()
	if err == nil || !strings.HasPrefix(err.Error(), "check the state folder: writing the journal of live dispatches failed") {
		t.Errorf("the error is %v", err)
	}
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"release " + dispatchA + " " + loop.ReasonJournal})
	equal(t, "the adapter's calls", h.machines.takeCalls(), []string{"check"})
}

// TestTheReasonsFitTheContract checks that each release reason is 1 to 200 characters
// and differs from the others.
func TestTheReasonsFitTheContract(t *testing.T) {
	reasons := []string{
		loop.ReasonLimit, loop.ReasonRetry, loop.ReasonDefect, loop.ReasonOther,
		loop.ReasonStartData, loop.ReasonPool, loop.ReasonJournal, loop.ReasonStopped,
		journal.ReasonRestarted,
	}
	for _, r := range reasons {
		if n := len([]rune(r)); n < 1 || n > 200 {
			t.Errorf("%q is %d characters", r, n)
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(reasons)))) != len(reasons) {
		t.Errorf("two reasons are the same: %q", reasons)
	}
}

// TestAJournalThatCannotBeWrittenAfterTheMakeStopsTheLoop returns the journal's error
// when the made entry cannot be written, and sends no release. After a restart, the
// claimed entry finds the machine by its key and keeps the claim's lifetime.
func TestAJournalThatCannotBeWrittenAfterTheMakeStopsTheLoop(t *testing.T) {
	h := newHarness(t)
	h.machines.makeStarted = make(chan struct{})
	h.machines.makeGate = make(chan struct{})
	h.start()
	h.send(claimResult{claim: newClaim(dispatchA), ok: true})
	select {
	case <-h.machines.makeStarted:
	case <-time.After(patience):
		t.Fatal("the make did not start")
	}
	blocker := filepath.Join(h.dir, journal.File+".new")
	if err := os.MkdirAll(filepath.Join(blocker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	close(h.machines.makeGate)
	err := h.result()
	if err == nil || !strings.HasPrefix(err.Error(), "check the state folder: writing the journal of live dispatches failed") {
		t.Errorf("the error is %v", err)
	}
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), nil)
	equal(t, "the journal", h.entries(), []journal.Entry{{DispatchID: dispatchA, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}})

	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	h.restart()
	equal(t, "Qory Apiary's calls", h.apiary.takeCalls(), []string{"dispatched " + dispatchA + " " + machineID(1)})
	equal(t, "the journal", h.entries(), []journal.Entry{{DispatchID: dispatchA, Phase: journal.PhaseDispatched, MachineID: machineID(1), MaxLifetimeMinutes: lifetime}})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
}
