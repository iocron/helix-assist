package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/leona/helix-assist/internal/lsp"
	"github.com/leona/helix-assist/internal/util"
)

type OllamaProvider struct {
	model     string
	chatModel string
	endpoint  string
	timeout   time.Duration
	logger    *lsp.Logger
}

func NewOllamaProvider(model, chatModel, endpoint string, timeoutMs int, logger *lsp.Logger) *OllamaProvider {
	if chatModel == "" {
		chatModel = model
	}
	return &OllamaProvider{
		model:     model,
		chatModel: chatModel,
		endpoint:  strings.TrimSuffix(endpoint, "/"),
		timeout:   time.Duration(timeoutMs) * time.Millisecond,
		logger:    logger,
	}
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages,omitempty"`
	Prompt   string          `json:"prompt,omitempty"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaResponse struct {
	Response string `json:"response"`
	Message  *struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
}

type ollamaGenerateRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options,omitempty"`
	Context []any          `json:"context,omitempty"`
}

func (p *OllamaProvider) Completion(ctx context.Context, req CompletionRequest, filepath, languageID string, numSuggestions int) ([]string, error) {
	systemPrompt := BuildCompletionSystemPrompt(languageID)
	userPrompt := BuildCompletionUserPrompt(filepath, req.ContentBefore, req.ContentAfter)
	fullPrompt := systemPrompt + "\n\n" + userPrompt

	results := make([]string, 0, numSuggestions)

	for i := 0; i < numSuggestions; i++ {
		apiReq := ollamaGenerateRequest{
			Model:  p.model,
			Prompt: fullPrompt,
			Stream: false,
			Options: map[string]any{
				"temperature": 0.2,
			},
		}

		resp, err := p.doRequest(ctx, "/api/generate", apiReq)
		if err != nil {
			if len(results) > 0 {
				break
			}
			return nil, err
		}

		var apiResp ollamaResponse
		if err := json.Unmarshal(resp, &apiResp); err != nil {
			if len(results) > 0 {
				break
			}
			return nil, fmt.Errorf("parse response: %w", err)
		}

		if apiResp.Response != "" {
			results = append(results, apiResp.Response)
		}
	}

	return util.UniqueStrings(results), nil
}

func (p *OllamaProvider) Chat(ctx context.Context, query, content, filepath, languageID string) (*ChatResponse, error) {
	cleanFilepath := strings.TrimPrefix(filepath, "file://")

	systemPrompt := BuildChatSystemPrompt(languageID)
	userContent := BuildChatUserPrompt(languageID, cleanFilepath, content, query)

	apiReq := ollamaRequest{
		Model: p.chatModel,
		Messages: []ollamaMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Stream: false,
		Options: map[string]any{
			"temperature": 0.1,
		},
	}

	jsonReq, _ := json.MarshalIndent(apiReq, "", "  ")
	p.logger.Log("DEBUG [Ollama Chat]: Request:", string(jsonReq))

	resp, err := p.doRequest(ctx, "/api/chat", apiReq)
	if err != nil {
		return nil, err
	}

	p.logger.Log("DEBUG [Ollama Chat]: Raw response:", string(resp))

	var apiResp ollamaResponse
	if err := json.Unmarshal(resp, &apiResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	var resultText string
	if apiResp.Message != nil && apiResp.Message.Content != "" {
		resultText = apiResp.Message.Content
	} else {
		resultText = apiResp.Response
	}

	if resultText == "" {
		return nil, fmt.Errorf("no completion found")
	}

	p.logger.Log("DEBUG [Ollama Chat]: Extracted text:", resultText)
	return &ChatResponse{Result: resultText}, nil
}

func (p *OllamaProvider) doRequest(ctx context.Context, endpoint string, body any) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	url := p.endpoint + endpoint
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}
