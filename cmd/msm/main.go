// Command msm is the Go port of Minecraft Server Manager.
package main

import (
	"os"

	"github.com/thehatchcloud/msm/internal/buildinfo"
	"github.com/thehatchcloud/msm/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, buildinfo.Current()))
}
