package machine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/smithy-go"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/integrations/contracts"
)

// The EC2 error codes this package acts on.
const (
	codeInstanceLimit      = "InstanceLimitExceeded"
	codeVcpuLimit          = "VcpuLimitExceeded"
	codeCapacity           = "InsufficientInstanceCapacity"
	codeSubnetFull         = "InsufficientFreeAddressesInSubnet"
	codeUnsupported        = "UnsupportedOperation"
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
	// Kind is the kind of failure that the adapter writes in its error document.
	Kind string
	// Code is the EC2 error code, such as VcpuLimitExceeded; empty when the call
	// returned no EC2 error.
	Code string
	// Err is the SDK's error.
	Err error
}

// settingsFile is how an error refers to the adapter's settings file.
const settingsFile = "aws-ec2.json"

// Error starts with what the instance admin does, then the cause.
func (e *Error) Error() string {
	var maxAttempts *retry.MaxAttemptsError
	switch {
	case e.Code == codeCapacity:
		return fmt.Sprintf("Wait, or map the size to another instance type in %s: %s failed with %s in the subnet's zone: %v", settingsFile, e.Op, e.Code, e.Err)
	case e.Code == codeSubnetFull:
		return fmt.Sprintf("Wait until machines end, or set subnet_id in %s to a subnet with free addresses: %s failed with %s: %v", settingsFile, e.Op, e.Code, e.Err)
	case e.Kind == contracts.FailureLimit:
		return fmt.Sprintf("Wait until machines end, or raise the account's EC2 instance or vCPU quota: %s failed with %s: %v", e.Op, e.Code, e.Err)
	case e.Code == codeUnauthorized:
		return fmt.Sprintf("Give the instance role the EC2 rights that the EC2 machine adapter needs: %s failed with %s: %v", e.Op, e.Code, e.Err)
	case e.Code == codeTokenMismatch:
		return fmt.Sprintf("Report a provisioner defect, unless %s changed since the first make: %s reused a key with other parameters (%s): %v", settingsFile, e.Op, e.Code, e.Err)
	case e.Code == codeTokenTerminated:
		return fmt.Sprintf("No action needed: the machine made for this key is terminated, and EC2 makes no second machine for the key: %v", e.Err)
	case e.Kind == contracts.FailureRetry && errors.As(e.Err, &maxAttempts):
		return fmt.Sprintf("If this repeats, check EC2's service health and the account's request rate in this region: %s failed after the SDK's %d attempts: %v", e.Op, maxAttempts.Attempt, e.Err)
	case e.Kind == contracts.FailureRetry && errors.Is(e.Err, context.DeadlineExceeded):
		return fmt.Sprintf("If this repeats, check the network path to EC2: %s did not finish within its time limit: %v", e.Op, e.Err)
	case e.Kind == contracts.FailureRetry:
		return fmt.Sprintf("If this repeats, check EC2's service health and the account's request rate in this region: %s failed with an error the SDK retries: %v", e.Op, e.Err)
	case e.Code == codeInstanceIDNotFound:
		return fmt.Sprintf("Check that the instance exists in this region: %s failed with %s, which EC2 can return right after RunInstances: %v", e.Op, e.Code, e.Err)
	case e.Code != "":
		return fmt.Sprintf("Look up EC2 error code %s, then check %s and the instance role: %s failed: %v", e.Code, settingsFile, e.Op, e.Err)
	case errors.Is(e.Err, context.Canceled):
		return fmt.Sprintf("No action needed: %s stopped when its context ended: %v", e.Op, e.Err)
	default:
		return fmt.Sprintf("Check the instance role and the network path to EC2: %s failed: %v", e.Op, e.Err)
	}
}

// Unwrap returns the SDK's error.
func (e *Error) Unwrap() error { return e.Err }

// classify returns the kind of err. It reads the error code first, because the limit
// codes are client errors that the SDK does not retry, and a capacity error that the
// SDK retried is still a limit.
func classify(err error) string {
	code := errorCode(err)
	switch {
	case code == codeInstanceLimit || code == codeVcpuLimit || code == codeCapacity || code == codeSubnetFull:
		return contracts.FailureLimit
	case defectCode(code):
		return contracts.FailureDefect
	}
	var maxAttempts *retry.MaxAttemptsError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &maxAttempts):
		return contracts.FailureRetry
	case errors.Is(err, context.Canceled):
		// The caller's context ended; a retry inside that context fails at once.
		return contracts.FailureOther
	case retry.IsErrorRetryables(retry.DefaultRetryables).IsErrorRetryable(err) == aws.TrueTernary:
		return contracts.FailureRetry
	}
	return contracts.FailureOther
}

// defectCode reports whether the EC2 error code means that the request, the settings
// or the instance role's rights are wrong, so a retry fails the same way. An unknown
// instance id is not a defect: right after RunInstances, EC2 can return
// InvalidInstanceID.NotFound for an instance that exists.
func defectCode(code string) bool {
	switch {
	case code == codeUnauthorized, code == codeTokenMismatch, code == "MissingParameter", code == "UnknownParameter":
		return true
	case code == codeInstanceIDNotFound:
		return false
	}
	return strings.HasPrefix(code, "Invalid") || strings.HasPrefix(code, "Unsupported")
}

// errorCode returns the EC2 error code in err's chain, or "".
func errorCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

// TokenTerminated reports whether err is IdempotentInstanceTerminated: the machine
// that RunInstances made for the client token is terminated, and EC2 makes no second
// machine for the token.
func TokenTerminated(err error) bool {
	return errorCode(err) == codeTokenTerminated
}

// wrap returns err as an *Error for op, or nil when err is nil.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Op: op, Kind: classify(err), Code: errorCode(err), Err: err}
}

// defect returns a failure of the kind defect with message.
func defect(message string) error {
	return &adapterio.Failure{Kind: contracts.FailureDefect, Message: message}
}

// other returns a failure of the kind other with message.
func other(message string) error {
	return &adapterio.Failure{Kind: contracts.FailureOther, Message: message}
}
