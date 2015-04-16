// Copyright 2012, 2013, 2014 Canonical Ltd.
// Copyright 2014 Cloudbase Solutions SRL
// Licensed under the AGPLv3, see LICENCE file for details.

package cloudconfig

import (
	"fmt"

	"github.com/juju/errors"
	"github.com/juju/names"
	"github.com/juju/utils"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/cloudconfig/cloudinit"
	"github.com/juju/juju/cloudconfig/instancecfg"
	"github.com/juju/juju/version"
)

const (
	// fileSchemePrefix is the prefix for file:// URLs.
	fileSchemePrefix = "file://"

	// NonceFile is written by cloud-init as the last thing it does.
	// The file will contain the machine's nonce. The filename is
	// relative to the Juju data-dir.
	NonceFile = "nonce.txt"
)

// UserdataConfig is the bridge between instancecfg and cloudinit
// It supports different levels of configuration for instances
type UserdataConfig interface {
	// Configure is a convenience function that updates the cloudinit.Config
	// with appropriate configuration. It will run ConfigureBasic() and
	// ConfigureJuju()
	Configure() error
	// ConfigureBasic updates the provided cloudinit.Config with
	// basic configuration to initialise an OS image.
	ConfigureBasic() error
	// ConfigureJuju updates the provided cloudinit.Config with configuration
	// to initialise a Juju machine agent.
	ConfigureJuju() error
}

// UserdataConfig is supposed to take in an instanceConfig as well as a
// cloudinit.cloudConfig and add attributes in the cloudinit structure based on
// the values inside instanceConfig and on the series
func NewUserdataConfig(icfg *instancecfg.InstanceConfig, conf cloudinit.CloudConfig) (UserdataConfig, error) {
	// TODO(ericsnow) bug #1426217
	// Protect icfg and conf better.
	operatingSystem, err := version.GetOSFromSeries(icfg.Series)
	if err != nil {
		return nil, err
	}

	base := baseConfigure{
		tag:  names.NewMachineTag(icfg.MachineId),
		icfg: icfg,
		conf: conf,
		os:   operatingSystem,
	}

	switch operatingSystem {
	case version.Ubuntu:
		return &unixConfigure{base}, nil
	case version.CentOS:
		return &unixConfigure{base}, nil
	case version.Windows:
		return &windowsConfigure{base}, nil
	default:
		return nil, errors.NotSupportedf("OS %s", icfg.Series)
	}
}

type baseConfigure struct {
	tag  names.Tag
	icfg *instancecfg.InstanceConfig
	conf cloudinit.CloudConfig
	os   version.OSType
}

// addAgentInfo adds agent-required information to the agent's directory
// and returns the agent directory name.
func (c *baseConfigure) addAgentInfo(tag names.Tag) (agent.Config, error) {
	acfg, err := c.icfg.AgentConfig(tag, c.icfg.Tools.Version.Number)
	if err != nil {
		return nil, errors.Trace(err)
	}
	acfg.SetValue(agent.AgentServiceName, c.icfg.MachineAgentServiceName)
	cmds, err := acfg.WriteCommands(c.conf.ShellRenderer())
	if err != nil {
		return nil, errors.Annotate(err, "failed to write commands")
	}
	c.conf.AddScripts(cmds...)
	return acfg, nil
}

func (c *baseConfigure) addMachineAgentToBoot() error {
	svc, err := c.icfg.InitService(c.conf.ShellRenderer())
	if err != nil {
		return errors.Trace(err)
	}

	// Make the agent run via a symbolic link to the actual tools
	// directory, so it can upgrade itself without needing to change
	// the init script.
	toolsDir := c.icfg.ToolsDir(c.conf.ShellRenderer())
	c.conf.AddScripts(c.toolsSymlinkCommand(toolsDir))

	name := c.tag.String()
	cmds, err := svc.InstallCommands()
	if err != nil {
		return errors.Annotatef(err, "cannot make cloud-init init script for the %s agent", name)
	}
	startCmds, err := svc.StartCommands()
	if err != nil {
		return errors.Annotatef(err, "cannot make cloud-init init script for the %s agent", name)
	}
	cmds = append(cmds, startCmds...)

	svcName := c.icfg.MachineAgentServiceName
	c.conf.AddRunCmd(cloudinit.LogProgressCmd("Starting Juju machine agent (%s)", svcName))
	c.conf.AddScripts(cmds...)
	return nil
}

// TODO(ericsnow) toolsSymlinkCommand should just be replaced with a
// call to shell.Renderer.Symlink.

func (c *baseConfigure) toolsSymlinkCommand(toolsDir string) string {
	switch c.os {
	case version.Windows:
		return fmt.Sprintf(
			`cmd.exe /C mklink /D %s %v`,
			c.conf.ShellRenderer().FromSlash(toolsDir),
			c.icfg.Tools.Version,
		)
	default:
		// TODO(dfc) ln -nfs, so it doesn't fail if for some reason that
		// the target already exists.
		return fmt.Sprintf(
			"ln -s %v %s",
			c.icfg.Tools.Version,
			shquote(toolsDir),
		)
	}
}

func shquote(p string) string {
	return utils.ShQuote(p)
}
