// Package project implements multi-function operations.
package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"text/template"

	"github.com/apex/log"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/pkg/errors"
	"github.com/tj/go-sync/semaphore"

	"apex/function"
	"apex/hooks"
	"apex/infra"
	"apex/service"
	"apex/utils"
	"apex/vpc"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	"gopkg.in/validator.v2"
)

const (
	// DefaultMemory defines default memory value (MB) for every function in a project
	DefaultMemory = 128

	// DefaultTimeout defines default timeout value (s) for every function in a project
	DefaultTimeout = 3

	// DefaultRetainedVersions defines numbers of retained versions
	DefaultRetainedVersions = 25

	// functions directory
	functionsDir = "functions"

	iamAssumeRolePolicy = `{
  		"Version": "2012-10-17",
		"Statement": [
    	{
      		"Effect": "Allow",
      		"Principal": {
			"Service": "lambda.amazonaws.com"
      	},
      	"Action": "sts:AssumeRole"
    	}
  		]
	}`

	edgeIamAssumeRolePolicy = `{
			"Version": "2012-10-17",
		"Statement": [
			{
					"Effect": "Allow",
					"Principal": {
						"Service": [
				"lambda.amazonaws.com",
				"edgelambda.amazonaws.com"
			]
				},
				"Action": "sts:AssumeRole"
			}
			]
	}`

	iamLogsPolicy = `{
	  "Version": "2012-10-17",
      "Statement": [
      {
      	"Action": [
        	"logs:*"
      	],
      	"Effect": "Allow",
      	"Resource": "*"
      }
  	]
   }`

	iamEC2Policy = `{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "logs:CreateLogGroup",
                "logs:CreateLogStream",
                "logs:PutLogEvents",
                "ec2:CreateNetworkInterface",
                "ec2:DescribeNetworkInterfaces",
                "ec2:DeleteNetworkInterface"
            ],
            "Resource": "*"
        }
    ]
}`
)

// Config for project.
type Config struct {
	Name               string            `json:"name" validate:"nonzero"`
	Description        string            `json:"description"`
	Runtime            string            `json:"runtime"`
	Memory             int64             `json:"memory"`
	Timeout            int64             `json:"timeout"`
	Role               string            `json:"role"`
	Handler            string            `json:"handler"`
	Shim               bool              `json:"shim"`
	NameTemplate       string            `json:"nameTemplate"`
	RetainedVersions   *int              `json:"retainedVersions"`
	DefaultEnvironment string            `json:"defaultEnvironment"`
	Environment        map[string]string `json:"environment"`
	Hooks              hooks.Hooks       `json:"hooks"`
	VPC                vpc.VPC           `json:"vpc"`
	Zip                string            `json:"zip"`
	S3Bucket           string            `json:"s3bucket"`
}

// Project represents zero or more Lambda functions.
type Project struct {
	Config
	Path             string
	Alias            string
	Concurrency      int
	Environment      string
	InfraEnvironment string
	Log              log.Interface
	ServiceProvider  service.Provideriface
	Functions        []*function.Function
	IgnoreFile       []byte
	nameTemplate     *template.Template
	Session          *session.Session
}

// defaults applies configuration defaults.
func (p *Project) defaults() {
	p.Memory = DefaultMemory
	p.Timeout = DefaultTimeout
	p.IgnoreFile = []byte(".apexignore\nfunction.json\n")

	if p.Concurrency == 0 {
		p.Concurrency = 5
	}

	if p.Config.Environment == nil {
		p.Config.Environment = make(map[string]string)
	}

	if p.NameTemplate == "" {
		p.NameTemplate = "{{.Project.Name}}_{{.Function.Name}}"
	}

	if p.RetainedVersions == nil {
		p.RetainedVersions = aws.Int(DefaultRetainedVersions)
	}
}

// Open the project.json file and prime the config.
func (p *Project) Open() error {
	p.defaults()

	configFile := "project.json"
	if p.Environment != "" {
		configFile = fmt.Sprintf("project.%s.json", p.Environment)
	}

	f, err := os.Open(filepath.Join(p.Path, configFile))
	if err != nil {
		return err
	}

	if err := json.NewDecoder(f).Decode(&p.Config); err != nil {
		return err
	}

	if p.InfraEnvironment == "" {
		p.InfraEnvironment = p.Config.DefaultEnvironment
	}

	if p.Role == "" {
		p.Role = p.readInfraRole()
	}

	if err := validator.Validate(&p.Config); err != nil {
		return err
	}

	t, err := template.New("nameTemplate").Parse(p.NameTemplate)
	if err != nil {
		return err
	}
	p.nameTemplate = t

	ignoreFile, err := utils.ReadIgnoreFile(p.Path)
	if err != nil {
		return err
	}
	p.IgnoreFile = append(p.IgnoreFile, ignoreFile...)

	return nil
}

// LoadFunctions reads the ./functions directory, populating the Functions field.
// If no `patterns` are specified, all functions are loaded.
func (p *Project) LoadFunctions(patterns ...string) error {
	dir := filepath.Join(p.Path, functionsDir)
	p.Log.Debugf("loading functions in %s", dir)

	names, err := p.FunctionDirNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		match, err := matches(name, patterns)
		if err != nil {
			return errors.Wrapf(err, "matching %s", name)
		}

		if !match {
			continue
		}

		fn, err := p.LoadFunction(name)
		if err != nil {
			return errors.Wrapf(err, "loading %s", name)
		}

		p.Functions = append(p.Functions, fn)
	}

	if len(p.Functions) == 0 {
		return errors.New("no function loaded")
	}

	return nil
}

// DeployAndClean deploys functions and then cleans up their build artifacts.
func (p *Project) DeployAndClean(validate bool) error {
	if !p.checkRole() {
		fmt.Println("Role not found, create it")
		err := p.createRole()
		if err != nil {
			fmt.Println("Create role failed! Exiting!")
			os.Exit(1)
		}
		time.Sleep(30 * time.Second)
	}
	if err := p.Deploy(validate); err != nil {
		return err
	}

	return p.Clean()
}

func (p *Project) defName() string {
	splittedRole := strings.Split(p.Role, "/")
	return splittedRole[len(splittedRole)-1]
}

// check is Role exists
func (p *Project) checkRole() bool {
	roleName := p.defName()
	input := &iam.GetRoleInput{
		RoleName: aws.String(roleName),
	}
	svc := iam.New(p.Session)
	_, err := svc.GetRole(input)
	if err != nil {
		return false
	}

	return true
}

// create role
func (p *Project) createRole() error {
	roleName := p.defName()
	policyName := fmt.Sprintf("%s_lambda_logs", p.Name)
	policyNameEC2 := fmt.Sprintf("%s_lambda_ec2", p.Name)
	svc := iam.New(p.Session)

	var err error
	for _, fn := range p.Functions {
		if fn.Edge {
			logf("creating Edge IAM %s role", roleName)
			_, err = svc.CreateRole(&iam.CreateRoleInput{
				RoleName:                 &roleName,
				AssumeRolePolicyDocument: aws.String(edgeIamAssumeRolePolicy),
			})
		} else {
			logf("creating IAM %s role", roleName)
			_, err = svc.CreateRole(&iam.CreateRoleInput{
				RoleName:                 &roleName,
				AssumeRolePolicyDocument: aws.String(iamAssumeRolePolicy),
			})
		}
	}

	if err != nil {
		return fmt.Errorf("creating role: %s", err)
	}

	logf("creating IAM %s policy", policyName)
	_, err = svc.PutRolePolicy(&iam.PutRolePolicyInput{
		PolicyName:     &policyName,
		RoleName:       &roleName,
		PolicyDocument: aws.String(iamLogsPolicy),
	})

	if err != nil {
		return fmt.Errorf("creating policy: %s", err)
	}
	logf("creating IAM %s policy", policyNameEC2)
	_, err = svc.PutRolePolicy(&iam.PutRolePolicyInput{
		PolicyName:     &policyNameEC2,
		RoleName:       &roleName,
		PolicyDocument: aws.String(iamEC2Policy),
	})

	if err != nil {
		return fmt.Errorf("creating policy: %s", err)
	}

	return nil
}

// Deploy functions and their configurations.
func (p *Project) Deploy(validate bool) error {
	p.Log.Debugf("deploying %d functions", len(p.Functions))

	sem := make(semaphore.Semaphore, p.Concurrency)
	errs := make(chan error)

	go func() {
		for _, fn := range p.Functions {
			fn := fn
			sem.Acquire()

			go func() {
				defer sem.Release()

				err := fn.Deploy(p.Session, validate)
				if err != nil {
					err = fmt.Errorf("function %s: %s", fn.Name, err)
				}

				errs <- err
			}()
		}

		sem.Wait()
		close(errs)
	}()

	for err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// Clean up function build artifacts.
func (p *Project) Clean() error {
	p.Log.Debugf("cleaning %d functions", len(p.Functions))

	for _, fn := range p.Functions {
		if err := fn.Clean(); err != nil {
			return fmt.Errorf("function %s: %s", fn.Name, err)
		}
	}

	return nil
}

// Delete functions.
func (p *Project) Delete() error {
	p.Log.Debugf("deleting %d functions", len(p.Functions))

	svc := iam.New(p.Session)

	roleName := p.defName()
	policyName := fmt.Sprintf("%s_lambda_logs", p.Name)
	policyNameEC2 := fmt.Sprintf("%s_lambda_ec2", p.Name)

	logf("Deletenig %s policy", policyName)
	_, err := svc.DeleteRolePolicy(&iam.DeleteRolePolicyInput{
		RoleName:   &roleName,
		PolicyName: &policyName,
	})

	if err != nil {
		return errors.Wrap(err, "deleting policy")
	}

	logf("Deletenig %s policy", policyNameEC2)
	_, err = svc.DeleteRolePolicy(&iam.DeleteRolePolicyInput{
		RoleName:   &roleName,
		PolicyName: &policyNameEC2,
	})

	if err != nil {
		return errors.Wrap(err, "deleting policy")
	}

	logf("Deletenig %s role", roleName)
	_, err = svc.DeleteRole(&iam.DeleteRoleInput{
		RoleName: &roleName,
	})

	if err != nil {
		return errors.Wrap(err, "deleting iam role")
	}

	for _, fn := range p.Functions {
		if _, err := fn.GetConfig(); err != nil {
			if awserr, ok := err.(awserr.Error); ok && awserr.Code() == "ResourceNotFoundException" {
				p.Log.Infof("function %q hasn't been deployed yet or has been deleted manually on AWS Lambda", fn.Name)
				continue
			}
			return fmt.Errorf("function %s: %s", fn.Name, err)
		}

		if err := fn.Delete(); err != nil {
			return fmt.Errorf("function %s: %s", fn.Name, err)
		}
	}

	return nil
}

// Rollback project functions to previous version.
func (p *Project) Rollback() error {
	p.Log.Debugf("rolling back %d functions", len(p.Functions))

	for _, fn := range p.Functions {
		if err := fn.Rollback(); err != nil {
			return fmt.Errorf("function %s: %s", fn.Name, err)
		}
	}

	return nil
}

// RollbackVersion project functions to the specified version.
func (p *Project) RollbackVersion(version string) error {
	p.Log.Debugf("rolling back %d functions to version %s", len(p.Functions), version)

	for _, fn := range p.Functions {
		if err := fn.RollbackVersion(version); err != nil {
			return fmt.Errorf("function %s: %s", fn.Name, err)
		}
	}

	return nil
}

// FunctionDirNames returns a list of function directory names.
func (p *Project) FunctionDirNames() (list []string, err error) {
	dir := filepath.Join(p.Path, functionsDir)

	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		if file.IsDir() {
			list = append(list, file.Name())
		}
	}

	return list, nil
}

// LoadEnvFromFile reads `path` JSON and applies it to the environment.
func (p *Project) LoadEnvFromFile(path string) error {
	p.Log.Debugf("load env from file %q", path)
	var env map[string]string

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := json.NewDecoder(f).Decode(&env); err != nil {
		return err
	}

	for k, v := range env {
		p.Setenv(k, v)
	}

	return nil
}

// Setenv sets environment variable `name` to `value` on every function in project.
func (p *Project) Setenv(name, value string) {
	for _, fn := range p.Functions {
		fn.Setenv(name, value)
	}
}

// LoadFunction returns the function in the ./functions/<name> directory.
func (p *Project) LoadFunction(name string) (*function.Function, error) {
	return p.LoadFunctionByPath(name, filepath.Join(p.Path, functionsDir, name))
}

// LoadFunctionByPath returns the function in the given directory.
func (p *Project) LoadFunctionByPath(name, path string) (*function.Function, error) {
	fn := &function.Function{
		Config: function.Config{
			Runtime:          p.Runtime,
			Memory:           p.Memory,
			Timeout:          p.Timeout,
			Role:             p.Role,
			Handler:          p.Handler,
			Shim:             p.Shim,
			Hooks:            p.Hooks,
			Environment:      copyStringMap(p.Config.Environment),
			RetainedVersions: p.RetainedVersions,
			VPC:              copyVPC(p.VPC),
			Zip:              p.Zip,
		},
		Name:       name,
		Path:       path,
		Log:        p.Log,
		IgnoreFile: p.IgnoreFile,
		Alias:      p.Alias,
	}

	if name, err := p.name(fn); err == nil {
		fn.FunctionName = name
	} else {
		return nil, err
	}

	if err := fn.Open(p.Environment); err != nil {
		return nil, err
	}

	fn.Service = p.ServiceProvider.NewService(fn.AWSConfig())
	return fn, nil
}

// CreateOrUpdateAlias ensures the given `alias` is available for `version`.
func (p *Project) CreateOrUpdateAlias(alias, version string, validate bool) error {
	p.Log.Debugf("updating %d functions", len(p.Functions))

	sem := make(semaphore.Semaphore, p.Concurrency)
	errs := make(chan error)

	go func() {
		for _, fn := range p.Functions {
			fn := fn
			sem.Acquire()

			go func() {
				defer sem.Release()

				version, err := fn.GetVersionFromAlias(version)
				if err != nil {
					err = fmt.Errorf("function %s: %s", fn.Name, err)
				}

				err = fn.CreateOrUpdateAlias(alias, version, validate)
				if err != nil {
					err = fmt.Errorf("function %s: %s", fn.Name, err)
				}

				errs <- err
			}()
		}

		sem.Wait()
		close(errs)
	}()

	for err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// name returns the computed name for `fn`, using the nameTemplate.
func (p *Project) name(fn *function.Function) (string, error) {
	data := struct {
		Project  *Project
		Function *function.Function
	}{
		Project:  p,
		Function: fn,
	}

	name, err := render(p.nameTemplate, data)
	if err != nil {
		return "", err
	}

	return name, nil
}

// readInfraRole reads lambda function IAM role from infrastructure
func (p *Project) readInfraRole() string {
	role, err := infra.Output(p.InfraEnvironment, "lambda_function_role_id")
	if err != nil {
		p.Log.Debugf("couldn't read role from infrastructure: %s", err)
		return ""
	}
	p.Log.Info(role)
	return role
}

// render returns a string by executing template `t` against the given value `v`.
func render(t *template.Template, v interface{}) (string, error) {
	buf := new(bytes.Buffer)

	if err := t.Execute(buf, v); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// copyStringMap returns a copy of `in`.
func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// copyVPC returns a copy of `in`.
func copyVPC(in vpc.VPC) vpc.VPC {
	securityGroups := make([]string, len(in.SecurityGroups))
	copy(securityGroups, in.SecurityGroups)
	subnets := make([]string, len(in.Subnets))
	copy(subnets, in.Subnets)

	return vpc.VPC{
		SecurityGroups: securityGroups,
		Subnets:        subnets,
	}
}

// matches returns true if `name` is matched by any of the given `patterns`,
// or if zero `patterns` are provided.
func matches(name string, patterns []string) (bool, error) {
	if len(patterns) == 0 {
		return true, nil
	}

	for _, pattern := range patterns {
		match, err := filepath.Match(pattern, name)
		if err != nil {
			return false, err
		}

		if match {
			return true, nil
		}
	}

	return false, nil
}

// logf outputs a log message.
func logf(s string, v ...interface{}) {
	fmt.Printf("  \033[34m[+]\033[0m %s\n", fmt.Sprintf(s, v...))
}
