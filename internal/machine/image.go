package machine

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// codeImageNotFound is the EC2 error code for an image id that does not exist.
const codeImageNotFound = "InvalidAMIID.NotFound"

// The CPU architectures of the provisioner contract's machine_spec.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// Image is what make and check read of the machine image.
type Image struct {
	// RootDevice is the root device name, such as /dev/xvda. Spec.RootDevice is set
	// from it.
	RootDevice string
	// RootSizeGiB is the size of the image's root volume in GiB, at least 1.
	RootSizeGiB int
	// Arch is the image's architecture as the provisioner contract writes it:
	// [ArchAMD64] or [ArchARM64].
	Arch string
}

// archOf returns the contract's architecture of an EC2 architecture, and false for an
// architecture that the contract does not have, such as i386 or x86_64_mac.
func archOf(a string) (string, bool) {
	switch a {
	case string(types.ArchitectureValuesX8664):
		return ArchAMD64, true
	case string(types.ArchitectureValuesArm64):
		return ArchARM64, true
	}
	return "", false
}

// Image returns the root device name, the root volume size and the architecture of
// image imageID, with one DescribeImages call for that image id.
func (c *Client) Image(ctx context.Context, imageID string) (Image, error) {
	if imageID == "" {
		return Image{}, defect("Fix the EC2 machine adapter: the image lookup has no image id")
	}
	out, err := c.api.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{imageID}})
	if errorCode(err) == codeImageNotFound {
		return Image{}, defect(fmt.Sprintf("Set image_id in %s to an image that exists in %s: DescribeImages returned no image %s: %v", settingsFile, c.Region(), imageID, err))
	}
	if err != nil {
		return Image{}, wrap("DescribeImages", err)
	}
	for _, img := range out.Images {
		if aws.ToString(img.ImageId) == imageID {
			return imageOf(img, imageID)
		}
	}
	return Image{}, defect(fmt.Sprintf("Set image_id in %s to an image that exists in %s: DescribeImages returned no image %s", settingsFile, c.Region(), imageID))
}

// imageOf reads the root device, its size and the architecture of img, the image
// imageID.
func imageOf(img types.Image, imageID string) (Image, error) {
	name := aws.ToString(img.RootDeviceName)
	if name == "" {
		return Image{}, defect(fmt.Sprintf("Set image_id in %s to an image with a root device: image %s has no root device name", settingsFile, imageID))
	}
	arch, ok := archOf(string(img.Architecture))
	if !ok {
		return Image{}, defect(fmt.Sprintf("Set image_id in %s to an x86_64 or arm64 image: image %s has the architecture %q", settingsFile, imageID, img.Architecture))
	}
	for _, m := range img.BlockDeviceMappings {
		if aws.ToString(m.DeviceName) != name || m.Ebs == nil {
			continue
		}
		if size := aws.ToInt32(m.Ebs.VolumeSize); size > 0 {
			return Image{RootDevice: name, RootSizeGiB: int(size), Arch: arch}, nil
		}
	}
	return Image{}, defect(fmt.Sprintf("Set image_id in %s to an image with an EBS root volume: image %s has no EBS volume of a known size on its root device", settingsFile, imageID))
}
