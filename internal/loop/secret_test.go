package loop_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/qoryai/hive/internal/adapter"
	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/journal"
	"github.com/qoryai/hive/internal/loop"
	"github.com/qoryai/integrations/contracts"
)

// TestARunCodeNeverFormatsAsItsText formats a run code and a claim with every verb,
// as JSON and in log lines. None contains the run code.
func TestARunCodeNeverFormatsAsItsText(t *testing.T) {
	c := newClaim(dispatchA)
	var out []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		out = append(out, fmt.Sprintf(verb, c.RunCode), fmt.Sprintf(verb, c), fmt.Sprintf(verb, &c))
	}
	out = append(out, fmt.Sprint(c), c.RunCode.String(), fmt.Errorf("wrapped: %v", c).Error())
	for _, v := range []any{c, &c, c.RunCode} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	text, _ := c.RunCode.MarshalText()
	out = append(out, string(text))
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", slog.Any("claim", c), slog.Any("code", c.RunCode))
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", slog.Any("claim", c), slog.Any("code", c.RunCode))
	out = append(out, buf.String())
	for _, s := range out {
		if strings.Contains(s, "MARKER") || strings.Contains(strings.ToLower(s), "qecr_") {
			t.Errorf("%q contains the run code", s)
		}
	}
	if !strings.Contains(fmt.Sprint(c), "[run code]") {
		t.Errorf("%v does not contain [run code]", c)
	}
}

// TestNoLogLineContainsTheRunCode runs claims whose failures carry the marker run
// code, a piece of the start data and a run key in their texts: a make, a claim, a
// dispatched, a release, a machine log, a read of a log and a delete. No log line
// contains the run code, a piece of the start data in base64 or the run key, and the
// lines that would have shown them say that the text is hidden.
func TestNoLogLineContainsTheRunCode(t *testing.T) {
	h := newHarness(t)
	h.start()
	// A make whose adapter message contains the run code.
	h.machines.makeErr = &adapter.Error{Command: "make", Kind: contracts.FailureRetry, Message: "Fix it: " + runCode}
	h.send(claimResult{claim: newClaim(dispatchA), ok: true})
	h.clock.waitPending(t, loop.ShortWait, loop.WatchInterval)
	data := h.machines.lastStartData()
	if !bytes.Contains(data, []byte(runCode)) {
		t.Fatalf("the start data %q has no run code", data)
	}
	h.clock.Advance(loop.ShortWait)
	// A make whose adapter message contains a piece of the start data in base64.
	piece := base64.StdEncoding.EncodeToString(data)[20:40]
	h.machines.mu.Lock()
	h.machines.makeErr = &adapter.Error{Command: "make", Kind: contracts.FailureRetry, Message: "Fix it: " + piece}
	h.machines.mu.Unlock()
	h.apiary.mu.Lock()
	h.apiary.releaseErr = errors.New("release failed for " + runCode)
	h.apiary.mu.Unlock()
	h.send(claimResult{claim: newClaim(dispatchB), ok: true})
	h.clock.waitPending(t, loop.ShortWait, loop.WatchInterval-loop.ShortWait)
	h.clock.Advance(loop.ShortWait)
	// A claim whose error contains a run key.
	h.send(claimResult{err: errors.New("claim failed: qrk_MARKERrunkey000000000000")})
	h.clock.waitPending(t, loop.ShortWait, loop.WatchInterval-2*loop.ShortWait)
	h.clock.Advance(loop.ShortWait)
	// A made machine whose dispatched fails with the run code.
	h.machines.mu.Lock()
	h.machines.makeErr = nil
	h.machines.mu.Unlock()
	h.apiary.setDispatched(dispatchC, errors.New("dispatched failed for "+runCode))
	h.claim(newClaim(dispatchC))
	// At the first watch, the dispatch ended, and the read of the log and the delete
	// fail with the run code.
	h.machines.mu.Lock()
	h.machines.logErr = &adapter.Error{Command: "log", Kind: contracts.FailureOther, Message: "log " + runCode}
	h.machines.deleteErr = []error{&adapter.Error{Command: "delete", Kind: contracts.FailureOther, Message: "delete " + runCode}}
	h.machines.mu.Unlock()
	h.apiary.setDispatched(dispatchC, fmt.Errorf("dispatched failed for %s: %w", runCode, loop.ErrGone))
	h.clock.Advance(loop.WatchInterval - 3*loop.ShortWait)
	h.clock.waitPending(t, loop.WatchInterval)
	// At the second watch, the upload of the log fails with the run code.
	h.machines.mu.Lock()
	h.machines.logErr = nil
	h.machines.mu.Unlock()
	h.apiary.mu.Lock()
	h.apiary.logErr = errors.New("machine log failed for " + runCode)
	h.apiary.mu.Unlock()
	h.tick()
	// The failed release keeps the claimed entry of dispatchB for the watch.
	equal(t, "the journal", h.entries(), []journal.Entry{{DispatchID: dispatchB, Phase: journal.PhaseClaimed, MaxLifetimeMinutes: lifetime}})
	if err := h.stop(); err != nil {
		t.Error(err)
	}
	logs := h.logs.String()
	for _, secret := range []string{"MARKER", "qecr_", "qrk_", piece} {
		if strings.Contains(strings.ToLower(logs), strings.ToLower(secret)) {
			t.Errorf("the log contains %q:\n%s", secret, logs)
		}
	}
	if adapterio.ContainsSecret(logs, data) {
		t.Errorf("the log contains a piece of the start data:\n%s", logs)
	}
	hidden := 0
	for _, l := range h.lines() {
		if e, ok := l["error"].(string); ok && strings.Contains(e, "so the provisioner does not show it") {
			hidden++
		}
	}
	// Two makes, a release, a claim, a dispatched, a read of a log, a delete and an
	// upload.
	if hidden != 8 {
		t.Errorf("%d lines say that the text is hidden, want 8:\n%s", hidden, logs)
	}
}
