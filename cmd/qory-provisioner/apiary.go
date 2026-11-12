package main

import (
	"context"
	"errors"
	"strings"

	"github.com/qoryai/hive/internal/loop"
	"github.com/qoryai/hive/internal/provisioner"
	"github.com/qoryai/integrations/contracts"
)

// claimWait is the wait_seconds of every claim: the most that claim.schema.json of
// provisioner/v1 allows, 25. While no work can be claimed, Qory Apiary holds the claim
// that long, so an idle provisioner sends about one claim every 25 seconds.
const claimWait = provisioner.MaxWaitSeconds

// apiary is the provisioner's client of Qory Apiary as the loop uses it, see
// [loop.Apiary]. Each error keeps the client's text, and the errors 410 gone, 409
// lease_expired, 409 machine_exists and the signed 403 forbidden also match the loop's
// sentinel of the same refusal. A machine end's errors match the loop's sentinels
// through [toLoopEnd].
type apiary struct {
	client *provisioner.Client
}

var _ loop.Apiary = apiary{}

// Claim claims a request for one machine, with a wait of [claimWait] and offers, the
// machines the adapter can make. It copies the run code into the loop's claim and
// clears the client's copy.
func (a apiary) Claim(ctx context.Context, offers []contracts.MachineSpec) (loop.Claim, bool, error) {
	c, ok, err := a.client.Claim(ctx, claimWait, offers)
	if err != nil || !ok {
		return loop.Claim{}, false, toLoop(err)
	}
	defer c.RunCode.Clear()
	var code strings.Builder
	code.Grow(c.RunCode.Len())
	// A strings.Builder returns no error on a write.
	_, _ = c.RunCode.WriteTo(&code)
	return loop.Claim{
		DispatchID:         c.DispatchID,
		PoolID:             c.PoolID,
		LeaseUntil:         c.LeaseUntil,
		Machine:            c.Machine,
		MachineSpec:        c.MachineSpec,
		RunCode:            loop.NewRunCode(code.String()),
		GatewayURL:         c.GatewayURL,
		MaxLifetimeMinutes: c.MaxLifetimeMinutes,
	}, true, nil
}

// Dispatched sends dispatched for the dispatch with machineType and no external. The
// contract's external needs the job's system, its ref and its https page together,
// and the machine adapter returns a machine id alone, so the machine id is not sent.
func (a apiary) Dispatched(ctx context.Context, dispatchID, _, machineType string) error {
	return toLoop(a.client.Dispatched(ctx, dispatchID, nil, machineType))
}

// Release gives the dispatch back with reason.
func (a apiary) Release(ctx context.Context, dispatchID, reason string) error {
	return toLoop(a.client.Release(ctx, dispatchID, reason))
}

// MachineLog uploads the machine's log of the dispatch. The contract never refuses a
// machine log with 410 gone or 409 lease_expired, so its error matches no loop sentinel.
func (a apiary) MachineLog(ctx context.Context, dispatchID, log string, truncated bool) error {
	return toLoop(a.client.MachineLog(ctx, dispatchID, log, truncated))
}

// MachineEnd sends how the removal of the dispatch's machine went, with one try.
func (a apiary) MachineEnd(ctx context.Context, dispatchID, state string) error {
	return toLoopEnd(a.client.MachineEnd(ctx, dispatchID, state))
}

// sentinels pairs each refusal of the client that the loop acts on with the loop's
// sentinel for it.
var sentinels = []struct{ client, loop error }{
	{provisioner.ErrGone, loop.ErrGone},
	{provisioner.ErrLeaseExpired, loop.ErrLeaseExpired},
	{provisioner.ErrMachineExists, loop.ErrMachineExists},
	{provisioner.ErrForbidden, loop.ErrForbidden},
}

// toLoop is err as the loop reads it: a [refusal] when errors.Is matches err against a
// client error in [sentinels], and err as it is otherwise, nil included.
func toLoop(err error) error {
	if err == nil {
		return nil
	}
	for _, s := range sentinels {
		if errors.Is(err, s.client) {
			return &refusal{err: err, sentinel: s.loop}
		}
	}
	return err
}

// toLoopEnd is err of a machine end as the loop reads it: a [refusal] with
// [loop.ErrMachineEndStopped] for a signed 400, 403 or 404, a [refusal] with
// [loop.ErrMachineEndUnusable] for a signed answer that breaks the contract, which the
// loop counts as done for deleting and delete_failed and as failed for gone, and err
// as it is otherwise, nil included, so the loop sends the post again.
func toLoopEnd(err error) error {
	switch {
	case errors.Is(err, provisioner.ErrMachineEndStopped):
		return &refusal{err: err, sentinel: loop.ErrMachineEndStopped}
	case errors.Is(err, provisioner.ErrAnswerUnusable):
		return &refusal{err: err, sentinel: loop.ErrMachineEndUnusable}
	}
	return err
}

// refusal is a client error that also matches a loop sentinel. Its text is the client
// error's text.
type refusal struct {
	err, sentinel error
}

// Error returns the client error's text.
func (r *refusal) Error() string { return r.err.Error() }

// Unwrap returns the client error and the loop sentinel, so errors.Is and errors.As
// match either.
func (r *refusal) Unwrap() []error { return []error{r.err, r.sentinel} }
