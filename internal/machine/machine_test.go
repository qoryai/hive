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

	"github.com/qoryai/hive/machines"
)

// kindOf returns the kind of err: the Kind of an *Error or of a *machines.Error in its
// chain, or "" for any other error.
func kindOf(err error) machines.Kind {
	var call *Error
	if errors.As(err, &call) {
		return call.Kind
	}
	var failure *machines.Error
	if errors.As(err, &failure) {
		return failure.Kind
	}
	return ""
}

// instance returns an EC2 instance with id, state, client token and the tags of
// testSpec.
func instance(id, state, token string) types.Instance {
	return types.Instance{
		InstanceId:  aws.String(id),
		State:       &types.InstanceState{Name: types.InstanceStateName(state)},
		ClientToken: aws.String(token),
		LaunchTime:  aws.Time(time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)),
		Tags: []types.Tag{
			{Key: aws.String(TagManaged), Value: aws.String("true")},
			{Key: aws.String(TagPool), Value: aws.String("np_0123456789abcdef")},
			{Key: aws.String(TagKey), Value: aws.String(token)},
		},
	}
}

// TestCreateInput pins every field of the RunInstances input that Create sends: the
// subnet and the security group only on the network interface, which has no public
// address, the IMDSv2 options, the stop on shutdown, the encrypted root volume, and
// the tags qory:managed, qory:pool and qory:key on the instance and its volume.
func TestCreateInput(t *testing.T) {
	spec := testSpec()
	f := &fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return &ec2.RunInstancesOutput{Instances: []types.Instance{instance("i-0123456789abcdef0", StatePending, spec.Key)}}, nil
	}}
	m, err := New(f).Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	want := Machine{ID: "i-0123456789abcdef0", State: StatePending, Key: spec.Key, LaunchTime: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)}
	if m != want {
		t.Errorf("machine %+v, want %+v", m, want)
	}
	tags := []types.Tag{
		{Key: aws.String("qory:managed"), Value: aws.String("true")},
		{Key: aws.String("qory:pool"), Value: aws.String("np_0123456789abcdef")},
		{Key: aws.String("qory:key"), Value: aws.String("dp_0123456789abcdef")},
	}
	wantIn := &ec2.RunInstancesInput{
		ClientToken:  aws.String("dp_0123456789abcdef"),
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		ImageId:      aws.String("ami-0123456789abcdef0"),
		InstanceType: types.InstanceType("m7g.large"),
		NetworkInterfaces: []types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex:              aws.Int32(0),
			AssociatePublicIpAddress: aws.Bool(false),
			SubnetId:                 aws.String("subnet-0123456789abcdef0"),
			Groups:                   []string{"sg-0123456789abcdef0"},
		}},
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
	if f.runs[0].IamInstanceProfile != nil || f.runs[0].KeyName != nil || f.runs[0].SubnetId != nil || f.runs[0].SecurityGroupIds != nil {
		t.Error("the input has an instance profile, a key pair, or a subnet or security group outside the network interface")
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
	const want = "Report this defect in the provisioner: the start data has 16385 bytes, and EC2 accepts at most 16384 bytes of user data"
	if err == nil || err.Error() != want || kindOf(err) != machines.KindDefect {
		t.Errorf("16385 bytes: error %v, want %q of kind defect", err, want)
	}
	if len(f.runs) != 1 {
		t.Errorf("%d RunInstances calls, want 1", len(f.runs))
	}
}

// TestCreateRefusesSpec pins each spec error, word for word and of kind defect, with
// no call.
func TestCreateRefusesSpec(t *testing.T) {
	cases := []struct {
		edit func(*Spec)
		want string
	}{
		{func(s *Spec) { s.Key = "" }, "Report this defect in the provisioner: the key has 0 bytes, and a client token has 1 to 64 characters"},
		{func(s *Spec) { s.Key = strings.Repeat("d", 65) }, "Report this defect in the provisioner: the key has 65 bytes, and a client token has 1 to 64 characters"},
		{func(s *Spec) { s.Key = "dp_ 1" }, "Report this defect in the provisioner: byte 3 of the key is 0x20, and a client token has only ASCII letters, digits and punctuation"},
		{func(s *Spec) { s.PoolID = "" }, "Fix the EC2 machine adapter: the machine spec has no pool id"},
		{func(s *Spec) { s.InstanceType = "" }, "Fix the EC2 machine adapter: the machine spec has no instance type"},
		{func(s *Spec) { s.ImageID = "" }, "Fix the EC2 machine adapter: the machine spec has no image id"},
		{func(s *Spec) { s.RootDevice = "" }, "Fix the EC2 machine adapter: the machine spec has no root device name"},
		{func(s *Spec) { s.SubnetID = "" }, "Fix the EC2 machine adapter: the machine spec has no subnet id"},
		{func(s *Spec) { s.SecurityGroupID = "" }, "Fix the EC2 machine adapter: the machine spec has no security group id"},
	}
	for _, tc := range cases {
		f := &fakeAPI{}
		spec := testSpec()
		tc.edit(&spec)
		_, err := New(f).Create(context.Background(), spec)
		if err == nil || err.Error() != tc.want || kindOf(err) != machines.KindDefect {
			t.Errorf("error %v, want %q of kind defect", err, tc.want)
		}
		if len(f.runs) != 0 {
			t.Errorf("%q: %d RunInstances calls, want 0", tc.want, len(f.runs))
		}
	}
	spec := testSpec()
	spec.Key = strings.Repeat("d", 64)
	if _, err := New(&fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return &ec2.RunInstancesOutput{Instances: []types.Instance{{}}}, nil
	}}).Create(context.Background(), spec); err != nil {
		t.Errorf("a 64-character key: %v", err)
	}
}

// TestCreateCountMismatch pins the error when RunInstances returns no instance.
func TestCreateCountMismatch(t *testing.T) {
	_, err := New(&fakeAPI{}).Create(context.Background(), testSpec())
	const want = "If this repeats, check EC2's service health in this region: RunInstances for key dp_0123456789abcdef returned 0 instances, not 1"
	if err == nil || err.Error() != want || kindOf(err) != machines.KindOther {
		t.Errorf("error %v, want %q of kind other", err, want)
	}
}

// TestCreateErrorKinds pins the kind and the text of each class of RunInstances
// error.
func TestCreateErrorKinds(t *testing.T) {
	deadline := fmt.Errorf("operation error EC2: RunInstances, %w", context.DeadlineExceeded)
	cases := []struct {
		err  error
		kind machines.Kind
		want string
	}{
		{apiError("InstanceLimitExceeded"), machines.KindLimit, "Wait until machines end, or raise the account's EC2 instance or vCPU quota: RunInstances failed with InstanceLimitExceeded: api error InstanceLimitExceeded: test message for InstanceLimitExceeded"},
		{apiError("VcpuLimitExceeded"), machines.KindLimit, "Wait until machines end, or raise the account's EC2 instance or vCPU quota: RunInstances failed with VcpuLimitExceeded: api error VcpuLimitExceeded: test message for VcpuLimitExceeded"},
		{&retry.MaxAttemptsError{Attempt: 3, Err: apiError("InsufficientInstanceCapacity")}, machines.KindLimit, "Wait, or map the size to another instance type in the EC2 adapter's settings file aws-ec2.json: RunInstances failed with InsufficientInstanceCapacity, because EC2 has no capacity for the instance type in the subnet's availability zone: exceeded maximum number of attempts, 3, api error InsufficientInstanceCapacity: test message for InsufficientInstanceCapacity"},
		{apiError("InsufficientFreeAddressesInSubnet"), machines.KindLimit, "Wait until machines end, or set subnet_id in the EC2 adapter's settings file aws-ec2.json to a subnet with free addresses: RunInstances failed with InsufficientFreeAddressesInSubnet: api error InsufficientFreeAddressesInSubnet: test message for InsufficientFreeAddressesInSubnet"},
		{apiError("IdempotentParameterMismatch"), machines.KindDefect, "Report this defect in the provisioner, unless the EC2 adapter's settings file aws-ec2.json changed after the first make for this key: RunInstances was sent twice for one key with different parameters, and EC2 returned IdempotentParameterMismatch: api error IdempotentParameterMismatch: test message for IdempotentParameterMismatch"},
		{apiError("IdempotentInstanceTerminated"), machines.KindOther, "No action needed: the machine made for this key is terminated, and EC2 makes no second machine for the key: api error IdempotentInstanceTerminated: test message for IdempotentInstanceTerminated"},
		{apiError("RequestLimitExceeded"), machines.KindRetry, "If this repeats, check EC2's service health in this region and the account's EC2 request rate: RunInstances failed with an error that the SDK retries: api error RequestLimitExceeded: test message for RequestLimitExceeded"},
		{&retry.MaxAttemptsError{Attempt: 3, Err: apiError("InternalError")}, machines.KindRetry, "If this repeats, check EC2's service health in this region and the account's EC2 request rate: RunInstances failed after the SDK's 3 attempts: exceeded maximum number of attempts, 3, api error InternalError: test message for InternalError"},
		{deadline, machines.KindRetry, "If this repeats, check the network path to EC2: RunInstances did not finish within its time limit: operation error EC2: RunInstances, context deadline exceeded"},
		{apiError("InvalidAMIID.NotFound"), machines.KindDefect, "Look up EC2 error code InvalidAMIID.NotFound in the EC2 API reference, and check the EC2 adapter's settings file aws-ec2.json and the instance role against it: RunInstances failed: api error InvalidAMIID.NotFound: test message for InvalidAMIID.NotFound"},
		{apiError("InvalidParameterValue"), machines.KindDefect, "Look up EC2 error code InvalidParameterValue in the EC2 API reference, and check the EC2 adapter's settings file aws-ec2.json and the instance role against it: RunInstances failed: api error InvalidParameterValue: test message for InvalidParameterValue"},
		{apiError("UnauthorizedOperation"), machines.KindDefect, "Give the instance role the EC2 rights that the EC2 machine adapter needs: RunInstances failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"},
		{apiError("AuthFailure"), machines.KindOther, "Look up EC2 error code AuthFailure in the EC2 API reference, and check the EC2 adapter's settings file aws-ec2.json and the instance role against it: RunInstances failed: api error AuthFailure: test message for AuthFailure"},
		{context.Canceled, machines.KindOther, "No action needed: RunInstances stopped when its context ended: context canceled"},
		{errors.New("failed to refresh cached credentials"), machines.KindOther, "Check the instance role and the network path to EC2: RunInstances failed: failed to refresh cached credentials"},
	}
	for _, tc := range cases {
		f := &fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) { return nil, tc.err }}
		_, err := New(f).Create(context.Background(), testSpec())
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("%v: error %T, want *Error", tc.err, err)
		}
		if e.Kind != tc.kind {
			t.Errorf("%v: kind %v, want %v", tc.err, e.Kind, tc.kind)
		}
		if err.Error() != tc.want {
			t.Errorf("error\n%s\nwant\n%s", err, tc.want)
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%v: the SDK's error is not in the chain", tc.err)
		}
	}
}

// TestClassify pins the kind of each class of error, wrapped or not.
func TestClassify(t *testing.T) {
	cases := map[error]machines.Kind{
		apiError("VcpuLimitExceeded"):                                machines.KindLimit,
		apiError("InsufficientInstanceCapacity"):                     machines.KindLimit,
		apiError("InsufficientFreeAddressesInSubnet"):                machines.KindLimit,
		fmt.Errorf("x: %w", apiError("IdempotentParameterMismatch")): machines.KindDefect,
		apiError("EC2ThrottledException"):                            machines.KindRetry,
		apiError("RequestTimeout"):                                   machines.KindRetry,
		apiError("InvalidParameterValue"):                            machines.KindDefect,
		apiError("InvalidParameter"):                                 machines.KindDefect,
		apiError("InvalidSubnetID.NotFound"):                         machines.KindDefect,
		apiError("MissingParameter"):                                 machines.KindDefect,
		apiError("UnknownParameter"):                                 machines.KindDefect,
		apiError("UnsupportedOperation"):                             machines.KindDefect,
		apiError("InvalidInstanceID.NotFound"):                       machines.KindOther,
		apiError("IdempotentInstanceTerminated"):                     machines.KindOther,
		context.DeadlineExceeded:                                     machines.KindRetry,
		context.Canceled:                                             machines.KindOther,
		errors.New("unknown"):                                        machines.KindOther,
	}
	for err, want := range cases {
		if got := classify(err); got != want {
			t.Errorf("%v: kind %v, want %v", err, got, want)
		}
	}
	if !TokenTerminated(fmt.Errorf("x: %w", apiError("IdempotentInstanceTerminated"))) || TokenTerminated(apiError("IdempotentParameterMismatch")) || TokenTerminated(nil) {
		t.Error("TokenTerminated matches another code")
	}
}

// TestCheckRights pins the dry run's input and its outcomes.
func TestCheckRights(t *testing.T) {
	f := &fakeAPI{run: func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) { return nil, apiError("DryRunOperation") }}
	if err := New(f).CheckRights(context.Background(), testSpec()); err != nil {
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
	const wantDenied = "Give the instance role the EC2 rights that the EC2 machine adapter needs: RunInstances dry run failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"
	if err == nil || err.Error() != wantDenied || kindOf(err) != machines.KindDefect {
		t.Errorf("UnauthorizedOperation: error %v, want %q of kind defect", err, wantDenied)
	}

	f.run = nil
	err = New(f).CheckRights(context.Background(), testSpec())
	const wantSuccess = "Check the EC2 endpoint: a RunInstances dry run returned success, and EC2 returns DryRunOperation for a dry run"
	if err == nil || err.Error() != wantSuccess || kindOf(err) != machines.KindOther {
		t.Errorf("success: error %v, want %q of kind other", err, wantSuccess)
	}

	f.run = func(*ec2.RunInstancesInput) (*ec2.RunInstancesOutput, error) {
		return nil, apiError("RequestLimitExceeded")
	}
	err = New(f).CheckRights(context.Background(), testSpec())
	if kindOf(err) != machines.KindRetry {
		t.Errorf("RequestLimitExceeded: kind %v, want retry", kindOf(err))
	}

	spec := testSpec()
	spec.PoolID = ""
	n := len(f.runs)
	if err := New(f).CheckRights(context.Background(), spec); err == nil || len(f.runs) != n {
		t.Errorf("an empty pool id: error %v, %d calls", err, len(f.runs)-n)
	}
}

// TestCheckListRights pins the DescribeInstances dry run's input and its outcomes.
func TestCheckListRights(t *testing.T) {
	f := &fakeAPI{describe: func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return nil, apiError("DryRunOperation")
	}}
	if err := New(f).CheckListRights(context.Background()); err != nil {
		t.Fatalf("DryRunOperation: %v", err)
	}
	if want := (&ec2.DescribeInstancesInput{DryRun: aws.Bool(true)}); !reflect.DeepEqual(f.describes[0], want) {
		t.Errorf("input %+v, want %+v", f.describes[0], want)
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return nil, apiError("UnauthorizedOperation")
	}
	err := New(f).CheckListRights(context.Background())
	const wantDenied = "Give the instance role the EC2 rights that the EC2 machine adapter needs: DescribeInstances dry run failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"
	if err == nil || err.Error() != wantDenied || kindOf(err) != machines.KindDefect {
		t.Errorf("UnauthorizedOperation: error %v, want %q of kind defect", err, wantDenied)
	}

	f.describe = nil
	err = New(f).CheckListRights(context.Background())
	const wantSuccess = "Check the EC2 endpoint: a DescribeInstances dry run returned success, and EC2 returns DryRunOperation for a dry run"
	if err == nil || err.Error() != wantSuccess || kindOf(err) != machines.KindOther {
		t.Errorf("success: error %v, want %q of kind other", err, wantSuccess)
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
	const key = "dp_0123456789abcdef"
	_, found, err := c.FindByToken(context.Background(), key)
	if err != nil || found {
		t.Fatalf("no machine: found %v, error %v", found, err)
	}
	wantIn := &ec2.DescribeInstancesInput{
		Filters:    []types.Filter{{Name: aws.String("client-token"), Values: []string{key}}},
		MaxResults: aws.Int32(1000),
	}
	if !reflect.DeepEqual(f.describes[0], wantIn) {
		t.Errorf("input %+v, want %+v", f.describes[0], wantIn)
	}

	reply = []types.Instance{instance("i-0aaaaaaaaaaaaaaa0", StateTerminated, key), instance("i-0bbbbbbbbbbbbbbb0", StateStopped, key)}
	m, found, err := c.FindByToken(context.Background(), key)
	if err != nil || !found || m.ID != "i-0bbbbbbbbbbbbbbb0" || m.State != StateStopped || m.Key != key {
		t.Errorf("machine %+v, found %v, error %v", m, found, err)
	}

	reply = reply[:1]
	m, found, err = c.FindByToken(context.Background(), key)
	if err != nil || !found || m.State != StateTerminated {
		t.Errorf("terminated only: machine %+v, found %v, error %v", m, found, err)
	}

	f.describe = func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return nil, apiError("RequestLimitExceeded")
	}
	if _, _, err := c.FindByToken(context.Background(), key); kindOf(err) != machines.KindRetry {
		t.Errorf("error %v, want kind retry", err)
	}
}

// TestList pins the tag and state filters and that List reads every page.
func TestList(t *testing.T) {
	pages := map[string]*ec2.DescribeInstancesOutput{
		"": {
			Reservations: []types.Reservation{{Instances: []types.Instance{instance("i-0aaaaaaaaaaaaaaa0", StateStopped, "dp_000000000000000a"), instance("i-0bbbbbbbbbbbbbbb0", StateRunning, "dp_000000000000000b")}}},
			NextToken:    aws.String("page-2"),
		},
		"page-2": {
			Reservations: []types.Reservation{{Instances: []types.Instance{instance("i-0ccccccccccccccc0", StatePending, "dp_000000000000000c")}}},
		},
	}
	f := &fakeAPI{describe: func(in *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
		return pages[aws.ToString(in.NextToken)], nil
	}}
	ms, err := New(f).List(context.Background(), "np_0123456789abcdef", StatePending, StateRunning, StateStopped)
	if err != nil {
		t.Fatal(err)
	}
	var ids, keys []string
	for _, m := range ms {
		ids = append(ids, m.ID)
		keys = append(keys, m.Key)
	}
	if want := []string{"i-0aaaaaaaaaaaaaaa0", "i-0bbbbbbbbbbbbbbb0", "i-0ccccccccccccccc0"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("ids %v, want %v", ids, want)
	}
	if want := []string{"dp_000000000000000a", "dp_000000000000000b", "dp_000000000000000c"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys %v, want %v", keys, want)
	}
	wantFilters := []types.Filter{
		{Name: aws.String("tag:qory:managed"), Values: []string{"true"}},
		{Name: aws.String("tag:qory:pool"), Values: []string{"np_0123456789abcdef"}},
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
	if _, err := New(f).List(context.Background(), "np_0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if got := f.describes[0].Filters; len(got) != 2 {
		t.Errorf("with no state: filters %+v, want the two tag filters", got)
	}
}

// TestTerminate pins the TerminateInstances input, the state after the call, the
// unknown id, and the kind of a failed call.
func TestTerminate(t *testing.T) {
	const id = "i-0123456789abcdef0"
	var reply *ec2.TerminateInstancesOutput
	var replyErr error
	f := &fakeAPI{terminate: func(*ec2.TerminateInstancesInput) (*ec2.TerminateInstancesOutput, error) { return reply, replyErr }}
	c := New(f)
	change := func(id string, previous, current types.InstanceStateName) *ec2.TerminateInstancesOutput {
		return &ec2.TerminateInstancesOutput{TerminatingInstances: []types.InstanceStateChange{{
			InstanceId:    aws.String(id),
			PreviousState: &types.InstanceState{Name: previous},
			CurrentState:  &types.InstanceState{Name: current},
		}}}
	}

	cases := []struct {
		reply *ec2.TerminateInstancesOutput
		state string
	}{
		{change(id, types.InstanceStateNameStopped, types.InstanceStateNameShuttingDown), StateShuttingDown},
		{change(id, types.InstanceStateNameTerminated, types.InstanceStateNameTerminated), StateTerminated},
		{change("i-0fedcba9876543210", types.InstanceStateNameRunning, types.InstanceStateNameShuttingDown), ""},
		{&ec2.TerminateInstancesOutput{}, ""},
	}
	for i, tc := range cases {
		reply, replyErr = tc.reply, nil
		state, found, err := c.Terminate(context.Background(), id)
		if err != nil || !found || state != tc.state {
			t.Errorf("case %d: state %q, found %v, error %v, want %q", i, state, found, err, tc.state)
		}
	}
	if want := (&ec2.TerminateInstancesInput{InstanceIds: []string{id}}); !reflect.DeepEqual(f.kills[0], want) {
		t.Errorf("terminate input %+v, want %+v", f.kills[0], want)
	}

	reply, replyErr = nil, apiError("InvalidInstanceID.NotFound")
	if state, found, err := c.Terminate(context.Background(), id); err != nil || found || state != "" {
		t.Errorf("NotFound: state %q, found %v, error %v", state, found, err)
	}

	replyErr = apiError("UnauthorizedOperation")
	_, _, err := c.Terminate(context.Background(), id)
	const want = "Give the instance role the EC2 rights that the EC2 machine adapter needs: TerminateInstances failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"
	if err == nil || err.Error() != want || kindOf(err) != machines.KindDefect {
		t.Errorf("error %v, want %q of kind defect", err, want)
	}
	replyErr = apiError("InvalidInstanceID.Malformed")
	if _, _, err := c.Terminate(context.Background(), id); kindOf(err) != machines.KindDefect {
		t.Errorf("InvalidInstanceID.Malformed: error %v, want kind defect", err)
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
	if err == nil || err.Error() != want || kindOf(err) != machines.KindOther {
		t.Errorf("error %v, want %q of kind other", err, want)
	}

	f.console = func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		return nil, apiError("InvalidInstanceID.NotFound")
	}
	if _, err := c.Console(context.Background(), id); len(f.consoles) != 4 {
		t.Errorf("NotFound: %d calls in all, want 4 with no second call, error %v", len(f.consoles), err)
	}
	if _, err := c.Console(context.Background(), id); errorCode(err) != "InvalidInstanceID.NotFound" || kindOf(err) != machines.KindOther {
		t.Errorf("error %v, want kind other", err)
	}
}

// TestConsoleNotNitro pins that Console reads the output again without Latest when
// EC2 returns UnsupportedOperation for Latest, and the error when that call fails too.
func TestConsoleNotNitro(t *testing.T) {
	const text = "boot on a Xen instance\n"
	f := &fakeAPI{console: func(in *ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		if aws.ToBool(in.Latest) {
			return nil, apiError("UnsupportedOperation")
		}
		return &ec2.GetConsoleOutputOutput{Output: aws.String(base64.StdEncoding.EncodeToString([]byte(text)))}, nil
	}}
	c := New(f)
	const id = "i-0123456789abcdef0"
	got, err := c.Console(context.Background(), id)
	if err != nil || got != text {
		t.Errorf("text %q, error %v", got, err)
	}
	want := []*ec2.GetConsoleOutputInput{
		{InstanceId: aws.String(id), Latest: aws.Bool(true)},
		{InstanceId: aws.String(id)},
	}
	if !reflect.DeepEqual(f.consoles, want) {
		t.Errorf("inputs %+v, want %+v", f.consoles, want)
	}

	f.console = func(*ec2.GetConsoleOutputInput) (*ec2.GetConsoleOutputOutput, error) {
		return nil, apiError("UnsupportedOperation")
	}
	_, err = c.Console(context.Background(), id)
	if errorCode(err) != "UnsupportedOperation" || kindOf(err) != machines.KindDefect || len(f.consoles) != 4 {
		t.Errorf("error %v, %d calls in all, want UnsupportedOperation of kind defect after 4 calls", err, len(f.consoles))
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
		{&ec2.DescribeImagesOutput{}, nil, "Set image_id in the EC2 adapter's settings file aws-ec2.json to an image that exists in this region: DescribeImages returned no image ami-0123456789abcdef0"},
		{&ec2.DescribeImagesOutput{Images: []types.Image{{ImageId: aws.String("ami-0fedcba9876543210"), RootDeviceName: aws.String("/dev/xvda")}}}, nil, "Set image_id in the EC2 adapter's settings file aws-ec2.json to an image that exists in this region: DescribeImages returned no image ami-0123456789abcdef0"},
		{nil, apiError("InvalidAMIID.NotFound"), "Set image_id in the EC2 adapter's settings file aws-ec2.json to an image that exists in this region: DescribeImages returned no image ami-0123456789abcdef0: api error InvalidAMIID.NotFound: test message for InvalidAMIID.NotFound"},
		{&ec2.DescribeImagesOutput{Images: []types.Image{{ImageId: aws.String(id)}}}, nil, "Set image_id in the EC2 adapter's settings file aws-ec2.json to an image with a root device: image ami-0123456789abcdef0 has no root device name"},
		{nil, apiError("UnauthorizedOperation"), "Give the instance role the EC2 rights that the EC2 machine adapter needs: DescribeImages failed with UnauthorizedOperation: api error UnauthorizedOperation: test message for UnauthorizedOperation"},
	}
	for _, tc := range cases {
		reply, replyErr = tc.reply, tc.err
		_, err := c.RootDevice(context.Background(), id)
		if err == nil || err.Error() != tc.want || kindOf(err) != machines.KindDefect {
			t.Errorf("error %v, want %q of kind defect", err, tc.want)
		}
	}

	n := len(f.images)
	_, err = c.RootDevice(context.Background(), "")
	const wantEmpty = "Fix the EC2 machine adapter: the root device lookup has no image id"
	if err == nil || err.Error() != wantEmpty || len(f.images) != n {
		t.Errorf("empty id: error %v, want %q with no call", err, wantEmpty)
	}
}

// TestCreateErrorsHideUserData pins that no Create error contains the user data or
// its base64, for each error class and for user data over the bound.
func TestCreateErrorsHideUserData(t *testing.T) {
	const marker = "qecr_runcodemarker4f2a9e"
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
	for _, code := range []string{"VcpuLimitExceeded", "InsufficientInstanceCapacity", "IdempotentParameterMismatch", "IdempotentInstanceTerminated", "RequestLimitExceeded", "UnauthorizedOperation", "InvalidParameterValue"} {
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
