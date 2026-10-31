package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/qoryai/integrations/contracts"
)

// Dispatched sends POST /v1/dispatches/<dispatchID>/dispatched, dispatched.schema.json:
// the machine for the dispatch exists and has the run code and the gateway URL.
// external is the job at the provider, or nil to send none. The provisioner sends the
// first dispatched before the claim's LeaseUntil, then again every 60 seconds with the
// same external while the machine exists; each one is built anew. The answer is a
// signed 204 while the dispatch lives. A refusal is a [*Refusal]: [ErrForbidden], a
// dispatch that is unknown or of another node pool; [ErrGone], a dispatch that ended,
// after which the contract requires the provisioner to send the machine log and delete
// the machine; [ErrLeaseExpired], only for the first dispatched of a stale dispatch,
// sent at or after the lease's end with no redemption of the run code before it, after
// which the contract requires the provisioner to stop the machine; [ErrNotFound]; or
// [ErrRateLimited].
func (c *Client) Dispatched(ctx context.Context, dispatchID string, external *contracts.External) error {
	what, err := ofDispatch("dispatched for ", dispatchID)
	if err != nil {
		return err
	}
	return c.toDispatch(ctx, what, kindDispatched, dispatchID, "dispatched", "dispatched.schema.json", func(t string) any {
		return contracts.Dispatched{Version: 1, Time: t, External: external}
	})
}

// Release sends POST /v1/dispatches/<dispatchID>/release, release.schema.json: the
// provisioner gives the dispatch back before dispatched, for reason, display text of 1
// to [MaxReason] characters. The release ends the dispatch, and the run code stops
// working; whether the work is claimed again is Qory Apiary's choice. The answer is a
// signed 204. A refusal is a [*Refusal]: [ErrForbidden], a dispatch that is unknown or
// of another node pool; [ErrGone], a dispatch that ended, which is also the answer to
// a try sent again after a release whose 204 was lost, with [Refusal.Resent] set;
// [ErrMachineExists], a dispatch after dispatched or after the machine redeemed the run
// code; [ErrLeaseExpired], a dispatch whose lease lapsed before dispatched or a
// redemption; [ErrNotFound]; or [ErrRateLimited].
func (c *Client) Release(ctx context.Context, dispatchID, reason string) error {
	what, err := ofDispatch("the release of ", dispatchID)
	if err != nil {
		return err
	}
	if n := utf8.RuneCountInString(reason); n < 1 || n > MaxReason {
		return fmt.Errorf("%s: give a reason of 1 to %d characters; this one has %d", what, MaxReason, n)
	}
	return c.toDispatch(ctx, what, kindRelease, dispatchID, "release", "release.schema.json", func(t string) any {
		return contracts.Release{Version: 1, Time: t, Reason: reason}
	})
}

// hiddenLine replaces each line of a machine log that contains what looks like a
// secret.
const hiddenLine = "[hidden: the line contained what looks like an access key secret, a run key or a run code]"

// MachineLog sends POST /v1/dispatches/<dispatchID>/machine-log,
// machine-log.schema.json: the log of the machine the provisioner created for the
// dispatch, from the machine's creation to its end, or, when the provisioner created
// no machine, the error text of its attempt to create one. truncated is whether the
// caller cut the start of log. Before it sends the log, the client replaces each line
// that contains what looks like an access key secret, a run key or a run code with a
// fixed text, each U+0000 with U+FFFD, and each run of bytes that is not UTF-8 with
// one U+FFFD. It then keeps the last part of the log, from the start of a line when
// the part has one and otherwise from the start of a character, such that the log is
// at most [MaxLog] bytes and the body at most contracts.MaxRequest bytes as sent, and
// sets truncated when it cut anything. So the log sent is valid UTF-8. Qory Apiary
// takes one machine log per dispatch, before or after the dispatch ends. The answer
// is a signed 204. A refusal is a [*Refusal]: [ErrForbidden], a dispatch that is
// unknown or of another node pool; [ErrMachineLogExists], a dispatch that has a
// machine log; [ErrNotFound]; or [ErrRateLimited].
func (c *Client) MachineLog(ctx context.Context, dispatchID, log string, truncated bool) error {
	what, err := ofDispatch("the machine log of ", dispatchID)
	if err != nil {
		return err
	}
	log, cut := fitLog(cleanLog(log))
	truncated = truncated || cut
	return c.toDispatch(ctx, what, kindMachineLog, dispatchID, "machine-log", "machine-log.schema.json", func(t string) any {
		return contracts.MachineLog{Version: 1, Time: t, Log: log, Truncated: truncated}
	})
}

// MachineEnd sends POST /v1/dispatches/<dispatchID>/machine-end,
// machine-end.schema.json: the result of deleting the machine the provisioner created
// for the dispatch. state is [contracts.MachineEndDeleting], [contracts.MachineEndGone]
// or [contracts.MachineEndDeleteFailed]. Qory Apiary takes it any number of times,
// before or after the dispatch ends. The answer is a signed 204. A refusal is a
// [*Refusal]: [ErrForbidden], a dispatch that is unknown or of another node pool;
// [ErrUnavailable]; [ErrNotFound]; or [ErrRateLimited]. A signed 400, 403 or 404 also
// matches [ErrMachineEndStopped]. A machine end is never refused 410 gone or 409
// lease_expired.
func (c *Client) MachineEnd(ctx context.Context, dispatchID, state string) error {
	return c.machineEnd(ctx, dispatchID, state, false)
}

// MachineEndOnce is [Client.MachineEnd] with one try and no retries.
func (c *Client) MachineEndOnce(ctx context.Context, dispatchID, state string) error {
	return c.machineEnd(ctx, dispatchID, state, true)
}

// machineEnd sends a machine end, with one try when once is true.
func (c *Client) machineEnd(ctx context.Context, dispatchID, state string, once bool) error {
	what, err := ofDispatch("the machine end of ", dispatchID)
	if err != nil {
		return err
	}
	return c.sendToDispatch(ctx, request{
		what: what, kind: kindMachineEnd, path: routeDispatches + dispatchID + "/machine-end", schema: "machine-end.schema.json", limit: MaxAnswer, timeout: RequestTimeout, once: once,
		body: func(t string) any { return contracts.MachineEnd{Version: 1, Time: t, State: state} },
	})
}

// cleanLog is log with each U+0000 replaced by U+FFFD, each run of bytes that is not
// UTF-8 replaced by one U+FFFD, and each line that contains what looks like a secret
// replaced by [hiddenLine]. A line ends after a line feed; the line feeds are kept.
func cleanLog(log string) string {
	replacement := string(utf8.RuneError)
	log = strings.ReplaceAll(strings.ToValidUTF8(log, replacement), "\x00", replacement)
	if !secret(log) {
		return log
	}
	lines := strings.SplitAfter(log, "\n")
	for i, line := range lines {
		if secret(line) {
			lines[i] = hiddenLine + line[len(strings.TrimRight(line, "\n")):]
		}
	}
	return strings.Join(lines, "")
}

// escapedLen is the bytes each ASCII character takes in a JSON string as json.Marshal
// writes it, and lineSepLen those of U+2028 and U+2029. Every other valid rune takes
// its UTF-8 bytes.
var (
	escapedLen [utf8.RuneSelf]int
	lineSepLen int
)

// logOverhead is the bytes of a machine log's body other than the log, with
// truncated set.
var logOverhead int

func init() {
	for r := range utf8.RuneSelf {
		b, _ := json.Marshal(string(rune(r)))
		escapedLen[r] = len(b) - 2
	}
	b, _ := json.Marshal(string(rune(0x2028)))
	lineSepLen = len(b) - 2
	b, _ = json.Marshal(contracts.MachineLog{Version: 1, Time: "2006-01-02T15:04:05Z", Truncated: true})
	logOverhead = len(b)
}

// fitLog is the last part of log, which is valid UTF-8, that is at most [MaxLog] bytes
// and whose body is at most contracts.MaxRequest bytes, as json.Marshal writes it. It
// steps back one whole character at a time, so the part starts at a character. When it
// cuts, the part starts after the first line feed in it, if one is there before its
// end, and cut is true.
func fitLog(log string) (part string, cut bool) {
	budget := contracts.MaxRequest - logOverhead
	start, size, written := len(log), 0, 0
	for start > 0 {
		r, n := utf8.DecodeLastRuneInString(log[:start])
		e := n
		switch {
		case r < utf8.RuneSelf:
			e = escapedLen[r]
		case r == 0x2028 || r == 0x2029:
			e = lineSepLen
		}
		if size+n > MaxLog || written+e > budget {
			break
		}
		size, written, start = size+n, written+e, start-n
	}
	if start == 0 {
		return log, false
	}
	if i := strings.IndexByte(log[start:], '\n'); i >= 0 && start+i+1 < len(log) {
		start += i + 1
	}
	return log[start:], true
}

// ofDispatch is the request what, prefix and dispatchID, once dispatchID is of the
// contract's form. Its error does not contain dispatchID, which may be any text.
func ofDispatch(prefix, dispatchID string) (string, error) {
	if !dispatchIDShape.MatchString(dispatchID) {
		return "", fmt.Errorf("report this defect in the provisioner: %sa dispatch: the dispatch id is not dp_ and 16 lower-case Crockford base32 characters", prefix)
	}
	return prefix + dispatchID, nil
}

// toDispatch sends a request about one dispatch, whose id ofDispatch checked, with a
// signed 204 as its answer.
func (c *Client) toDispatch(ctx context.Context, what string, k kind, dispatchID, action, schema string, body func(string) any) error {
	return c.sendToDispatch(ctx, request{
		what: what, kind: k, path: routeDispatches + dispatchID + "/" + action, schema: schema, limit: MaxAnswer, timeout: RequestTimeout, body: body,
	})
}

// sendToDispatch sends req, whose answer is a signed 204.
func (c *Client) sendToDispatch(ctx context.Context, req request) error {
	ans, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	defer clear(ans.body)
	if ans.status != http.StatusNoContent {
		return c.refused(req.what, req.kind, ans)
	}
	return nil
}
