package machine

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/qoryai/hive/internal/adapterio"
)

// minShown is how many characters of a variable text at the end of a message, such as
// the SDK's error, fit in adapterio.MaxMessage after the fix and the cause.
const minShown = 40

// wantFits fails t unless message ends with tail, and message with tail cut to its
// first minShown characters has at most adapterio.MaxMessage characters. tail is ""
// for a message whose variable parts are all bounded.
func wantFits(t *testing.T, message, tail string) {
	t.Helper()
	if !strings.HasSuffix(message, tail) {
		t.Errorf("the message does not end with %q: %s", tail, message)
		return
	}
	n := utf8.RuneCountInString(message) - utf8.RuneCountInString(tail) + min(utf8.RuneCountInString(tail), minShown)
	if n > adapterio.MaxMessage {
		t.Errorf("%d characters, more than %d: %s", n, adapterio.MaxMessage, message)
	}
}

// TestMessagesFit checks that each fixed message of this package, with typical values,
// fits in adapterio.MaxMessage, and that an SDK error comes last in it, so the cut to
// that length removes only the end of the SDK error.
func TestMessagesFit(t *testing.T) {
	ctx := context.Background()
	const (
		longOp     = "DescribeInstances dry run"
		instanceID = "i-0123456789abcdef0"
		imageID    = "ami-0123456789abcdef0"
	)
	sdk := func(code string) error {
		return &smithy.GenericAPIError{Code: code, Message: "The request failed."}
	}

	// One failed EC2 call for each branch of Error.Error, with the longest operation
	// that reaches it.
	for _, tc := range []struct {
		op  string
		err error
	}{
		{"RunInstances dry run", sdk(codeCapacity)},
		{"RunInstances dry run", sdk(codeSubnetFull)},
		{"RunInstances dry run", sdk(codeInstanceLimit)},
		{longOp, sdk(codeUnauthorized)},
		{"RunInstances", sdk(codeTokenMismatch)},
		{"RunInstances", sdk(codeTokenTerminated)},
		{longOp, &retry.MaxAttemptsError{Attempt: 3, Err: sdk("InternalError")}},
		{longOp, fmt.Errorf("operation error EC2: %w", context.DeadlineExceeded)},
		{longOp, sdk("RequestLimitExceeded")},
		{"TerminateInstances", sdk(codeInstanceIDNotFound)},
		{longOp, sdk("InvalidParameterValue")},
		{longOp, context.Canceled},
		{longOp, errors.New("failed to refresh cached credentials")},
	} {
		wantFits(t, wrap(tc.op, tc.err).Error(), tc.err.Error())
	}

	// LoadConfig. Each refused variable fails before any call.
	isolateAWSEnv(t, "")
	for _, v := range refusedVariables {
		t.Setenv(v.name, "value")
		_, err := LoadConfig(ctx)
		if err == nil {
			t.Fatalf("%s: no error", v.name)
		}
		wantFits(t, err.Error(), "")
		t.Setenv(v.name, "")
	}
	sdkText := "operation error ec2imds: GetRegion, request canceled"
	for _, m := range []string{msgReadEnv, msgRegion, msgLoad, msgCredentials} {
		wantFits(t, m+sdkText, sdkText)
	}
	wantFits(t, msgNoRegion, "")
	wantFits(t, msgEndpoint, "")
	wantFits(t, fmt.Sprintf(msgSource, "SharedConfigCredentials", ec2rolecreds.ProviderName), "")

	// Create and CheckRights.
	two := &fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return &ec2.RunInstancesOutput{Instances: []types.Instance{{}, {}}}, nil
	}}
	_, err := New(two).Create(ctx, testSpec())
	wantFits(t, err.Error(), "")
	wantFits(t, New(&fakeAPI{}).CheckRights(ctx, testSpec()).Error(), "")
	wantFits(t, New(&fakeAPI{}).CheckListRights(ctx).Error(), "")
	for _, edit := range []func(*Spec){
		func(s *Spec) { s.PoolID = "" },
		func(s *Spec) { s.InstanceType = "" },
		func(s *Spec) { s.ImageID = "" },
		func(s *Spec) { s.RootDevice = "" },
		func(s *Spec) { s.SubnetID = "" },
		func(s *Spec) { s.SecurityGroupID = "" },
		func(s *Spec) { s.UserData = make([]byte, MaxUserData+1) },
		func(s *Spec) { s.Key = strings.Repeat("d", maxClientToken+1) },
		func(s *Spec) { s.Key = strings.Repeat("d", maxClientToken-1) + "\xff" },
	} {
		spec := testSpec()
		edit(&spec)
		_, err := New(&fakeAPI{}).Create(ctx, spec)
		if err == nil {
			t.Fatal("a wrong spec: no error")
		}
		wantFits(t, err.Error(), "")
	}

	// Console.
	_, b64 := base64.StdEncoding.DecodeString("not base64")
	bad := &fakeAPI{console: func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		return &ec2.GetConsoleOutputOutput{Output: aws.String("not base64")}, nil
	}}
	_, err = New(bad).Console(ctx, instanceID)
	wantFits(t, err.Error(), b64.Error())

	// RootDevice.
	_, err = New(&fakeAPI{}).RootDevice(ctx, "")
	wantFits(t, err.Error(), "")
	missing := sdk(codeImageNotFound)
	_, err = New(&fakeAPI{image: func(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) { return nil, missing }}).RootDevice(ctx, imageID)
	wantFits(t, err.Error(), missing.Error())
	noRoot := &fakeAPI{image: func(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
		return &ec2.DescribeImagesOutput{Images: []types.Image{{ImageId: aws.String(imageID)}}}, nil
	}}
	_, err = New(noRoot).RootDevice(ctx, imageID)
	wantFits(t, err.Error(), "")
	_, err = New(&fakeAPI{}).RootDevice(ctx, imageID)
	wantFits(t, err.Error(), "")
}
