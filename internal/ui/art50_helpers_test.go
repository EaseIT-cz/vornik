package ui

import (
	"context"

	"vornik.io/vornik/internal/aidisclosure"
)

// servedAllUI is a disclosure store for which every scope was already served:
// the state of a chat that was disclosed to in its originating turn (GitHub #342).
type servedAllUI struct{}

func (servedAllUI) WasServed(context.Context, string, string) (bool, error)  { return true, nil }
func (servedAllUI) MarkServed(context.Context, string, string, string) error { return nil }

func alreadyDisclosedUI() *aidisclosure.Service {
	return aidisclosure.New(aidisclosure.Config{}, servedAllUI{})
}
