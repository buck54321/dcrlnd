//go:build no_walletrpc
// +build no_walletrpc

package main

import "github.com/urfave/cli"

// walletCommands will return nil for non-walletrpc builds.
func walletCommands() []cli.Command {
	return nil
}
