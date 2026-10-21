package machine

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"
)

// Kind is the class of an EC2 call's error. It sets what the caller does next.
type Kind int

const (
	// KindOther is every error that no other kind matches. The caller does not
	// repeat the call with the same input.
	KindOther Kind = iota
	// KindRetried is an error that the SDK's standard retryer retries: throttling
	// (RequestLimitExceeded), HTTP 500, 502, 503 and 504, a request timeout, and a
	// connection error. When the caller receives it, the SDK's attempts or its retry
	// quota ran out; the caller waits and calls again.
	KindRetried
	// KindLimit is InstanceLimitExceeded or VcpuLimitExceeded: the account has no room
	// for another machine.
	KindLimit
	// KindTokenMismatch is IdempotentParameterMismatch: an earlier RunInstances used
	// the same client token with different parameters.
	KindTokenMismatch
	// KindTokenTerminated is IdempotentInstanceTerminated: the machine started with
	// this client token is terminated, and EC2 starts no second one for the token.
	KindTokenTerminated
)

// String returns the kind's name for a log attribute.
func (k Kind) String() string {
	switch k {
	case KindRetried:
		return "retried"
	case KindLimit:
		return "limit"
	case KindTokenMismatch:
		return "token-mismatch"
	case KindTokenTerminated:
		return "token-terminated"
	default:
		return "other"
	}
}

// The EC2 error codes this package acts on.
const (
	codeInstanceLimit      = "InstanceLimitExceeded"
	codeVcpuLimit          = "VcpuLimitExceeded"
	codeTokenMismatch      = "IdempotentParameterMismatch"
	codeTokenTerminated    = "IdempotentInstanceTerminated"
	codeDryRun             = "DryRunOperation"
	codeUnauthorized       = "UnauthorizedOperation"
	codeInstanceIDNotFound = "InvalidInstanceID.NotFound"
)

// Error is a failed EC2 call. Errors from Client's methods that come from an EC2 call
// are *Error.
type Error struct {
	// Op is the EC2 operation, such as RunInstances.
	Op string
	// Kind is the class of Err.
	Kind Kind
	// Code is the EC2 error code, such as VcpuLimitExceeded; empty when the call
	// returned no EC2 error.
	Code string
	// Err is the SDK's error.
	Err error
}

// Error starts with what to do, then the cause.
func (e *Error) Error() string {
	switch {
	case e.Kind == KindRetried:
		return fmt.Sprintf("Call %s again after a pause: the call failed with an error that the SDK retries: %v", e.Op, e.Err)
	case e.Kind == KindLimit:
		return fmt.Sprintf("Wait until machines end, or raise the account's EC2 instance or vCPU quota: %s failed with %s: %v", e.Op, e.Code, e.Err)
	case e.Kind == KindTokenMismatch:
		return fmt.Sprintf("Release the run, and send no second %s for its dispatch: the client token was used before with different parameters: %v", e.Op, e.Err)
	case e.Kind == KindTokenTerminated:
		return fmt.Sprintf("Release the run: the machine started with this client token is terminated, and EC2 starts no second machine for it: %v", e.Err)
	case e.Code == codeUnauthorized:
		return fmt.Sprintf("Give the instance role the provisioner's EC2 rights: %s failed with %s: %v", e.Op, e.Code, e.Err)
	case e.Code != "":
		return fmt.Sprintf("Fix the cause of EC2 error %s: %s failed: %v", e.Code, e.Op, e.Err)
	case errors.Is(e.Err, context.Canceled), errors.Is(e.Err, context.DeadlineExceeded):
		return fmt.Sprintf("No action needed: %s stopped when its context ended: %v", e.Op, e.Err)
	default:
		return fmt.Sprintf("Check the instance role and the network path to EC2: %s failed: %v", e.Op, e.Err)
	}
}

// Unwrap returns the SDK's error.
func (e *Error) Unwrap() error { return e.Err }

// Classify returns the kind of err: the Kind of an *Error in its chain, or else the
// kind that follows from err's EC2 error code and the SDK's standard retry rules.
func Classify(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return classify(err)
}

// classify reads the kind from the error code first, because the limit and token codes
// are client errors that the SDK does not retry.
func classify(err error) Kind {
	switch errorCode(err) {
	case codeInstanceLimit, codeVcpuLimit:
		return KindLimit
	case codeTokenMismatch:
		return KindTokenMismatch
	case codeTokenTerminated:
		return KindTokenTerminated
	}
	var maxAttempts *retry.MaxAttemptsError
	if errors.As(err, &maxAttempts) {
		return KindRetried
	}
	// The caller's context ended; a retry inside that context fails at once.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return KindOther
	}
	if retry.IsErrorRetryables(retry.DefaultRetryables).IsErrorRetryable(err) == aws.TrueTernary {
		return KindRetried
	}
	return KindOther
}

// errorCode returns the EC2 error code in err's chain, or "".
func errorCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

// wrap returns err as an *Error for op, or nil when err is nil.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Op: op, Kind: classify(err), Code: errorCode(err), Err: err}
}
