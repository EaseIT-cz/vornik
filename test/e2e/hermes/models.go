package hermes

// laneModel is one pinned model for Hermes. Hermes refuses a window below
// 64K, so each is served at 65536 tokens; models trained for 32K get YaRN.
// Only the default is the gate; the others are kept for the measurements
// recorded in the lane design's As built section.
type laneModel struct {
	Name   string // the model name Hermes is configured with
	URL    string
	File   string
	SHA256 string
	Args   []string // extra llama-server arguments
	// FileBytes is the pinned file's size (fixed by SHA256; ensureModel
	// checks it). OverheadBytes is what llama.cpp adds at a 64K window: KV
	// cache and compute buffers. Their sum is the container's memory limit
	// (lane design, "Memory guardrails", 2026-10-14).
	FileBytes     int64
	OverheadBytes int64
}

// memLimit is the llama.cpp container's memory limit for this model.
func (m laneModel) memLimit() int64 { return m.FileBytes + m.OverheadBytes }

var yarn64K = []string{"--rope-scaling", "yarn", "--rope-scale", "2", "--yarn-orig-ctx", "32768"}

// qwen3Args: thinking off; on CPU its reasoning tokens cost minutes per
// turn. (A q8_0 KV cache with flash attention cut memory but slowed prompt
// processing from 21 to 4 tokens/s on the bring-up host, 2026-09-30.)
var qwen3Args = append([]string{"--chat-template-kwargs", `{"enable_thinking":false}`}, yarn64K...)

// laneModels are the candidates measured at bring-up (lane design, As
// built). VORNIK_E2E_HERMES_LLM picks one; defaultLaneModel is the gate.
var laneModels = map[string]laneModel{
	"qwen2.5-3b": {
		Name:   "qwen2.5-3b-instruct",
		URL:    "https://huggingface.co/Qwen/Qwen2.5-3B-Instruct-GGUF/resolve/main/qwen2.5-3b-instruct-q4_k_m.gguf",
		File:   "qwen2.5-3b-instruct-q4_k_m.gguf",
		SHA256: "626b4a6678b86442240e33df819e00132d3ba7dddfe1cdc4fbb18e0a9615c62d",
		Args:   yarn64K,
		// Derived: f16 KV at 64K, 36 layers x 2 KV heads x 128 = 2.25 GiB,
		// plus 1 GiB of compute buffers.
		FileBytes:     2104932768,
		OverheadBytes: 2*gib + 256*mib + 1*gib,
	},
	"qwen3-4b": {
		Name:   "qwen3-4b",
		URL:    "https://huggingface.co/Qwen/Qwen3-4B-GGUF/resolve/main/Qwen3-4B-Q4_K_M.gguf",
		File:   "Qwen3-4B-Q4_K_M.gguf",
		SHA256: "7485fe6f11af29433bc51cab58009521f205840f5b4ae3a32fa7f92e8534fdf5",
		Args:   qwen3Args,
		// Derived: f16 KV at 64K with 8 KV heads = 9 GiB, plus 1 GiB.
		FileBytes:     2497280256,
		OverheadBytes: 10 * gib,
	},
	// The gate (operator's choice, 2026-09-30): a mixture of experts with
	// about 3.6B active parameters, so it generates at small-model speed on
	// CPU. Native 128K window; reasoning effort low keeps turns short.
	"gpt-oss-20b": {
		Name:   "gpt-oss-20b",
		URL:    "https://huggingface.co/ggml-org/gpt-oss-20b-GGUF/resolve/main/gpt-oss-20b-MXFP4.gguf",
		File:   "gpt-oss-20b-MXFP4.gguf",
		SHA256: "27cd6c432c7672cb812a92f611cf3ba7bbc35928262bb1e1253ff4ee6ae35901",
		Args:   []string{"--chat-template-kwargs", `{"reasoning_effort":"low"}`},
		// Measured: about 16 GB resident at bring-up, so ~3.9 GB over the
		// file; +25% headroom (lane design §2) rounds up to 5 GiB.
		FileBytes:     12109566624,
		OverheadBytes: 5 * gib,
	},
}

const defaultLaneModel = "gpt-oss-20b"
