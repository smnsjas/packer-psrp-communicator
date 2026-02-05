// Command example demonstrates that the PSRP communicator package compiles
// and can be wired into a Packer plugin binary. It is NOT a usable standalone
// plugin — Packer's plugin system has no RegisterCommunicator hook, so this
// binary advertises zero components. Real usage requires a builder plugin
// that imports the communicator/psrp package and registers it via
// communicator.StepConnect.CustomConnect["psrp"].
//
// See the project README for integration instructions.
package main

import (
	"fmt"
	"os"

	"github.com/hashicorp/packer-plugin-sdk/plugin"
	"github.com/smnsjas/packer-psrp-communicator/version"
)

func main() {
	pps := plugin.NewSet()
	pps.SetVersion(version.PluginVersion)

	err := pps.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
