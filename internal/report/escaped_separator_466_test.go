package report

// GitHub #466 (adversarial probe 2026-10-21): a key behind a literal escape
// sequence, as in a provider's JSON error body quoted inside last_error
// ({"error":"x\nghp_..."} with the two characters backslash and n), passed
// the line scrubber: the letter before the key defeats the \b-anchored nets
// and the detector. Found at: review (a probe of the primitive); a user reads
// it on every scrubbed route, which the api chain tests cover; this test pins
// the primitive (process-spawn-law design, "Amendment 2026-10-21 (#466)", D5).

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLineScrubber466_EscapedSeparatorIsWithheld(t *testing.T) {
	scrub, err := NewLineScrubber("")
	require.NoError(t, err)
	aws := "AKIA" + "IOSFODNN7EXAMPLQ"
	slack := "xoxb-" + "1234567890-abcdefghij"
	ghp := "ghp_" + "Q7rT2xLm9KQ7rT2xLm9KQ7rT2xLm9KQ7rT2x"
	for _, line := range []string{
		`{"error":"x\n` + aws + `"}`,
		`{"error":"x\n` + slack + `"}`,
		`{"error":"x\n` + ghp + `"}`,
		`x\t` + aws,
		`x\r\n` + aws,
		`x\u000a` + ghp,
		`x\x0a` + slack,
		`{"x\n` + slack + `":1}`,
		`x\"` + ghp,
		`{"body":"{\"error\":\"x\\n` + ghp + `\"}"}`, // JSON quoted inside JSON
		`x\\\n` + aws,
		// Adversarial code review 2026-10-21 (review-20261021-1d49 F1 and
		// the lane's own adversarial pass): a backslash before any letter,
		// a C octal escape, a percent-encoded separator, and an escape
		// inside the secret itself.
		`x\a` + ghp,
		`x\e` + aws,
		`x\_` + slack,
		`x\012` + aws,
		`token%3D` + ghp,
		`Authorization: Bearer%20` + ghp,
		`q=a%26token%3D` + aws,
		`x%0A` + slack,
		`x%5Cn` + ghp,
		`"aws_secret_access_key":"wJalrXUtnFEMI\/K7MDENG\/bPxRfiCYEXAMPLEKEY"`,
		// Code review round 2 (review-20261021-1033): an escaped backslash
		// written as a hex or unicode escape, then n, then the key.
		`x\x5Cn` + ghp,
		`x\n` + aws,
		// Coordinator security review 2026-10-21: a malformed '%' elsewhere on
		// the line made the whole-line percent decode fail and skip
		// (fail-open), and decoding was single-pass, so a double-encoded
		// separator passed.
		`100% done; token%3D` + ghp,
		`token%253D` + ghp,
		`x%255Cn` + aws,
		`x\\\\u005Cn` + slack,
	} {
		out, withheld := scrub(line)
		assert.True(t, withheld, "a key behind an escape sequence passed: %q -> %q", line, out)
	}

	// A line with a backslash and no secret is still served, scrubbed as
	// before.
	out, withheld := scrub(`path C:\new\tmp failed: exit status 1`)
	assert.False(t, withheld)
	assert.Equal(t, `path C:\new\tmp failed: exit status 1`, out)

	// A malformed or plain '%' with no secret is still served.
	out, withheld = scrub(`progress 100% done, 5%% left`)
	assert.False(t, withheld)
	assert.Equal(t, `progress 100% done, 5%% left`, out)

	// Fail closed: a line still decoding after the bound is withheld,
	// whatever it hides.
	layered := "token%3D" + ghp
	for i := 0; i < 6; i++ {
		layered = strings.ReplaceAll(layered, "%", "%25")
	}
	_, withheld = scrub(layered)
	assert.True(t, withheld, "a line encoded past the decode bound must be withheld")
}
