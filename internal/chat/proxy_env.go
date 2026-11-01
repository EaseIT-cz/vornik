package chat

import (
	"os"

	"github.com/rs/zerolog"
)

// proxyEnvVars are the variables http.ProxyFromEnvironment reads for HTTP(S)
// targets. NO_PROXY only narrows them, so it is not reported.
var proxyEnvVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"}

// LogIgnoredProxyEnv logs one warning when a proxy variable is set: since
// GitHub #505 the model clients dial through the SSRF guard and ignore it
// (operator decision A6, 2026-10-21; 18-security-and-tenancy.md "Amendment
// 2026-10-28 (#505)", C5). It names the variables, never their values, which
// can carry proxy credentials.
func LogIgnoredProxyEnv(logger zerolog.Logger) {
	var set []string
	for _, k := range proxyEnvVars {
		if os.Getenv(k) != "" {
			set = append(set, k)
		}
	}
	if len(set) == 0 {
		return
	}
	logger.Warn().Strs("variables", set).
		Msg("model clients ignore these proxy variables since #505: they dial their endpoint directly through the SSRF guard; a provider reachable only through a proxy is unreachable")
}
