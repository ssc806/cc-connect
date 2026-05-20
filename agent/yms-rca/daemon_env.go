package ymsagent

import "os"

// daemonConnectionsDirEnv overrides the connections directory scanned by
// DiscoverDaemonEnv. Used by tests and deployment helpers whose profiles live
// outside ~/.yms-rca.
const daemonConnectionsDirEnv = "CC_YMS_RCA_CONNECTIONS_DIR"

// DiscoverDaemonEnv walks the yms-rca connections directory and returns
// the env-var name/value pairs derived from each profile's mcp.token_env.
//
// Behaviour:
//   - reads CC_YMS_RCA_CONNECTIONS_DIR if set, else DefaultConnectionsDir()
//   - returns nil, nil when no directory can be resolved (fresh install
//     without a yms-rca config is a normal state)
//   - returns the discovered env values; missing vars are skipped silently
//   - a non-nil error reflects profile-parse warnings
func DiscoverDaemonEnv() (map[string]string, error) {
	dir := os.Getenv(daemonConnectionsDirEnv)
	if dir == "" {
		dir = DefaultConnectionsDir()
	}
	if dir == "" {
		return nil, nil
	}
	entries, err := DiscoverConnectionTokenEnvNames(dir)
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if v, ok := os.LookupEnv(e.EnvName); ok && v != "" {
			out[e.EnvName] = v
		}
	}
	return out, err
}
