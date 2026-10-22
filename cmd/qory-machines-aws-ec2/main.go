// Command qory-machines-aws-ec2 is the EC2 machine adapter: the provisioner runs it as
//
//	qory-machines-aws-ec2 <command> --settings FILE
//
// to make, list, read and delete EC2 instances, as machines/v1 of the integrations
// contracts states the protocol. FILE is the adapter's settings file aws-ec2.json.
package main

import (
	"context"
	"os"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/awsec2"
)

func main() {
	os.Exit(adapterio.Serve(context.Background(), os.Args[1:], os.Stdin, os.Stdout, awsec2.New(os.Stderr)))
}
