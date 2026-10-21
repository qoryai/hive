package machine

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// codeImageNotFound is the EC2 error code for an image id that does not exist.
const codeImageNotFound = "InvalidAMIID.NotFound"

// RootDevice returns the root device name of image imageID, such as /dev/sda1, with
// one DescribeImages call for that image id. Spec.RootDevice is set from it.
func (c *Client) RootDevice(ctx context.Context, imageID string) (string, error) {
	if imageID == "" {
		return "", fmt.Errorf("Report this defect in the provisioner: the root device lookup has no image id")
	}
	out, err := c.api.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{imageID}})
	if errorCode(err) == codeImageNotFound {
		return "", fmt.Errorf("Use an image id that exists in this region: DescribeImages returned no image %s: %w", imageID, err)
	}
	if err != nil {
		return "", wrap("DescribeImages", err)
	}
	for _, img := range out.Images {
		if aws.ToString(img.ImageId) != imageID {
			continue
		}
		if name := aws.ToString(img.RootDeviceName); name != "" {
			return name, nil
		}
		return "", fmt.Errorf("Use an image with a root device: image %s has no root device name", imageID)
	}
	return "", fmt.Errorf("Use an image id that exists in this region: DescribeImages returned no image %s", imageID)
}
