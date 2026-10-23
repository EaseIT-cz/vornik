package dispatcher

import "strings"

// memoryChunkBody is the text of one memory chunk as it goes INSIDE its
// untrusted wrapper (GitHub #456, untrusted-content wrapper design §2.4):
// every field a chunk's writer controls, never a daemon-generated one.
// A companion remember call sets source_name (tags included),
// content_class and repo_scope free-form, so none of them may sit on a
// header line outside the wrapper. Empty fields are omitted; an all-empty
// chunk gives "", and callers then emit no wrapper at all.
func memoryChunkBody(sourceName, contentClass, repoScope, content string) string {
	var b strings.Builder
	if sourceName != "" {
		b.WriteString("source_name: ")
		b.WriteString(sourceName)
		b.WriteString("\n")
	}
	if contentClass != "" {
		b.WriteString("content_class: ")
		b.WriteString(contentClass)
		b.WriteString("\n")
	}
	if repoScope != "" {
		b.WriteString("repo_scope: ")
		b.WriteString(repoScope)
		b.WriteString("\n")
	}
	b.WriteString(content)
	return b.String()
}
