package awsec2

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/machine"
	"github.com/qoryai/integrations/contracts"
)

// Neutral example values of the requests.
const (
	testKey  = "dp_0123456789abcdef"
	testPool = "np_0123456789abcdef"
	testID   = "i-0123456789abcdef0"
)

// serve runs command through adapterio.Serve with h, the settings file path and the
// request, and returns the exit status and standard output.
func serve(t *testing.T, h *Handler, command, path, request string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	status := adapterio.Serve(context.Background(), []string{command, "--settings", path}, strings.NewReader(request), &out, h)
	return status, out.String()
}

// wantFailure fails t unless the command exited with status 1 and wrote the error
// document of kind and message, cut to its first 200 characters.
func wantFailure(t *testing.T, status int, out, kind, message string) {
	t.Helper()
	if r := []rune(message); len(r) > adapterio.MaxMessage {
		message = string(r[:adapterio.MaxMessage])
	}
	if status != adapterio.ExitFailure {
		t.Fatalf("exit status %d, want 1; output %s", status, out)
	}
	doc, err := adapterio.DecodeError([]byte(strings.TrimSuffix(out, "\n")))
	if err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if doc.Error.Kind != kind || doc.Error.Message != message {
		t.Errorf("error %s: %s\nwant %s: %s", doc.Error.Kind, doc.Error.Message, kind, message)
	}
}

// makeRequest is a make request of size with the start data data.
func makeRequest(size string, data []byte) string {
	return `{"version": 1, "key": "` + testKey + `", "pool": "` + testPool + `", "size": "` + size + `", "start_data": "` + base64.StdEncoding.EncodeToString(data) + `"}`
}

// image answers DescribeImages with the image of validSettings and its root device.
func image(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: []types.Image{{ImageId: aws.String("ami-0123456789abcdef0"), RootDeviceName: aws.String("/dev/xvda")}}}, nil
}

// launched returns an instance with id, state, the qory:key tag key and a launch time
// in UTC+2 with half a second.
func launched(id, state, key string) types.Instance {
	return types.Instance{
		InstanceId: aws.String(id),
		State:      &types.InstanceState{Name: types.InstanceStateName(state)},
		LaunchTime: aws.Time(time.Date(2026, 10, 22, 12, 0, 0, 500_000_000, time.FixedZone("UTC+2", 2*3600))),
		Tags: []types.Tag{
			{Key: aws.String("qory:managed"), Value: aws.String("true")},
			{Key: aws.String("qory:pool"), Value: aws.String(testPool)},
			{Key: aws.String("qory:key"), Value: aws.String(key)},
		},
	}
}

// TestCheck pins check: one DescribeImages for the image, then one RunInstances dry
// run for each instance type of sizes, in order, with placeholder tags in the shapes
// of a pool id and a key, and no machine made.
func TestCheck(t *testing.T) {
	f := &fakeAPI{
		image: image,
		run:   func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) { return nil, apiError("DryRunOperation") },
		describe: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			return nil, apiError("DryRunOperation")
		},
	}
	h, _, _ := testHandler(f)
	path := writeSettings(t, `{"subnet_id": "subnet-0123456789abcdef0", "security_group_id": "sg-0123456789abcdef0", "image_id": "ami-0123456789abcdef0",
		"sizes": {"standard": "t3.medium", "large": "m7g.large", "medium": "t3.medium"}}`)
	status, out := serve(t, h, "check", path, `{"version": 1}`)
	if status != adapterio.ExitOK || out != "{}\n" {
		t.Fatalf("exit status %d, output %q", status, out)
	}
	if len(f.images) != 1 || !reflect.DeepEqual(f.images[0].ImageIds, []string{"ami-0123456789abcdef0"}) {
		t.Errorf("DescribeImages calls %+v", f.images)
	}
	var got []string
	for _, in := range f.runs {
		if !aws.ToBool(in.DryRun) {
			t.Error("check sent RunInstances without DryRun")
		}
		if in.ClientToken != nil {
			t.Errorf("the dry run has the client token %q", aws.ToString(in.ClientToken))
		}
		got = append(got, string(in.InstanceType))
		tags := in.TagSpecifications[0].Tags
		if aws.ToString(tags[1].Value) != "np_0000000000000000" || aws.ToString(tags[2].Value) != "dp_0000000000000000" {
			t.Errorf("tags %+v", tags)
		}
		if aws.ToString(in.BlockDeviceMappings[0].DeviceName) != "/dev/xvda" || in.UserData != nil {
			t.Errorf("input %+v", in)
		}
	}
	if want := []string{"m7g.large", "t3.medium"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dry runs for %v, want %v", got, want)
	}
	if want := []*ec2.DescribeInstancesInput{{DryRun: aws.Bool(true)}}; !reflect.DeepEqual(f.describes, want) {
		t.Errorf("DescribeInstances calls %+v, want one dry run", f.describes)
	}
	if len(f.kills) != 0 || len(f.consoles) != 0 {
		t.Errorf("%d TerminateInstances and %d GetConsoleOutput calls, want none", len(f.kills), len(f.consoles))
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	status, out = serve(t, h, "check", path, `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Give the instance role the EC2 rights that the EC2 machine adapter needs: DescribeInstances dry run failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation")

	f.run = func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	status, out = serve(t, h, "check", path, `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Give the instance role the EC2 rights that the EC2 machine adapter needs: RunInstances dry run failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation")
}

// TestCheckStopsEarly pins that check makes no EC2 call when the settings or the
// credentials fail, and writes their errors.
func TestCheckStopsEarly(t *testing.T) {
	f := &fakeAPI{}
	h, _, built := testHandler(f)
	status, out := serve(t, h, "check", writeSettings(t, `{}`), `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Add the missing members to aws-ec2.json: subnet_id, security_group_id, image_id, sizes.")
	if *built != 0 || f.calls() != 0 {
		t.Errorf("%d clients built and %d EC2 calls, want none", *built, f.calls())
	}

	h.client = func(context.Context) (*machine.Client, error) {
		return nil, &adapterio.Failure{Kind: contracts.FailureDefect, Message: "Set AWS_REGION: the environment has no region, and the instance metadata returned none"}
	}
	status, out = serve(t, h, "check", writeSettings(t, validSettings), `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Set AWS_REGION: the environment has no region, and the instance metadata returned none")
	if f.calls() != 0 {
		t.Errorf("%d EC2 calls, want none", f.calls())
	}

	f.image = func(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
		return nil, apiError("InvalidAMIID.NotFound")
	}
	h, _, _ = testHandler(f)
	status, out = serve(t, h, "check", writeSettings(t, validSettings), `{"version": 1}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Set image_id in aws-ec2.json to an image that exists in this region: DescribeImages returned no image ami-0123456789abcdef0: api error InvalidAMIID.NotFound: test message for InvalidAMIID.NotFound")
	if len(f.runs) != 0 {
		t.Errorf("%d RunInstances calls, want none", len(f.runs))
	}
}

// TestMake pins make: the instance type of the size, the start data as base64 user
// data, the key as the client token and the qory:key tag, the pool as the qory:pool
// tag, and the id of the instance in the answer.
func TestMake(t *testing.T) {
	f := &fakeAPI{image: image, run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return &ec2.RunInstancesOutput{Instances: []types.Instance{launched(testID, "pending", testKey)}}, nil
	}}
	h, _, _ := testHandler(f)
	path := writeSettings(t, validSettings)
	data := []byte("#!/bin/sh\necho start\n")
	for range 2 {
		status, out := serve(t, h, "make", path, makeRequest("large", data))
		if status != adapterio.ExitOK || out != `{"id":"`+testID+`"}`+"\n" {
			t.Fatalf("exit status %d, output %q", status, out)
		}
	}
	if len(f.runs) != 2 || !reflect.DeepEqual(f.runs[0], f.runs[1]) {
		t.Fatalf("RunInstances calls %d, or the second input differs", len(f.runs))
	}
	in := f.runs[0]
	if aws.ToString(in.ClientToken) != testKey || in.DryRun != nil || in.InstanceType != "m7g.large" {
		t.Errorf("client token %q, dry run %v, instance type %q", aws.ToString(in.ClientToken), in.DryRun, in.InstanceType)
	}
	if got, err := base64.StdEncoding.DecodeString(aws.ToString(in.UserData)); err != nil || !bytes.Equal(got, data) {
		t.Errorf("user data %q, error %v", got, err)
	}
	wantTags := []types.Tag{
		{Key: aws.String("qory:managed"), Value: aws.String("true")},
		{Key: aws.String("qory:pool"), Value: aws.String(testPool)},
		{Key: aws.String("qory:key"), Value: aws.String(testKey)},
	}
	for _, ts := range in.TagSpecifications {
		if !reflect.DeepEqual(ts.Tags, wantTags) {
			t.Errorf("%s tags %+v", ts.ResourceType, ts.Tags)
		}
	}
	ni := in.NetworkInterfaces
	if len(ni) != 1 || aws.ToBool(ni[0].AssociatePublicIpAddress) || ni[0].AssociatePublicIpAddress == nil ||
		aws.ToString(ni[0].SubnetId) != "subnet-0123456789abcdef0" || !reflect.DeepEqual(ni[0].Groups, []string{"sg-0123456789abcdef0"}) ||
		in.SubnetId != nil || in.SecurityGroupIds != nil {
		t.Errorf("network interfaces %+v, subnet %v, groups %v", ni, in.SubnetId, in.SecurityGroupIds)
	}
	mo := in.MetadataOptions
	if mo.HttpTokens != types.HttpTokensStateRequired || mo.HttpEndpoint != types.InstanceMetadataEndpointStateEnabled || aws.ToInt32(mo.HttpPutResponseHopLimit) != 1 {
		t.Errorf("metadata options %+v", mo)
	}
	if in.InstanceInitiatedShutdownBehavior != types.ShutdownBehaviorStop || aws.ToString(in.BlockDeviceMappings[0].DeviceName) != "/dev/xvda" {
		t.Errorf("shutdown %q, root device %q", in.InstanceInitiatedShutdownBehavior, aws.ToString(in.BlockDeviceMappings[0].DeviceName))
	}
}

// TestMakeUnknownSize pins that a size that sizes does not contain fails with the
// kind defect, before any EC2 call.
func TestMakeUnknownSize(t *testing.T) {
	f := &fakeAPI{}
	h, _, built := testHandler(f)
	status, out := serve(t, h, "make", writeSettings(t, validSettings), makeRequest("huge", []byte("x")))
	wantFailure(t, status, out, contracts.FailureDefect, "Add the size huge to sizes in aws-ec2.json: the make request has a size that sizes does not contain")
	if *built != 0 || f.calls() != 0 {
		t.Errorf("%d clients built and %d EC2 calls, want none", *built, f.calls())
	}
}

// TestMakeTerminated pins make for a key whose machine is terminated: EC2 returns
// IdempotentInstanceTerminated, and make returns the id that DescribeInstances returns
// for the client token, or the error when EC2 no longer returns the machine.
func TestMakeTerminated(t *testing.T) {
	var found []types.Instance
	f := &fakeAPI{
		image: image,
		run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
			return nil, apiError("IdempotentInstanceTerminated")
		},
		describe: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: found}}}, nil
		},
	}
	h, _, _ := testHandler(f)
	path := writeSettings(t, validSettings)
	found = []types.Instance{launched(testID, "terminated", testKey)}
	status, out := serve(t, h, "make", path, makeRequest("standard", []byte("x")))
	if status != adapterio.ExitOK || out != `{"id":"`+testID+`"}`+"\n" {
		t.Fatalf("exit status %d, output %q", status, out)
	}
	if want := []types.Filter{{Name: aws.String("client-token"), Values: []string{testKey}}}; !reflect.DeepEqual(f.describes[0].Filters, want) {
		t.Errorf("filters %+v", f.describes[0].Filters)
	}

	found = nil
	status, out = serve(t, h, "make", path, makeRequest("standard", []byte("x")))
	wantFailure(t, status, out, contracts.FailureOther, "No action needed: the machine made for this key is terminated, and EC2 makes no second machine for the key: api error IdempotentInstanceTerminated: test message for IdempotentInstanceTerminated")
}

// TestMakeErrorKinds pins the kind of each class of RunInstances error in the error
// document, and that no message contains the start data, its base64 or the run code
// in it.
func TestMakeErrorKinds(t *testing.T) {
	const runCode = "qecr_runcodemarker0c9d"
	data := []byte(`{"version": 1, "run_code": "` + runCode + `"}`)
	encoded := base64.StdEncoding.EncodeToString(data)
	cases := []struct {
		err  error
		kind string
	}{
		{apiError("InstanceLimitExceeded"), contracts.FailureLimit},
		{apiError("VcpuLimitExceeded"), contracts.FailureLimit},
		{apiError("InsufficientFreeAddressesInSubnet"), contracts.FailureLimit},
		{&retry.MaxAttemptsError{Attempt: 3, Err: apiError("InsufficientInstanceCapacity")}, contracts.FailureLimit},
		{apiError("RequestLimitExceeded"), contracts.FailureRetry},
		{&retry.MaxAttemptsError{Attempt: 3, Err: apiError("InternalError")}, contracts.FailureRetry},
		{context.DeadlineExceeded, contracts.FailureRetry},
		{apiError("UnauthorizedOperation"), contracts.FailureDefect},
		{apiError("InvalidParameterValue"), contracts.FailureDefect},
		{apiError("IdempotentParameterMismatch"), contracts.FailureDefect},
		{apiError("AuthFailure"), contracts.FailureOther},
		{errors.New("unexpected end of the answer"), contracts.FailureOther},
	}
	for _, tc := range cases {
		f := &fakeAPI{image: image, run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) { return nil, tc.err }}
		h, log, _ := testHandler(f)
		status, out := serve(t, h, "make", writeSettings(t, validSettings), makeRequest("standard", data))
		if status != adapterio.ExitFailure {
			t.Fatalf("%v: exit status %d", tc.err, status)
		}
		doc, err := adapterio.DecodeError([]byte(strings.TrimSuffix(out, "\n")))
		if err != nil || doc.Error.Kind != tc.kind {
			t.Errorf("%v: document %+v, error %v, want kind %s", tc.err, doc, err, tc.kind)
		}
		for _, s := range []string{out, log.String()} {
			if strings.Contains(s, runCode) || strings.Contains(s, encoded) {
				t.Errorf("%v: the output contains the start data: %s", tc.err, s)
			}
		}
	}
}

// TestList pins list: the tag and state filters, the state of each EC2 state,
// terminated left out, the key from the qory:key tag, the start time in UTC, and an
// instance whose qory:key tag is not a key left out with a log line.
func TestList(t *testing.T) {
	f := &fakeAPI{describe: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{
			launched("i-0000000000000000a", "pending", "dp_000000000000000a"),
			launched("i-0000000000000000b", "running", "dp_000000000000000b"),
			launched("i-0000000000000000c", "stopping", "dp_000000000000000c"),
			launched("i-0000000000000000d", "stopped", "dp_000000000000000d"),
			launched("i-0000000000000000e", "shutting-down", "dp_000000000000000e"),
			launched("i-0000000000000000f", "terminated", "dp_000000000000000f"),
			launched("i-0000000000000001a", "running", "run-41"),
		}}}}, nil
	}}
	h, log, _ := testHandler(f)
	status, out := serve(t, h, "list", "/nonexistent/aws-ec2.json", `{"version": 1, "pool": "`+testPool+`"}`)
	if status != adapterio.ExitOK {
		t.Fatalf("exit status %d, output %s", status, out)
	}
	const started = `"started":"2026-10-22T10:00:00Z"`
	want := `{"machines":[` +
		`{"id":"i-0000000000000000a","key":"dp_000000000000000a","state":"starting",` + started + `},` +
		`{"id":"i-0000000000000000b","key":"dp_000000000000000b","state":"running",` + started + `},` +
		`{"id":"i-0000000000000000c","key":"dp_000000000000000c","state":"running",` + started + `},` +
		`{"id":"i-0000000000000000d","key":"dp_000000000000000d","state":"stopped",` + started + `},` +
		`{"id":"i-0000000000000000e","key":"dp_000000000000000e","state":"deleting",` + started + `}]}` + "\n"
	if out != want {
		t.Errorf("output\n%s\nwant\n%s", out, want)
	}
	wantFilters := []types.Filter{
		{Name: aws.String("tag:qory:managed"), Values: []string{"true"}},
		{Name: aws.String("tag:qory:pool"), Values: []string{testPool}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped", "shutting-down"}},
	}
	if !reflect.DeepEqual(f.describes[0].Filters, wantFilters) {
		t.Errorf("filters %+v", f.describes[0].Filters)
	}
	const line = `level=WARN msg="Set the instance's qory:key tag to the key of its make request, or remove its qory:managed tag: list leaves out an instance whose qory:key tag is not a key" instance=i-0000000000000001a`
	if !strings.Contains(log.String(), line) || strings.Count(log.String(), "\n") != 1 {
		t.Errorf("log\n%s\nwant the line\n%s", log, line)
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	if status, out := serve(t, h, "list", "/nonexistent/aws-ec2.json", `{"version": 1, "pool": "`+testPool+`"}`); status != adapterio.ExitOK || out != `{"machines":[]}`+"\n" {
		t.Errorf("no machine: exit status %d, output %q", status, out)
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	status, out = serve(t, h, "list", "/nonexistent/aws-ec2.json", `{"version": 1, "pool": "`+testPool+`"}`)
	wantFailure(t, status, out, contracts.FailureDefect, "Give the instance role the EC2 rights that the EC2 machine adapter needs: DescribeInstances failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation")
}

// TestListNoLaunchTime pins that an instance that EC2 returns with no launch time is
// left out of the list answer, with one log line, so the provisioner does not take
// its start as 0001-01-01.
func TestListNoLaunchTime(t *testing.T) {
	noTime := launched("i-0000000000000002a", "running", "dp_000000000000002a")
	noTime.LaunchTime = nil
	f := &fakeAPI{describe: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{
			noTime,
			launched("i-0000000000000002b", "running", "dp_000000000000002b"),
		}}}}, nil
	}}
	h, log, _ := testHandler(f)
	status, out := serve(t, h, "list", "/nonexistent/aws-ec2.json", `{"version": 1, "pool": "`+testPool+`"}`)
	want := `{"machines":[{"id":"i-0000000000000002b","key":"dp_000000000000002b","state":"running","started":"2026-10-22T10:00:00Z"}]}` + "\n"
	if status != adapterio.ExitOK || out != want {
		t.Errorf("exit status %d, output\n%s\nwant\n%s", status, out, want)
	}
	const line = `level=WARN msg="Check the instance in EC2: list leaves out an instance that DescribeInstances returned with no launch time" instance=i-0000000000000002a`
	if !strings.Contains(log.String(), line) || strings.Count(log.String(), "\n") != 1 {
		t.Errorf("log\n%s\nwant the line\n%s", log, line)
	}
}

// TestLog pins log: GetConsoleOutput with Latest for the id, and the decoded text.
func TestLog(t *testing.T) {
	const text = "boot\r\nqory-machine-exit status=0\r\n"
	f := &fakeAPI{console: func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		return &ec2.GetConsoleOutputOutput{Output: aws.String(base64.StdEncoding.EncodeToString([]byte(text)))}, nil
	}}
	h, _, _ := testHandler(f)
	status, out := serve(t, h, "log", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
	a, err := adapterio.DecodeLogAnswer([]byte(strings.TrimSuffix(out, "\n")))
	if status != adapterio.ExitOK || err != nil || a.Log != text {
		t.Fatalf("exit status %d, output %q, error %v", status, out, err)
	}
	if want := (&ec2.GetConsoleOutputInput{InstanceId: aws.String(testID), Latest: aws.Bool(true)}); !reflect.DeepEqual(f.consoles[0], want) {
		t.Errorf("input %+v", f.consoles[0])
	}

	f.console = func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		return nil, apiError("InvalidInstanceID.NotFound")
	}
	status, out = serve(t, h, "log", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
	wantFailure(t, status, out, contracts.FailureOther, "Check that the instance exists in this region: GetConsoleOutput failed with InvalidInstanceID.NotFound, which EC2 can return right after RunInstances: api error InvalidInstanceID.NotFound: test message for InvalidInstanceID.NotFound")
}

// TestLogNotNitro pins that log returns the console text of an instance that is not
// on the Nitro system: GetConsoleOutput with Latest returns UnsupportedOperation, and
// log reads the output again without Latest.
func TestLogNotNitro(t *testing.T) {
	const text = "boot on a Xen instance\n"
	f := &fakeAPI{console: func(in *ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		if aws.ToBool(in.Latest) {
			return nil, apiError("UnsupportedOperation")
		}
		return &ec2.GetConsoleOutputOutput{Output: aws.String(base64.StdEncoding.EncodeToString([]byte(text)))}, nil
	}}
	h, _, _ := testHandler(f)
	status, out := serve(t, h, "log", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
	a, err := adapterio.DecodeLogAnswer([]byte(strings.TrimSuffix(out, "\n")))
	if status != adapterio.ExitOK || err != nil || a.Log != text {
		t.Fatalf("exit status %d, output %q, error %v", status, out, err)
	}
	if len(f.consoles) != 2 || f.consoles[1].Latest != nil {
		t.Errorf("inputs %+v, want a second call without Latest", f.consoles)
	}
}

// TestLogCut pins that log keeps the end of a long console text that fits a log
// answer: U+0000 and each run of invalid UTF-8 become U+FFFD, the text is at most
// 768 KiB, and the answer, with its line feed, at most 1 MiB.
func TestLogCut(t *testing.T) {
	for i, c := range []struct {
		console, end string
		size         int
	}{
		{strings.Repeat("a\xff", adapterio.MaxLog/2), "a\uFFFD", adapterio.MaxLog},
		{strings.Repeat("boot\x00\x1b[0m\n", adapterio.MaxLog/4) + "end", "boot\uFFFD\x1b[0m\nend", 0},
	} {
		f := &fakeAPI{console: func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
			return &ec2.GetConsoleOutputOutput{Output: aws.String(base64.StdEncoding.EncodeToString([]byte(c.console)))}, nil
		}}
		h, _, _ := testHandler(f)
		status, out := serve(t, h, "log", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
		a, err := adapterio.DecodeLogAnswer([]byte(strings.TrimSuffix(out, "\n")))
		if status != adapterio.ExitOK || err != nil || len(out) > contracts.MaxMachinesAnswer || len(a.Log) > adapterio.MaxLog || !strings.HasSuffix(a.Log, c.end) || strings.Contains(a.Log, "\x00") {
			t.Errorf("case %d: exit status %d, %d bytes of output, %d bytes of log, error %v", i, status, len(out), len(a.Log), err)
		}
		if c.size != 0 && len(a.Log) != c.size {
			t.Errorf("case %d: %d bytes of log, want %d", i, len(a.Log), c.size)
		}
		if c.size == 0 && len(out) < contracts.MaxMachinesAnswer-6 {
			t.Errorf("case %d: %d bytes of output, want the end that fits 1 MiB", i, len(out))
		}
	}
}

// TestDelete pins delete: TerminateInstances for the id; gone for an unknown id and a
// terminated instance, deleting for any other, and the kind of a failed call.
func TestDelete(t *testing.T) {
	var reply *ec2.TerminateInstancesOutput
	var replyErr error
	f := &fakeAPI{terminate: func(*ec2.TerminateInstancesInput) (*ec2.TerminateInstancesOutput, error) { return reply, replyErr }}
	h, _, _ := testHandler(f)
	change := func(previous, current types.InstanceStateName) *ec2.TerminateInstancesOutput {
		return &ec2.TerminateInstancesOutput{TerminatingInstances: []types.InstanceStateChange{{
			InstanceId:    aws.String(testID),
			PreviousState: &types.InstanceState{Name: previous},
			CurrentState:  &types.InstanceState{Name: current},
		}}}
	}
	cases := []struct {
		reply *ec2.TerminateInstancesOutput
		err   error
		state string
	}{
		{change(types.InstanceStateNameRunning, types.InstanceStateNameShuttingDown), nil, "deleting"},
		{change(types.InstanceStateNameStopped, types.InstanceStateNameShuttingDown), nil, "deleting"},
		{change(types.InstanceStateNameShuttingDown, types.InstanceStateNameShuttingDown), nil, "deleting"},
		{change(types.InstanceStateNameTerminated, types.InstanceStateNameTerminated), nil, "gone"},
		{&ec2.TerminateInstancesOutput{}, nil, "deleting"},
		{nil, apiError("InvalidInstanceID.NotFound"), "gone"},
	}
	for i, tc := range cases {
		reply, replyErr = tc.reply, tc.err
		status, out := serve(t, h, "delete", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
		if want := `{"state":"` + tc.state + `"}` + "\n"; status != adapterio.ExitOK || out != want {
			t.Errorf("case %d: exit status %d, output %q, want %q", i, status, out, want)
		}
	}
	if want := (&ec2.TerminateInstancesInput{InstanceIds: []string{testID}}); !reflect.DeepEqual(f.kills[0], want) {
		t.Errorf("input %+v", f.kills[0])
	}

	reply, replyErr = nil, apiError("RequestLimitExceeded")
	status, out := serve(t, h, "delete", "/nonexistent/aws-ec2.json", `{"version": 1, "id": "`+testID+`"}`)
	wantFailure(t, status, out, contracts.FailureRetry, "If this repeats, check EC2's service health and the account's request rate in this region: TerminateInstances failed with an error the SDK retries: api error RequestLimitExceeded: test message for RequestLimitExceeded")
}

// TestFailure pins the conversion of each kind of error to the error document.
func TestFailure(t *testing.T) {
	done := &adapterio.Failure{Kind: contracts.FailureLimit, Message: "m"}
	if got := failure(done); got != done {
		t.Errorf("a *adapterio.Failure: %#v", got)
	}
	var e *adapterio.Failure
	if !errors.As(failure(errors.New("plain")), &e) || e.Kind != contracts.FailureOther || e.Message != "plain" {
		t.Errorf("a plain error: %#v", e)
	}
}
