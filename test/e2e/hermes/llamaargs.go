package hermes

// A bound on one model reply (lane design, "Amendment 2026-10-14 — a bound
// on one model reply", GitHub #380). Untagged so the unit suite reaches it;
// startLlama (infra_test.go, e2e_hermes) uses it.

import (
	"fmt"
	"strconv"
)

const (
	// defaultLlamaThreads suits a shared host; the dedicated CI runner VM
	// sets VORNIK_E2E_LLAMA_THREADS=14.
	defaultLlamaThreads = "6"
	// defaultLlamaMaxTokens: the longest legitimate reply measured in the
	// release lane was 243 tokens (run 4400844a); a runaway tool call ran
	// past 3,400. Hermes re-sends a truncated tool call up to 4 times, so one
	// runaway costs up to 5 x this bound.
	defaultLlamaMaxTokens = 1024
)

// llamaThreads is VORNIK_E2E_LLAMA_THREADS, or 6. Anything but a positive
// integer is an error, so a typo fails the run at once instead of as a
// llama.cpp start-up timeout (review 454e F4).
func llamaThreads(getenv func(string) string) (string, error) {
	v := getenv("VORNIK_E2E_LLAMA_THREADS")
	if v == "" {
		return defaultLlamaThreads, nil
	}
	if n, err := strconv.Atoi(v); err != nil || n <= 0 {
		return "", fmt.Errorf("VORNIK_E2E_LLAMA_THREADS=%q: want a positive integer", v)
	}
	return v, nil
}

// llamaMaxTokens is VORNIK_E2E_LLAMA_MAX_TOKENS, or 1024. Anything but a
// positive integer is an error: llama.cpp reads -1 as "no limit", and a
// typo must not remove the bound.
func llamaMaxTokens(getenv func(string) string) (int, error) {
	v := getenv("VORNIK_E2E_LLAMA_MAX_TOKENS")
	if v == "" {
		return defaultLlamaMaxTokens, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("VORNIK_E2E_LLAMA_MAX_TOKENS=%q: want a positive integer (the per-reply token bound)", v)
	}
	return n, nil
}

// llamaServerArgs is the podman run argument list for llama.cpp: publish
// port, model mount, image, then llama-server's flags, the model's own last.
// -n is the server default for a request that names no bound; it does not
// cap a request that does (b11610: the request's max_tokens wins), which
// ToolRecorder enforces.
func llamaServerArgs(port int, modelDir, image string, m laneModel, threads string, maxTokens int) []string {
	args := []string{"-p", fmt.Sprintf("127.0.0.1:%d:8080", port),
		"-v", modelDir + ":/models:ro,Z", image,
		"-m", "/models/" + m.File, "--jinja", "-c", "65536", "-np", "1",
		"-t", threads, "-n", strconv.Itoa(maxTokens),
		"--temp", "0", "--seed", "1", "--host", "0.0.0.0", "--port", "8080"}
	return append(args, m.Args...)
}
