//go:build !no_yms_rca

package main

import (
	"os"

	_ "github.com/chenhg5/cc-connect/agent/yms-rca"
	"github.com/chenhg5/cc-connect/daemon"
	"github.com/chenhg5/cc-connect/ymsprofile"
)

func init() {
	daemon.RegisterEnvDiscoverer(discoverYmsRCAEnv)
}

// discoverYmsRCAEnv reads the yms-rca connections directory and
// returns the env-var name/value pairs for every profile's
// mcp.token_env, so `cc-connect daemon install` bakes them into the
// service file. The CC_YMS_RCA_CONNECTIONS_DIR override exists for
// tests and for users whose profiles live outside ~/.yms-rca.
func discoverYmsRCAEnv() (map[string]string, error) {
	dir := os.Getenv("CC_YMS_RCA_CONNECTIONS_DIR")
	if dir == "" {
		dir = ymsprofile.DefaultConnectionsDir()
	}
	if dir == "" {
		return nil, nil
	}
	entries, err := ymsprofile.DiscoverConnectionTokenEnvNames(dir)
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if v, ok := os.LookupEnv(e.EnvName); ok && v != "" {
			out[e.EnvName] = v
		}
	}
	return out, err
}
