// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

// TODO(dimitern): bug http://pad.lv/1425569
// Disabled until we have time to fix these tests on i386 properly.
//
// +build !386

package main

import (
	"flag"
	stdtesting "testing"

	cmdtesting "github.com/juju/juju/cmd/testing"
	_ "github.com/juju/juju/provider/dummy" // XXX Why?
	"github.com/juju/juju/testing"
)

func TestPackage(t *stdtesting.T) {
	testing.MgoTestPackage(t)
}

// Reentrancy point for testing (something as close as possible to) the juju
// tool itself.
func TestRunMain(t *stdtesting.T) {
	if *cmdtesting.FlagRunMain {
		Main(flag.Args())
	}
}
