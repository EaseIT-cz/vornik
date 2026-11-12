package secretstore

import (
	"os"
	"sync/atomic"

	"vornik.io/vornik/internal/mcpauth"
)

// process is the daemon's resolver for secret values read at the point of
// use (GitHub #626, agent-administered Vornik design, amendment 2026-11-12
// E): the environment first, then the operator credentials in the store.
var process atomic.Pointer[mcpauth.SecretSource]

// SetProcessSource installs the daemon's resolver. The container calls it
// once at boot with its late-bound source, which sees a store created
// later.
func SetProcessSource(s mcpauth.SecretSource) {
	if s == nil {
		process.Store(nil)
		return
	}
	process.Store(&s)
}

// Value is the one way daemon code reads a secret by its variable name: a
// project integration's IMAP or SMTP password, Slack signing secret, GitHub
// webhook secret, a webhook source's secret, an a2a step's API key. It
// returns the environment's value when set, else the stored operator
// credential of that name, else "". Before SetProcessSource it reads the
// environment only, as os.Getenv did. internal/architecture's reader-list
// test refuses a new os.Getenv(<…>Env) secret read that bypasses it.
func Value(name string) string {
	if name == "" {
		return ""
	}
	if p := process.Load(); p != nil {
		v, _ := (*p).Get(name)
		return v
	}
	return os.Getenv(name)
}
