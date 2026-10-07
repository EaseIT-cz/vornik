//go:build !linux

package agentloop

import (
	"fmt"
	"os"
)

// The supported agent image is Linux; never weaken output confinement on a
// platform without the reviewed descriptor-relative nofollow implementation.
func writeRenderedDocument(_ *os.Root, _ string, _ []byte) error {
	return fmt.Errorf("document_render output requires the Linux agent image")
}

func openDocumentSource(_ *os.Root, _ string) (*os.File, error) {
	return nil, fmt.Errorf("document_render requires the Linux agent image")
}
