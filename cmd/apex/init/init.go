// Package init bootstraps an Apex project.
package init

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/tj/cobra"

	"apex/boot"
	"apex/cmd/apex/root"
)

var credentialsError = `

  AWS region missing, are your credentials set up? Try:

  $ export AWS_PROFILE=myapp-stage
  $ apex init

  Visit http://apex.run/#aws-credentials for more details on
  setting up AWS credentials and specifying which profile to
  use.

`
// env vars.
var env string

// Command config.
var Command = &cobra.Command{
	Use:              "init",
	Short:            "Initialize a project",
	PersistentPreRun: root.PreRunNoop,
	RunE:             run,
}

// Initialize.
func init() {
	root.Register(Command)
	f := Command.Flags()
	f.StringVarP(&env, "env", "e", "", "Set environment name")
}

// Run command.
func run(c *cobra.Command, args []string) error {
	if err := root.Prepare(c, args); err != nil {
		return err
	}
	region := root.Config.Region
	if region == nil {
		return errors.New(credentialsError)
	}

	b := boot.Bootstrapper{
		IAM:    iam.New(root.Session),
		Region: *region,
	}

	return b.Boot()
}
