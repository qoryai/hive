// Command qory-machines-aws-ec2 is the EC2 machine adapter: the provisioner runs it as
//
//	qory-machines-aws-ec2 <command> --settings FILE
//
// to make, list, read and delete EC2 instances, as package machines states the
// protocol. FILE is the adapter's settings file aws-ec2.json.
package main

import (
	"context"
	"os"

	"github.com/qoryai/hive/internal/awsec2"
	"github.com/qoryai/hive/machines"
)

func main() {
	os.Exit(machines.Serve(context.Background(), os.Args[1:], os.Stdin, os.Stdout, awsec2.New(os.Stderr)))
}
