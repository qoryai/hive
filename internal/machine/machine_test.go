package machine

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// instance returns an EC2 instance with id, state, client token and the run's tags.
func instance(id, state, token string) types.Instance {
	return types.Instance{
		InstanceId:  aws.String(id),
		State:       &types.InstanceState{Name: types.InstanceStateName(state)},
		ClientToken: aws.String(token),
		LaunchTime:  aws.Time(time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)),
		Tags: []types.Tag{
			{Key: aws.String(TagManaged), Value: aws.String("true")},
			{Key: aws.String(TagPool), Value: aws.String("pool-7")},
			{Key: aws.String(TagRun), Value: aws.String("run-41")},
		},
	}
}

// TestCreateInput pins every field of the RunInstances input that Create sends.
func TestCreateInput(t *testing.T) {
	f := &fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return &ec2.RunInstancesOutput{Instances: []types.Instance{instance("i-0123456789abcdef0", StatePending, testSpec().DispatchID)}}, nil
	}}
	spec := testSpec()
	m, err := New(f).Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	want := Machine{ID: "i-0123456789abcdef0", State: StatePending, DispatchID: spec.DispatchID, PoolID: "pool-7", RunID: "run-41", LaunchTime: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)}
	if m != want {
		t.Errorf("machine %+v, want %+v", m, want)
	}
	tags := []types.Tag{
		{Key: aws.String("qory:managed"), Value: aws.String("true")},
		{Key: aws.String("qory:pool"), Value: aws.String("pool-7")},
		{Key: aws.String("qory:run"), Value: aws.String("run-41")},
	}
	wantIn := &ec2.RunInstancesInput{
		ClientToken:                       aws.String(spec.DispatchID),
		MinCount:                          aws.Int32(1),
		MaxCount:                          aws.Int32(1),
		ImageId:                           aws.String("ami-0123456789abcdef0"),
		InstanceType:                      types.InstanceType("m7g.large"),
		SubnetId:                          aws.String("subnet-0123456789abcdef0"),
		SecurityGroupIds:                  []string{"sg-0123456789abcdef0"},
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorStop,
		MetadataOptions: &types.InstanceMetadataOptionsRequest{
			HttpTokens:              types.HttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1),
			HttpEndpoint:            types.InstanceMetadataEndpointStateEnabled,
		},
		BlockDeviceMappings: []types.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/sda1"),
			Ebs:        &types.EbsBlockDevice{Encrypted: aws.Bool(true)},
		}},
		TagSpecifications: []types.TagSpecification{
			{ResourceType: types.ResourceTypeInstance, Tags: tags},
			{ResourceType: types.ResourceTypeVolume, Tags: tags},
		},
		UserData: aws.String(base64.StdEncoding.EncodeToString(spec.UserData)),
	}
	if len(f.runs) != 1 {
		t.Fatalf("%d RunInstances calls, want 1", len(f.runs))
	}
	if !reflect.DeepEqual(f.runs[0], wantIn) {
		t.Errorf("input\n%+v\nwant\n%+v", f.runs[0], wantIn)
	}
	if f.runs[0].IamInstanceProfile != nil || f.runs[0].KeyName != nil {
		t.Error("the input has an instance profile or a key pair")
	}
}

// TestCreateIsRepeatable pins that two Creates with one spec send the same input, so
// EC2 matches the client token to one machine.
func TestCreateIsRepeatable(t *testing.T) {
	f := &fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return &ec2.RunInstancesOutput{Instances: []types.Instance{instance("i-0123456789abcdef0", StatePending, "x")}}, nil
	}}
	c := New(f)
	for range 2 {
		if _, err := c.Create(context.Background(), testSpec()); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(f.runs[0], f.runs[1]) {
		t.Errorf("the second input differs:\n%+v\n%+v", f.runs[0], f.runs[1])
	}
}

// TestCreateUserDataBound pins that 16384 bytes of user data are sent and 16385 bytes
// are refused before any call.
func TestCreateUserDataBound(t *testing.T) {
	f := &fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return &ec2.RunInstancesOutput{Instances: []types.Instance{instance("i-0123456789abcdef0", StatePending, "x")}}, nil
	}}
	c := New(f)
	spec := testSpec()
	spec.UserData = []byte(strings.Repeat("a", MaxUserData))
	if _, err := c.Create(context.Background(), spec); err != nil {
		t.Fatalf("16384 bytes: %v", err)
	}
	spec.UserData = []byte(strings.Repeat("a", MaxUserData+1))
	_, err := c.Create(context.Background(), spec)
	const want = "Report this defect in the provisioner: the user data has 16385 bytes, and EC2 accepts at most 16384"
	if err == nil || err.Error() != want {
		t.Errorf("16385 bytes: error %v, want %q", err, want)
	}
	if len(f.runs) != 1 {
		t.Errorf("%d RunInstances calls, want 1", len(f.runs))
	}
}

// TestCreateRefusesSpec pins each spec error, word for word, with no call.
func TestCreateRefusesSpec(t *testing.T) {
	cases := []struct {
		edit func(*Spec)
		want string
	}{
		{func(s *Spec) { s.DispatchID = "" }, "Report this defect in the provisioner: the dispatch id has 0 bytes, and a client token has 1 to 64 characters"},
		{func(s *Spec) { s.DispatchID = strings.Repeat("d", 65) }, "Report this defect in the provisioner: the dispatch id has 65 bytes, and a client token has 1 to 64 characters"},
		{func(s *Spec) { s.DispatchID = "dsp 1" }, "Report this defect in the provisioner: byte 3 of the dispatch id is 0x20, and a client token has only ASCII letters, digits and punctuation"},
		{func(s *Spec) { s.RunID = "" }, "Report this defect in the provisioner: the machine spec has no run id"},
		{func(s *Spec) { s.PoolID = "" }, "Report this defect in the provisioner: the machine spec has no pool id"},
		{func(s *Spec) { s.InstanceType = "" }, "Report this defect in the provisioner: the machine spec has no instance type"},
		{func(s *Spec) { s.ImageID = "" }, "Report this defect in the provisioner: the machine spec has no image id"},
		{func(s *Spec) { s.RootDevice = "" }, "Report this defect in the provisioner: the machine spec has no root device name"},
		{func(s *Spec) { s.SubnetID = "" }, "Report this defect in the provisioner: the machine spec has no subnet id"},
		{func(s *Spec) { s.SecurityGroupID = "" }, "Report this defect in the provisioner: the machine spec has no security group id"},
	}
	for _, tc := range cases {
		f := &fakeAPI{}
		spec := testSpec()
		tc.edit(&spec)
		_, err := New(f).Create(context.Background(), spec)
		if err == nil || err.Error() != tc.want {
			t.Errorf("error %v, want %q", err, tc.want)
		}
		if len(f.runs) != 0 {
			t.Errorf("%q: %d RunInstances calls, want 0", tc.want, len(f.runs))
		}
	}
	spec := testSpec()
	spec.DispatchID = strings.Repeat("d", 64)
	if _, err := New(&fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return &ec2.RunInstancesOutput{Instances: []types.Instance{{}}}, nil
	}}).Create(context.Background(), spec); err != nil {
		t.Errorf("a 64-character id: %v", err)
	}
}

// TestCreateCountMismatch pins the error when RunInstances returns no instance.
func TestCreateCountMismatch(t *testing.T) {
	_, err := New(&fakeAPI{}).Create(context.Background(), testSpec())
	const want = "If this repeats, check EC2's service health in this region: RunInstances for client token dsp-0f3a9c2e-1b7d-4e55-8a10-6c2d9e4b7a01 returned 0 instances, not 1"
	if err == nil || err.Error() != want {
		t.Errorf("error %v, want %q", err, want)
	}
}

// TestCreateErrorKinds pins the kind and the text of each class of RunInstances
// error.
func TestCreateErrorKinds(t *testing.T) {
	cases := []struct {
		err  error
		kind Kind
		want string
	}{
		{apiError("InstanceLimitExceeded"), KindLimit, "Wait until machines end, or raise the account's EC2 instance or vCPU quota: RunInstances failed with InstanceLimitExceeded: api error InstanceLimitExceeded: test message for InstanceLimitExceeded"},
		{apiError("VcpuLimitExceeded"), KindLimit, "Wait until machines end, or raise the account's EC2 instance or vCPU quota: RunInstances failed with VcpuLimitExceeded: api error VcpuLimitExceeded: test message for VcpuLimitExceeded"},
		{apiError("IdempotentParameterMismatch"), KindTokenMismatch, "Report this defect in the provisioner: it sent RunInstances for one dispatch twice with different parameters, and EC2 returned IdempotentParameterMismatch: api error IdempotentParameterMismatch: test message for IdempotentParameterMismatch"},
		{apiError("IdempotentInstanceTerminated"), KindTokenTerminated, "No action needed: the machine started with this client token is terminated, and EC2 starts no second machine for the token: api error IdempotentInstanceTerminated: test message for IdempotentInstanceTerminated"},
		{apiError("RequestLimitExceeded"), KindRetried, "If this repeats, check EC2's service health in this region and the account's EC2 request rate: RunInstances failed with an error that the SDK retries: api error RequestLimitExceeded: test message for RequestLimitExceeded"},
		{&retry.MaxAttemptsError{Attempt: 3, Err: apiError("InsufficientInstanceCapacity")}, KindRetried, "If this repeats, check EC2's service health in this region and the account's EC2 request rate: RunInstances failed after the SDK's 3 attempts: exceeded maximum number of attempts, 3, api error InsufficientInstanceCapacity: test message for InsufficientInstanceCapacity"},
		{apiError("InvalidAMIID.NotFound"), KindOther, "Look up EC2 error code InvalidAMIID.NotFound in the EC2 API reference, and check the provisioner's settings and the instance role against it: RunInstances failed: api error InvalidAMIID.NotFound: test message for InvalidAMIID.NotFound"},
		{apiError("UnauthorizedOperation"), KindOther, "Give the instance role the provisioner's EC2 rights: RunInstances failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"},
		{context.Canceled, KindOther, "No action needed: RunInstances stopped when its context ended: context canceled"},
		{errors.New("failed to refresh cached credentials"), KindOther, "Check the instance role and the network path to EC2: RunInstances failed: failed to refresh cached credentials"},
	}
	for _, tc := range cases {
		f := &fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) { return nil, tc.err }}
		_, err := New(f).Create(context.Background(), testSpec())
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("%v: error %T, want *Error", tc.err, err)
		}
		if Classify(err) != tc.kind || e.Kind != tc.kind {
			t.Errorf("%v: kind %v, want %v", tc.err, Classify(err), tc.kind)
		}
		if err.Error() != tc.want {
			t.Errorf("error\n%s\nwant\n%s", err, tc.want)
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%v: the SDK's error is not in the chain", tc.err)
		}
	}
}

// TestClassifyUnwrapped pins Classify on errors that are not *Error.
func TestClassifyUnwrapped(t *testing.T) {
	cases := map[error]Kind{
		apiError("VcpuLimitExceeded"):                                KindLimit,
		fmt.Errorf("x: %w", apiError("IdempotentParameterMismatch")): KindTokenMismatch,
		apiError("EC2ThrottledException"):                            KindRetried,
		apiError("RequestTimeout"):                                   KindRetried,
		apiError("InvalidParameterValue"):                            KindOther,
		context.DeadlineExceeded:                                     KindOther,
		nil:                                                          KindOther,
	}
	for err, want := range cases {
		if got := Classify(err); got != want {
			t.Errorf("%v: kind %v, want %v", err, got, want)
		}
	}
	for k, want := range map[Kind]string{KindOther: "other", KindRetried: "retried", KindLimit: "limit", KindTokenMismatch: "token-mismatch", KindTokenTerminated: "token-terminated"} {
		if k.String() != want {
			t.Errorf("%d: %q, want %q", k, k.String(), want)
		}
	}
}

// TestCheckRights pins the dry run's input and its three outcomes.
func TestCheckRights(t *testing.T) {
	f := &fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) { return nil, apiError("DryRunOperation") }}
	spec := testSpec()
	spec.DispatchID = ""
	if err := New(f).CheckRights(context.Background(), spec); err != nil {
		t.Fatalf("DryRunOperation: %v", err)
	}
	want, err := runInput(testSpec(), true)
	if err != nil {
		t.Fatal(err)
	}
	want.ClientToken = nil
	want.DryRun = aws.Bool(true)
	if !reflect.DeepEqual(f.runs[0], want) {
		t.Errorf("dry run input\n%+v\nwant\n%+v", f.runs[0], want)
	}

	f.run = func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	err = New(f).CheckRights(context.Background(), testSpec())
	const wantDenied = "Give the instance role the provisioner's EC2 rights: RunInstances dry run failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"
	if err == nil || err.Error() != wantDenied {
		t.Errorf("UnauthorizedOperation: error %v, want %q", err, wantDenied)
	}

	f.run = nil
	err = New(f).CheckRights(context.Background(), testSpec())
	const wantSuccess = "Check the EC2 endpoint: a RunInstances dry run returned success, and EC2 returns DryRunOperation for a dry run"
	if err == nil || err.Error() != wantSuccess {
		t.Errorf("success: error %v, want %q", err, wantSuccess)
	}

	f.run = func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return nil, apiError("RequestLimitExceeded")
	}
	err = New(f).CheckRights(context.Background(), testSpec())
	if Classify(err) != KindRetried {
		t.Errorf("RequestLimitExceeded: kind %v, want retried", Classify(err))
	}

	spec.RunID = ""
	n := len(f.runs)
	if err := New(f).CheckRights(context.Background(), spec); err == nil || len(f.runs) != n {
		t.Errorf("an empty run id: error %v, %d calls", err, len(f.runs)-n)
	}
}

// TestFindByToken pins the filter, the not-found case and the choice of a machine that
// is not terminated.
func TestFindByToken(t *testing.T) {
	var reply []types.Instance
	f := &fakeAPI{describe: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: reply}}}, nil
	}}
	c := New(f)
	_, found, err := c.FindByToken(context.Background(), "dsp-1")
	if err != nil || found {
		t.Fatalf("no machine: found %v, error %v", found, err)
	}
	wantIn := &ec2.DescribeInstancesInput{
		Filters:    []types.Filter{{Name: aws.String("client-token"), Values: []string{"dsp-1"}}},
		MaxResults: aws.Int32(1000),
	}
	if !reflect.DeepEqual(f.describes[0], wantIn) {
		t.Errorf("input %+v, want %+v", f.describes[0], wantIn)
	}

	reply = []types.Instance{instance("i-0aaaaaaaaaaaaaaa0", StateTerminated, "dsp-1"), instance("i-0bbbbbbbbbbbbbbb0", StateStopped, "dsp-1")}
	m, found, err := c.FindByToken(context.Background(), "dsp-1")
	if err != nil || !found || m.ID != "i-0bbbbbbbbbbbbbbb0" || m.State != StateStopped || m.DispatchID != "dsp-1" {
		t.Errorf("machine %+v, found %v, error %v", m, found, err)
	}

	reply = reply[:1]
	m, found, err = c.FindByToken(context.Background(), "dsp-1")
	if err != nil || !found || m.State != StateTerminated {
		t.Errorf("terminated only: machine %+v, found %v, error %v", m, found, err)
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return nil, apiError("RequestLimitExceeded")
	}
	if _, _, err := c.FindByToken(context.Background(), "dsp-1"); Classify(err) != KindRetried {
		t.Errorf("error %v, want kind retried", err)
	}
}

// TestList pins the tag and state filters and that List reads every page.
func TestList(t *testing.T) {
	pages := map[string]*ec2.DescribeInstancesOutput{
		"": {
			Reservations: []types.Reservation{{Instances: []types.Instance{instance("i-0aaaaaaaaaaaaaaa0", StateStopped, "dsp-1"), instance("i-0bbbbbbbbbbbbbbb0", StateRunning, "dsp-2")}}},
			NextToken:    aws.String("page-2"),
		},
		"page-2": {
			Reservations: []types.Reservation{{Instances: []types.Instance{instance("i-0ccccccccccccccc0", StatePending, "dsp-3")}}},
		},
	}
	f := &fakeAPI{describe: func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return pages[aws.ToString(in.NextToken)], nil
	}}
	ms, err := New(f).List(context.Background(), "pool-7", StatePending, StateRunning, StateStopped)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	if want := []string{"i-0aaaaaaaaaaaaaaa0", "i-0bbbbbbbbbbbbbbb0", "i-0ccccccccccccccc0"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids %v, want %v", ids, want)
	}
	wantFilters := []types.Filter{
		{Name: aws.String("tag:qory:managed"), Values: []string{"true"}},
		{Name: aws.String("tag:qory:pool"), Values: []string{"pool-7"}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopped"}},
	}
	if len(f.describes) != 2 {
		t.Fatalf("%d DescribeInstances calls, want 2", len(f.describes))
	}
	for _, in := range f.describes {
		if !reflect.DeepEqual(in.Filters, wantFilters) || aws.ToInt32(in.MaxResults) != 1000 || in.InstanceIds != nil {
			t.Errorf("input %+v", in)
		}
	}

	f.describes = nil
	if _, err := New(f).List(context.Background(), "pool-7"); err != nil {
		t.Fatal(err)
	}
	if got := f.describes[0].Filters; len(got) != 2 {
		t.Errorf("with no state: filters %+v, want the two tag filters", got)
	}
}

// TestDescribeAndTerminated pins the read by id and the confirmation of a terminate.
func TestDescribeAndTerminated(t *testing.T) {
	var reply *ec2.DescribeInstancesOutput
	var replyErr error
	f := &fakeAPI{describe: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) { return reply, replyErr }}
	c := New(f)
	const id = "i-0123456789abcdef0"

	reply = &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{instance(id, StateShuttingDown, "dsp-1")}}}}
	done, err := c.Terminated(context.Background(), id)
	if err != nil || done {
		t.Errorf("shutting-down: done %v, error %v", done, err)
	}
	wantIn := &ec2.DescribeInstancesInput{InstanceIds: []string{id}}
	if !reflect.DeepEqual(f.describes[0], wantIn) {
		t.Errorf("input %+v, want %+v (by id, with no tag filter)", f.describes[0], wantIn)
	}

	reply = &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: []types.Instance{{InstanceId: aws.String(id), State: &types.InstanceState{Name: types.InstanceStateNameTerminated}}}}}}
	if done, err := c.Terminated(context.Background(), id); err != nil || !done {
		t.Errorf("terminated with no tags: done %v, error %v", done, err)
	}

	reply, replyErr = nil, apiError("InvalidInstanceID.NotFound")
	if done, err := c.Terminated(context.Background(), id); err != nil || done {
		t.Errorf("NotFound: done %v, error %v", done, err)
	}
	if _, found, err := c.Describe(context.Background(), id); err != nil || found {
		t.Errorf("NotFound: found %v, error %v", found, err)
	}

	replyErr = apiError("InternalError")
	if _, err := c.Terminated(context.Background(), id); errorCode(err) != "InternalError" {
		t.Errorf("InternalError: error %v", err)
	}

	if err := c.Terminate(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if want := (&ec2.TerminateInstancesInput{InstanceIds: []string{id}}); !reflect.DeepEqual(f.kills[0], want) {
		t.Errorf("terminate input %+v, want %+v", f.kills[0], want)
	}
	f.terminate = func(*ec2.TerminateInstancesInput) (*ec2.TerminateInstancesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	err = c.Terminate(context.Background(), id)
	const want = "Give the instance role the provisioner's EC2 rights: TerminateInstances failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"
	if err == nil || err.Error() != want {
		t.Errorf("error %v, want %q", err, want)
	}
}

// TestConsole pins the input, the decoding and the empty output.
func TestConsole(t *testing.T) {
	const text = "boot\r\nqory-machine-exit status=0 opened=yes\r\n"
	out := aws.String(base64.StdEncoding.EncodeToString([]byte(text)))
	f := &fakeAPI{console: func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		return &ec2.GetConsoleOutputOutput{Output: out}, nil
	}}
	c := New(f)
	const id = "i-0123456789abcdef0"
	got, err := c.Console(context.Background(), id)
	if err != nil || got != text {
		t.Errorf("text %q, error %v", got, err)
	}
	if want := (&ec2.GetConsoleOutputInput{InstanceId: aws.String(id), Latest: aws.Bool(true)}); !reflect.DeepEqual(f.consoles[0], want) {
		t.Errorf("input %+v, want %+v", f.consoles[0], want)
	}

	out = nil
	if got, err := c.Console(context.Background(), id); err != nil || got != "" {
		t.Errorf("no output: text %q, error %v", got, err)
	}

	out = aws.String("not base64!")
	_, err = c.Console(context.Background(), id)
	const want = "If this repeats, check EC2's service health in this region: GetConsoleOutput returned output for i-0123456789abcdef0 that is not base64: illegal base64 data at input byte 3"
	if err == nil || err.Error() != want {
		t.Errorf("error %v, want %q", err, want)
	}

	f.console = func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		return nil, apiError("InvalidInstanceID.NotFound")
	}
	if _, err := c.Console(context.Background(), id); errorCode(err) != "InvalidInstanceID.NotFound" {
		t.Errorf("error %v", err)
	}
}

// TestRootDevice pins the DescribeImages input, the root device name, and the errors
// for an empty id, a missing image and an image with no root device name.
func TestRootDevice(t *testing.T) {
	const id = "ami-0123456789abcdef0"
	var reply *ec2.DescribeImagesOutput
	var replyErr error
	f := &fakeAPI{image: func(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) { return reply, replyErr }}
	c := New(f)

	reply = &ec2.DescribeImagesOutput{Images: []types.Image{{ImageId: aws.String(id), RootDeviceName: aws.String("/dev/sda1")}}}
	name, err := c.RootDevice(context.Background(), id)
	if err != nil || name != "/dev/sda1" {
		t.Errorf("name %q, error %v", name, err)
	}
	if want := (&ec2.DescribeImagesInput{ImageIds: []string{id}}); !reflect.DeepEqual(f.images[0], want) {
		t.Errorf("input %+v, want %+v", f.images[0], want)
	}

	cases := []struct {
		reply *ec2.DescribeImagesOutput
		err   error
		want  string
	}{
		{&ec2.DescribeImagesOutput{}, nil, "Use an image id that exists in this region: DescribeImages returned no image ami-0123456789abcdef0"},
		{&ec2.DescribeImagesOutput{Images: []types.Image{{ImageId: aws.String("ami-0fedcba9876543210"), RootDeviceName: aws.String("/dev/xvda")}}}, nil, "Use an image id that exists in this region: DescribeImages returned no image ami-0123456789abcdef0"},
		{nil, apiError("InvalidAMIID.NotFound"), "Use an image id that exists in this region: DescribeImages returned no image ami-0123456789abcdef0: api error InvalidAMIID.NotFound: test message for InvalidAMIID.NotFound"},
		{&ec2.DescribeImagesOutput{Images: []types.Image{{ImageId: aws.String(id)}}}, nil, "Use an image with a root device: image ami-0123456789abcdef0 has no root device name"},
		{nil, apiError("UnauthorizedOperation"), "Give the instance role the provisioner's EC2 rights: DescribeImages failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"},
	}
	for _, tc := range cases {
		reply, replyErr = tc.reply, tc.err
		_, err := c.RootDevice(context.Background(), id)
		if err == nil || err.Error() != tc.want {
			t.Errorf("error %v, want %q", err, tc.want)
		}
	}

	n := len(f.images)
	_, err = c.RootDevice(context.Background(), "")
	const wantEmpty = "Report this defect in the provisioner: the root device lookup has no image id"
	if err == nil || err.Error() != wantEmpty || len(f.images) != n {
		t.Errorf("empty id: error %v, want %q with no call", err, wantEmpty)
	}
}

// TestCreateErrorsHideUserData pins that no Create error contains the user data or
// its base64, for each error class and for user data over the bound.
func TestCreateErrorsHideUserData(t *testing.T) {
	const marker = "run-code-marker-4f2a9e"
	data := []byte(`{"version": 1, "run_code": "` + marker + `"}`)
	encoded := base64.StdEncoding.EncodeToString(data)
	check := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("no error")
		}
		if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), encoded) {
			t.Errorf("the error contains the user data: %v", err)
		}
	}
	for _, code := range []string{"VcpuLimitExceeded", "IdempotentParameterMismatch", "IdempotentInstanceTerminated", "RequestLimitExceeded", "UnauthorizedOperation", "InvalidParameterValue"} {
		f := &fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) { return nil, apiError(code) }}
		spec := testSpec()
		spec.UserData = data
		_, err := New(f).Create(context.Background(), spec)
		check(err)
	}
	spec := testSpec()
	spec.UserData = []byte(marker + strings.Repeat("a", MaxUserData))
	_, err := New(&fakeAPI{}).Create(context.Background(), spec)
	check(err)

	c, _ := newWireClient(t, reply{400, errorXML("InvalidParameterValue")})
	spec = testSpec()
	spec.UserData = data
	_, err = c.Create(context.Background(), spec)
	check(err)
}
