package readability

// GitHub #633 design §6.3: the report a review returns. It states what was
// examined and what was not, and stays inside its caps.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRender_TablePerFileAndDenominators(t *testing.T) {
	out := Render([]File{{Name: "design.md", Report: Measure("This generally works. See above.\n")}}, nil)
	require.Contains(t, out, "| design.md |")
	require.Contains(t, out, "hedge list: ")
	require.Contains(t, out, "vague-reference list: ")
	require.Contains(t, out, "English", "the language scope is stated")
	require.Contains(t, out, "design.md:1")
	require.Contains(t, out, "hedge")
	require.Contains(t, out, "vague-reference")
}

func TestRender_NotExaminedListedWithReason(t *testing.T) {
	out := Render(nil, []NotExamined{{Name: "scan.pdf", Reason: "not a text file"}})
	require.Contains(t, out, "scan.pdf")
	require.Contains(t, out, "not a text file")
	require.Contains(t, out, "Files examined: 0")
}

func TestRender_AtMostTwentyFindingsAndSaysHowManyMore(t *testing.T) {
	text := strings.Repeat("This probably works.\n", 25)
	out := Render([]File{{Name: "a.md", Report: Measure(text)}}, nil)
	require.Equal(t, maxFindings, strings.Count(out, "\n- `a.md:"))
	require.Contains(t, out, "5 more findings not shown")
}

func TestRender_BoundedAtSixteenKiB(t *testing.T) {
	var files []File
	for i := 0; i < 400; i++ {
		files = append(files, File{Name: strings.Repeat("n", 100) + ".md", Report: Measure("Fine.\n")})
	}
	out := Render(files, nil)
	require.LessOrEqual(t, len(out), maxReportBytes)
	require.Contains(t, out, "report truncated")
}
