package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// chatCompletion posts one classification prompt to mlx_lm server's
// OpenAI-compatible endpoint and returns the model's reply text. mlx_lm's
// server has no grammar/JSON-schema-constrained decoding (unlike
// llama-server), so the prompt itself asks for JSON and classify.go parses
// leniently rather than trusting the shape.
func chatCompletion(baseURL, prompt string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Messages:    []chatMessage{{Role: "user", Content: prompt}},
		Temperature: 0,
		MaxTokens:   300,
	})
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(baseURL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mlx_lm server: %s: %s", resp.Status, string(raw))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("mlx_lm server: decoding response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("mlx_lm server: empty response")
	}
	return out.Choices[0].Message.Content, nil
}
