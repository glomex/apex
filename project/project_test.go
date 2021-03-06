package project_test

import (
	"testing"

	"apex/project"
	"github.com/apex/log"
	"github.com/apex/log/handlers/discard"
	"github.com/stretchr/testify/assert"

	_ "apex/plugins/golang"
	_ "apex/plugins/hooks"
	_ "apex/plugins/inference"
	_ "apex/plugins/nodejs"
	_ "apex/plugins/python"
	_ "apex/plugins/shim"
)

func init() {
	log.SetHandler(discard.New())
}

func TestProject_Open_requireConfigValues(t *testing.T) {
	p := &project.Project{
		Path: "_fixtures/invalidName",
		Log:  log.Log,
	}
	nameErr := p.Open()

	assert.Contains(t, nameErr.Error(), "Name: zero value")
}

func TestProject_LoadFunctions_loadAll(t *testing.T) {
	p := &project.Project{
		Path: "_fixtures/twoFunctions",
		Log:  log.Log,
	}

	assert.NoError(t, p.Open(), "open")
	assert.NoError(t, p.LoadFunctions(), "load")

	assert.Equal(t, 2, len(p.Functions))
	assert.Equal(t, "bar", p.Functions[0].Name)
	assert.Equal(t, "foo", p.Functions[1].Name)
}

func TestProject_LoadFunctions_loadSpecified(t *testing.T) {
	p := &project.Project{
		Path: "_fixtures/twoFunctions",
		Log:  log.Log,
	}

	assert.NoError(t, p.Open(), "open")
	assert.NoError(t, p.LoadFunctions("foo"), "load")

	assert.Equal(t, 1, len(p.Functions))
	assert.Equal(t, "foo", p.Functions[0].Name)
}

func TestProject_LoadFunctions_onlyExisting(t *testing.T) {
	p := &project.Project{
		Path: "_fixtures/twoFunctions",
		Log:  log.Log,
	}

	assert.NoError(t, p.Open(), "open")
	assert.NoError(t, p.LoadFunctions("foo", "something"), "load")

	assert.Equal(t, 1, len(p.Functions))
	assert.Equal(t, "foo", p.Functions[0].Name)
}

func TestProject_LoadFunctions_noFunctionLoaded(t *testing.T) {
	p := &project.Project{
		Path: "_fixtures/twoFunctions",
		Log:  log.Log,
	}

	p.Open()
	err := p.LoadFunctions("something")

	assert.EqualError(t, err, "no function loaded")
}

func TestProject_LoadFunctionByPath_mergeEnvWithFunctionEnv(t *testing.T) {
	p := &project.Project{
		Path: "_fixtures/envMerge",
		Log:  log.Log,
	}

	assert.NoError(t, p.Open(), "open")
	assert.NoError(t, p.LoadFunctions("foo"), "load")

	assert.Equal(t, map[string]string{"PROJECT_ENV": "projectEnv", "FUNCTION_ENV": "functionEnv", "APEX_FUNCTION_NAME": "foo", "LAMBDA_FUNCTION_NAME": "envMerge_foo"}, p.Functions[0].Environment)
}

func TestProject_LoadFunctionByPath_overrideVpcWithFunctionVpc(t *testing.T) {
	p := &project.Project{
		Path: "_fixtures/vpcOverride",
		Log:  log.Log,
	}

	p.Open()

	assert.Equal(t, "sg-default", p.VPC.SecurityGroups[0])

	bar, _ := p.LoadFunction("bar")
	assert.Equal(t, "sg-override", bar.VPC.SecurityGroups[0])
	assert.Equal(t, "sg-default", p.VPC.SecurityGroups[0])

	foo, _ := p.LoadFunction("foo")
	assert.Equal(t, "sg-default", foo.VPC.SecurityGroups[0])
	assert.Equal(t, "sg-default", p.VPC.SecurityGroups[0])
}
