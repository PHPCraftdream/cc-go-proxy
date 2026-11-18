package main

import (
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/cli"
	"github.com/PHPCraftdream/cc-go-proxy/cmd/ccgp/tui"
)

func init() {
	cli.InteractiveHook = tui.InteractiveCmd
	cli.LaunchPickedHook = tui.LaunchPicked
}
