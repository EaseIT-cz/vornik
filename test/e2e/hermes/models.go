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

// boundedServerCaches caps llama-server's two host-memory caches, whose
// b11610 defaults are 32 context checkpoints per slot and an 8 GiB prompt
// cache (--cache-ram 8192): neither is in KV + compute, so a limit sized from
// those alone does not hold them. A checkpoint copies the state that cannot
// be shifted (a sliding-window KV cache, a recurrent state), so for Gemma 4
// each is 300 MiB, for Qwen3.6 63 MiB, for gpt-oss (128-token window) a few
// MiB. The gate runs with the defaults, as built (lane design, amendment
// 2026-10-15).
var boundedServerCaches = []string{"--ctx-checkpoints", "8", "--cache-ram", "2048"}

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
	// CPU. Native 128K window. The server's reasoning effort is low, but
	// Hermes's requests send reasoning_effort medium, which wins at b11610
	// (lane design, amendment 2026-10-15): the gate's turns reason at medium.
	"gpt-oss-20b": {
		Name:   "gpt-oss-20b",
		URL:    "https://huggingface.co/ggml-org/gpt-oss-20b-GGUF/resolve/main/gpt-oss-20b-MXFP4.gguf",
		File:   "gpt-oss-20b-MXFP4.gguf",
		SHA256: "27cd6c432c7672cb812a92f611cf3ba7bbc35928262bb1e1253ff4ee6ae35901",
		Args:   []string{"--chat-template-kwargs", `{"reasoning_effort":"low"}`},
		// Measured: about 16 GB resident at bring-up, so ~3.9 GB over the
		// file; +25% headroom (lane design §2) rounds up to 5 GiB. That
		// resident figure counted page cache; unlike the candidates' it is
		// not derived from llama.cpp's buffers, and is kept as built (lane
		// design, amendment 2026-10-15).
		FileBytes:     12109566624,
		OverheadBytes: 5 * gib,
	},
	// Gate candidates (GitHub #381; lane design, "Amendment 2026-10-15 —
	// gate model candidates"). Both are mixtures of experts with 3-4B
	// active parameters, measured on the CI runner VM against the gate.
	//
	// Qwen3.6-35B-A3B: the lineage of the local qwen3.6:35b (Ollama family
	// qwen35moe, 36.0B parameters). Native 256K window, so no YaRN at 64K.
	// Hybrid attention: only every fourth layer keeps a KV cache
	// (full_attention_interval 4), so the cache at 64K is small. Thinking off, as for qwen3-4b.
	"qwen3.6-35b-a3b": {
		Name:   "qwen3.6-35b-a3b",
		URL:    "https://huggingface.co/ggml-org/Qwen3.6-35B-A3B-GGUF/resolve/main/Qwen3.6-35B-A3B-Q4_K_M.gguf",
		File:   "Qwen3.6-35B-A3B-Q4_K_M.gguf",
		SHA256: "671e47e0ec53c665d048b98c3ecbfd5236b5ca9c3e02ed19fc8f81f7b85140c7",
		Args:   append([]string{"--chat-template-kwargs", `{"enable_thinking":false}`}, boundedServerCaches...),
		// Measured on the CI runner VM (b11610, 64K): KV 1,280 MiB (10 of 40
		// layers), recurrent state 63 MiB, compute 140 MiB = 1.45 GiB; plus
		// 8 checkpoints of the 63 MiB state (0.49 GiB) and the 2 GiB prompt
		// cache = 3.94 GiB; +25% (§2 headroom) rounds up to 5 GiB. Lane
		// design, amendment 2026-10-15.
		FileBytes:     20419565568,
		OverheadBytes: 5 * gib,
	},
	// Gemma 4 26B-A4B: production's same-class remote fallback. Google's
	// own quantization-aware Q4_0 file. Native 256K window.
	"gemma-4-26b-a4b": {
		Name:   "gemma-4-26b-a4b",
		URL:    "https://huggingface.co/google/gemma-4-26B-A4B-it-qat-q4_0-gguf/resolve/main/gemma-4-26B_q4_0-it.gguf",
		File:   "gemma-4-26B_q4_0-it.gguf",
		SHA256: "3eca3b8f6d7baf218a7dd6bba5fb59a56ee25fe2d567b6f5f589b4f697eca51d",
		Args:   append([]string{"--chat-template-kwargs", `{"enable_thinking":false}`}, boundedServerCaches...),
		// Measured on the CI runner VM (b11610, 64K): KV 1,580 MiB (5
		// global layers at 64K, 25 sliding-window layers at 1,536 cells)
		// plus 170 MiB compute = 1.71 GiB; plus 8 checkpoints of the 300 MiB
		// sliding-window cache (2.34 GiB) and the 2 GiB prompt cache = 6.05
		// GiB; +25% (§2 headroom) rounds up to 8 GiB. With llama.cpp's
		// default caches it was OOM-killed at 3 GiB (run gemma-r0oom). Lane
		// design, amendment 2026-10-15.
		FileBytes:     14439363584,
		OverheadBytes: 8 * gib,
	},
	// Round 2 (#381; lane design, amendment 2026-10-15, "Round 2"). Both are
	// pinned by Hugging Face commit, not main, and both bound the server
	// caches like the round-1 candidates.
	//
	// Nemotron 3.5 Lightning 30B-A3B: Mamba-2 + MoE hybrid, 3B active; only
	// 6 of 52 layers keep a KV cache. ggml-org's Q4_0 at the 2026-09-13
	// commit, after the conversion fix llama.cpp #27729. OpenMDW-1.1. Its
	// template reads enable_thinking (default true).
	"nemotron-3.5-lightning-30b-a3b": {
		Name:   "nemotron-3.5-lightning-30b-a3b",
		URL:    "https://huggingface.co/ggml-org/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-GGUF/resolve/8a08a1c81dadcc75d35dbb96016cfd344b632e67/NVIDIA-Nemotron-3.5-Lightning-30B-A3B-Q4_0.gguf",
		File:   "NVIDIA-Nemotron-3.5-Lightning-30B-A3B-Q4_0.gguf",
		SHA256: "61f87e75974e4b535dcdf9aad056541a9514f1dfa4538b463b081d19b7a00e3c",
		Args:   append([]string{"--chat-template-kwargs", `{"enable_thinking":false}`}, boundedServerCaches...),
		// Measured on the CI runner VM (b11610, 64K, probe 2026-10-15 22:15 UTC): KV
		// 384 MiB (6 layers), recurrent state 47.62 MiB, compute 143.52 MiB =
		// 0.56 GiB; plus 8 checkpoints of the 47.62 MiB state (0.37 GiB) and
		// the 2 GiB prompt cache = 2.93 GiB; +25% (§2 headroom) rounds up to
		// 4 GiB.
		FileBytes:     18898091584,
		OverheadBytes: 4 * gib,
	},
	// North-Mini-Code-1.0 (Cohere, cohere2moe, llama.cpp #24260): 3B active,
	// 13 full-attention layers and 36 sliding-window (4,096) ones.
	// Apache-2.0. Unsloth's UD-IQ4_XS, a single file. Its template reads
	// `reasoning`, not enable_thinking; with reasoning undefined it thinks
	// unless the request's reasoning_effort is "none", so Hermes's "medium"
	// would turn thinking on. reasoning:false holds against every effort
	// (rendered on the VM). IQ4_XS is not repacked: the weights stay mapped
	// file pages, so the limit must hold the whole file.
	"north-mini-code-1.0": {
		Name:   "north-mini-code-1.0",
		URL:    "https://huggingface.co/unsloth/North-Mini-Code-1.0-GGUF/resolve/e306bb4bf0df610f5471d97a01de2b6e0b24d356/North-Mini-Code-1.0-UD-IQ4_XS.gguf",
		File:   "North-Mini-Code-1.0-UD-IQ4_XS.gguf",
		SHA256: "34565d2cbce8202e7baacce74845e34852038f95c0f612398c3b794524a945c0",
		Args:   append([]string{"--chat-template-kwargs", `{"reasoning":false}`}, boundedServerCaches...),
		// Measured on the CI runner VM (b11610, 64K, probe 2026-10-15 22:15 UTC): KV
		// 1,664 MiB (13 full layers) + 324 MiB (36 window layers, 4,608
		// cells), compute 172.53 MiB = 2.11 GiB; plus 8 checkpoints of the
		// 324 MiB window cache (2.53 GiB: a checkpoint grows at 72 KiB per
		// token, measured, to the full cache once a prompt fills its 4,608
		// allocated cells, the 4,096 window plus the 512 batch, as Hermes's
		// 14K-token prompts do) and the 2 GiB prompt cache = 6.64 GiB; +25% (§2 headroom) rounds up to
		// 9 GiB.
		FileBytes:     15230132320,
		OverheadBytes: 9 * gib,
	},
}

const defaultLaneModel = "gpt-oss-20b"
