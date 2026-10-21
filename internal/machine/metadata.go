package machine

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

// Metadata is the part of the SDK's instance metadata client that this package
// calls. *imds.Client implements it.
type Metadata interface {
	GetMetadata(ctx context.Context, in *imds.GetMetadataInput, optFns ...func(*imds.Options)) (*imds.GetMetadataOutput, error)
}

// NewMetadata returns the SDK's instance metadata client built from cfg. It calls
// IMDSv2; with AWS_EC2_METADATA_V1_DISABLED=true it makes no IMDSv1 call.
func NewMetadata(cfg aws.Config) *imds.Client {
	return imds.NewFromConfig(cfg)
}

// maxAddress bounds the read of local-ipv4: an IPv4 address has at most 15 bytes.
const maxAddress = 64

// LocalIPv4 returns the host's private IPv4 address, local-ipv4 in the instance
// metadata. Machines reach the gateway at this address.
func LocalIPv4(ctx context.Context, md Metadata) (netip.Addr, error) {
	out, err := md.GetMetadata(ctx, &imds.GetMetadataInput{Path: "local-ipv4"})
	if err != nil {
		return netip.Addr{}, fmt.Errorf("Run the provisioner on an EC2 instance with IMDSv2 reachable from its container: reading local-ipv4 from the instance metadata failed: %w", err)
	}
	defer out.Content.Close()
	b, err := io.ReadAll(io.LimitReader(out.Content, maxAddress))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("Start the provisioner again: reading local-ipv4 from the instance metadata failed: %w", err)
	}
	text := strings.TrimSpace(string(b))
	addr, err := netip.ParseAddr(text)
	if err != nil || !addr.Is4() {
		return netip.Addr{}, fmt.Errorf("Check the host's network interface: local-ipv4 in the instance metadata is %q, not an IPv4 address", text)
	}
	return addr, nil
}
