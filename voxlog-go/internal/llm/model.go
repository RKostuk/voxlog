package llm

import "voxlog-go/internal/asr"

// Spec is the one LLM model Task Hub classification uses. Reuses
// asr.ModelSpec/asr.Download/asr.IsDownloaded/asr.ModelDir verbatim -- those
// are already generic over Family/Variant/Files, so a multi-file model needs
// no download code of its own; every file below lands flat inside
// asr.ModelDir(baseDir, Spec), which is exactly the layout mlx_lm expects
// for its --model directory.
//
// MLX, 8-bit -- the exact quantized build already tested, run through Apple's
// MLX runtime (see server.go) rather than llama.cpp/GGUF.
var Spec = asr.ModelSpec{
	Family:      "qwen-mlx",
	Variant:     "4b-8bit",
	Description: "Task classification model (MLX, 8-bit) · ~4.5 GB",
	Files: []asr.ModelFile{
		{URL: hfURL("config.json"), Filename: "config.json"},
		{URL: hfURL("generation_config.json"), Filename: "generation_config.json"},
		{URL: hfURL("tokenizer.json"), Filename: "tokenizer.json"},
		{URL: hfURL("tokenizer_config.json"), Filename: "tokenizer_config.json"},
		{URL: hfURL("chat_template.jinja"), Filename: "chat_template.jinja"},
		{URL: hfURL("model.safetensors.index.json"), Filename: "model.safetensors.index.json"},
		{URL: hfURL("model.safetensors"), Filename: "model.safetensors"},
	},
}

func hfURL(file string) string {
	return "https://huggingface.co/SiddhJagani/Qwen3.8-4B-mlx-8Bit/resolve/main/" + file
}
