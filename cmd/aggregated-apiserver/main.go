package main

import (
	"os"

	apiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/component-base/cli"
	_ "k8s.io/component-base/logs/json/register" // for JSON log format registration

	"github.com/karmada-io/karmada/cmd/aggregated-apiserver/app"
)

func main() {
	ctx := apiserver.SetupSignalContext()
	cmd := app.NewAggregatedApiserverCommand(ctx)
	code := cli.Run(cmd)
	os.Exit(code)
}
