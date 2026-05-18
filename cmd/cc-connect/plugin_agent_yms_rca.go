//go:build !no_yms_rca

package main

import (
	ymsagent "github.com/chenhg5/cc-connect/agent/yms-rca"
	"github.com/chenhg5/cc-connect/daemon"
)

func init() {
	daemon.RegisterEnvDiscoverer(ymsagent.DiscoverDaemonEnv)
}
