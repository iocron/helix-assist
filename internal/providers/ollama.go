package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/leona/helix-assist/internal/lsp"
)

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type OllamaProvider struct {
	model      string
	chatModel  string
	endpoint   string
	timeout    time.Duration
	logger     *lsp.Logger
	httpClient *http.Client
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
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutMs) * time.Millisecond,
		},
	}
}

type ollamaGenerateRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Suffix  string         `json:"suffix,omitempty"`
	Stream  bool           `json:"stream"`
	Raw     bool           `json:"raw,omitempty"`
	Options map[string]any `json:"options,omitempty"`
}

type ollamaGenerateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

type ollamaChatRequest struct {
	Model    string         `json:"model"`
	Messages []ollamaMsg    `json:"messages"`
	Stream   bool           `json:"stream"`
	Options  map[string]any `json:"options,omitempty"`
}

type ollamaMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatResponse struct {
	Message *ollamaMsg `json:"message"`
	Done    bool       `json:"done"`
}

func (p *OllamaProvider) Completion(ctx context.Context, req CompletionRequest, filepath, languageID string, numSuggestions int) ([]string, error) {
	// Limit context to last 30 lines (increased from 20)
	beforeLines := strings.Split(req.ContentBefore, "\n")
	if len(beforeLines) > 30 {
		beforeLines = beforeLines[len(beforeLines)-30:]
	}
	before := strings.Join(beforeLines, "\n")

	// Limit after context to 15 lines (increased from 5 to show more existing code)
	// This helps the model avoid regenerating code that already exists
	afterLines := strings.Split(req.ContentAfter, "\n")
	if len(afterLines) > 15 {
		afterLines = afterLines[:15]
	}
	after := strings.Join(afterLines, "\n")

	p.logger.Log("Ollama FIM before:", before[maxInt(0, len(before)-200):])
	p.logger.Log("Ollama FIM after:", after[:minInt(100, len(after))])

	// Build FIM prompt using Qwen-style tokens
	// Format: <|fim_prefix|>{before}<|fim_suffix|>{after}<|fim_middle|>
	fimPrompt := fmt.Sprintf("<|fim_prefix|>%s<|fim_suffix|>%s<|fim_middle|>", before, after)

	// Ensure at least 1 suggestion
	if numSuggestions < 1 {
		numSuggestions = 1
	}

	// Generate multiple suggestions in parallel
	type completionResult struct {
		index      int
		completion string
		err        error
	}

	resultChan := make(chan completionResult, numSuggestions)
	var wg sync.WaitGroup

	for i := 0; i < numSuggestions; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			// Increase temperature for subsequent suggestions to get diversity
			// First: 0.2, Second: 0.4, Third: 0.6, etc.
			temperature := 0.2 + (float64(idx) * 0.2)
			if temperature > 0.9 {
				temperature = 0.9
			}

			apiReq := ollamaGenerateRequest{
				Model:  p.model,
				Prompt: fimPrompt,
				Stream: false,
				Raw:    true,
				Options: map[string]any{
					"temperature": temperature,
					"top_p":       0.9,
					"num_predict": 128,
					"stop":        []string{"\n\n\n", "<|fim", "<|end", "<|file", "```", "\nfunc ", "\n//"},
					"seed":        idx, // Different seed for each suggestion
				},
			}

			resp, err := p.doRequest(ctx, "/api/generate", apiReq)
			if err != nil {
				p.logger.Log("Ollama request failed for suggestion", idx+1, ":", err)
				resultChan <- completionResult{idx, "", err}
				return
			}

			var apiResp ollamaGenerateResponse
			if err := json.Unmarshal(resp, &apiResp); err != nil {
				p.logger.Log("Parse error for suggestion", idx+1, ":", err)
				resultChan <- completionResult{idx, "", err}
				return
			}

			if apiResp.Response == "" {
				p.logger.Log("Ollama returned empty response for suggestion", idx+1)
				resultChan <- completionResult{idx, "", fmt.Errorf("empty response")}
				return
			}

			p.logger.Log(fmt.Sprintf("Ollama raw response [%d/%d]:", idx+1, numSuggestions), apiResp.Response[:minInt(300, len(apiResp.Response))])

			completion := p.cleanCompletion(apiResp.Response, req.ContentBefore, req.ContentAfter)
			if completion == "" {
				p.logger.Log("Ollama completion was empty after cleaning for suggestion", idx+1)
				resultChan <- completionResult{idx, "", fmt.Errorf("empty after cleaning")}
				return
			}

			p.logger.Log(fmt.Sprintf("Ollama cleaned completion [%d/%d]:", idx+1, numSuggestions), completion[:minInt(200, len(completion))])
			resultChan <- completionResult{idx, completion, nil}
		}(i)
	}

	// Wait for all goroutines to finish
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results
	completions := make([]string, 0, numSuggestions)
	seen := make(map[string]bool)

	for result := range resultChan {
		if result.err == nil && result.completion != "" {
			// Only add unique completions
			if !seen[result.completion] {
				seen[result.completion] = true
				completions = append(completions, result.completion)
			} else {
				p.logger.Log("Skipping duplicate completion for suggestion", result.index+1)
			}
		}
	}

	if len(completions) == 0 {
		p.logger.Log("No valid completions generated")
		return nil, nil
	}

	p.logger.Log(fmt.Sprintf("Generated %d unique completions", len(completions)))
	return completions, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// isBlockContext checks if the before content suggests we're starting a block (for, if, func, etc.)
func (p *OllamaProvider) isBlockContext(before string) bool {
	lines := strings.Split(before, "\n")
	if len(lines) == 0 {
		return false
	}
	lastLine := strings.TrimSpace(lines[len(lines)-1])

	// Check for block-starting keywords
	blockStarters := []string{"for ", "for{", "if ", "if(", "else ", "else{", "switch ", "select ", "func ", "func("}
	for _, starter := range blockStarters {
		if strings.HasPrefix(lastLine, starter) || strings.Contains(lastLine, starter) {
			return true
		}
	}

	// Check if line ends with opening brace
	if strings.HasSuffix(lastLine, "{") {
		return true
	}

	return false
}

func (p *OllamaProvider) cleanCompletion(response, before, after string) string {
	if response == "" {
		return ""
	}

	p.logger.Log("cleanCompletion input:", response[:minInt(300, len(response))])

	// Remove markdown code blocks
	codeBlockRe := regexp.MustCompile("(?s)```[a-z]*\\n?(.*?)```")
	if matches := codeBlockRe.FindStringSubmatch(response); len(matches) > 1 {
		response = strings.TrimSpace(matches[1])
	}

	// Remove inline backticks
	response = strings.Trim(response, "`")

	// Remove model-specific tokens
	for _, token := range []string{"<|", "<FILL>", "<CURSOR>", "</s>", "<s>"} {
		if idx := strings.Index(response, token); idx != -1 {
			response = response[:idx]
		}
	}

	// Remove common chat prefixes
	prefixes := []string{
		"here's the completion:",
		"here is the completion:",
		"here is the completed code:",
		"completion:",
		"the completion is:",
		"output:",
		"answer:",
		"code:",
	}
	lower := strings.ToLower(strings.TrimSpace(response))
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			response = strings.TrimSpace(response[len(prefix):])
			lower = strings.ToLower(strings.TrimSpace(response))
		}
	}

	// Get the text on the current line (before cursor)
	beforeLines := strings.Split(before, "\n")
	currentLinePrefix := ""
	if len(beforeLines) > 0 {
		currentLinePrefix = strings.TrimSpace(beforeLines[len(beforeLines)-1])
	}

	// Strip repeated current line prefix from response
	// e.g., if cursor is at "for " and model returns "for i := 0", extract "i := 0"
	if currentLinePrefix != "" {
		respTrimmed := strings.TrimSpace(response)
		if strings.HasPrefix(respTrimmed, currentLinePrefix) {
			response = strings.TrimPrefix(respTrimmed, currentLinePrefix)
			response = strings.TrimLeft(response, " ")
		}
	}

	// Truncate at overlap with 'after' content
	if after != "" {
		response = p.truncateAtAfterOverlap(response, after)
		response = p.removeAfterDuplicates(response, after)
		response = p.removeAfterFunctionDuplicates(response, after)
	}

	// For simple statements (no block opening), limit completion more aggressively
	// This prevents verbose multi-line suggestions for variable declarations etc.
	if !p.isBlockContext(before) && strings.Contains(response, "\n") {
		lines := strings.Split(response, "\n")

		// For variable declarations or simple assignments, keep only first complete statement
		beforeTrimmed := strings.TrimSpace(before)
		if strings.HasSuffix(beforeTrimmed, "var") ||
		   strings.Contains(beforeTrimmed, "var ") ||
		   strings.Contains(beforeTrimmed, ":=") {
			// Find first line that completes the statement
			firstLine := strings.TrimSpace(lines[0])
			if len(firstLine) >= 2 {
				p.logger.Log("Truncating variable declaration to first statement")
				response = lines[0]
			}
		} else if len(lines) > 10 {
			// For other non-block contexts, only truncate if very long
			firstLine := strings.TrimSpace(lines[0])
			if len(firstLine) >= 2 && !strings.HasSuffix(firstLine, "{") {
				p.logger.Log("Truncating long non-block completion to first line")
				response = lines[0]
			}
		}
	}

	response = strings.TrimLeft(response, "\n")
	response = strings.TrimRight(response, " \t\n")

	// BIG FIX: HELIX HANGING ISSUE - Remove ALL leading whitespace (spaces and tabs)
	// Model often adds leading whitespace that Helix filters out
	response = strings.TrimLeft(response, " \t")

	if len(strings.TrimSpace(response)) < 2 {
		return ""
	}

	p.logger.Log("cleanCompletion output:", response[:minInt(200, len(response))])
	return response
}

// truncateAtAfterOverlap truncates completion where it starts repeating 'after' content
func (p *OllamaProvider) truncateAtAfterOverlap(response, after string) string {
	if after == "" {
		return response
	}

	// Get the first meaningful token/word from after content
	afterTrimmed := strings.TrimSpace(after)
	if afterTrimmed == "" {
		return response
	}

	// Extract first word/token from after (skip single chars like braces)
	firstAfterLine := strings.Split(afterTrimmed, "\n")[0]
	firstAfterToken := strings.TrimSpace(firstAfterLine)

	// Skip if it's just a brace or too short
	if len(firstAfterToken) <= 1 {
		return response
	}

	// Also try first word only
	words := strings.Fields(firstAfterToken)
	if len(words) == 0 {
		return response
	}
	firstWord := words[0]

	// Skip common tokens that appear everywhere
	skipTokens := map[string]bool{
		"{": true, "}": true, "(": true, ")": true,
		"[": true, "]": true, "//": true, "/*": true,
	}
	if skipTokens[firstWord] {
		if len(words) > 1 {
			firstWord = words[1]
		} else {
			return response
		}
	}

	// Find if response contains the first after token/word at a natural boundary
	// This catches cases where model generates content that should come after cursor
	if len(firstWord) >= 2 {
		// Look for the token in response
		idx := strings.Index(response, firstWord)
		if idx > 0 {
			// Check if it's at a word boundary (preceded by non-alphanumeric)
			if idx > 0 {
				prevChar := response[idx-1]
				if prevChar == ' ' || prevChar == '\t' || prevChar == '\n' ||
					prevChar == '[' || prevChar == '(' || prevChar == '{' ||
					prevChar == '=' || prevChar == ':' {
					// Truncate before this overlap
					response = strings.TrimRight(response[:idx], " \t")
				}
			}
		}
	}

	return response
}

// removeAfterDuplicates removes content from completion that duplicates the start of 'after'
func (p *OllamaProvider) removeAfterDuplicates(response, after string) string {
	afterLines := strings.Split(after, "\n")
	respLines := strings.Split(response, "\n")

	// Only check for duplicates at the end of the response that match the start of 'after'
	// Don't remove structural tokens like single braces that appear throughout code
	validLines := respLines

	// Check if the last few lines of response match the first few lines of after
	for i := minInt(3, len(respLines)); i > 0; i-- {
		respEnd := respLines[len(respLines)-i:]
		if len(afterLines) < i {
			continue
		}
		afterStart := afterLines[:i]

		match := true
		hasSubstantialContent := false
		for j := 0; j < i; j++ {
			respTrimmed := strings.TrimSpace(respEnd[j])
			afterTrimmed := strings.TrimSpace(afterStart[j])

			// Track if we have substantial content (not just braces/empty)
			if respTrimmed != "" && respTrimmed != "}" && respTrimmed != "{" {
				hasSubstantialContent = true
			}

			if respTrimmed != afterTrimmed {
				match = false
				break
			}
		}

		// Only remove if lines match AND include substantial content
		// This prevents removing matches that are only structural braces
		if match && hasSubstantialContent {
			// Safety check: don't remove if it would leave less than 1 line
			if len(respLines)-i < 1 {
				p.logger.Log("removeAfterDuplicates: skipping - would remove entire response")
				continue
			}
			p.logger.Log("removeAfterDuplicates: removing", i, "duplicate lines")
			validLines = respLines[:len(respLines)-i]
			break
		}
	}

	return strings.Join(validLines, "\n")
}

// removeAfterFunctionDuplicates removes entire function definitions from response if they appear in 'after'
func (p *OllamaProvider) removeAfterFunctionDuplicates(response, after string) string {
	if response == "" || after == "" {
		return response
	}

	respLines := strings.Split(response, "\n")
	afterLines := strings.Split(after, "\n")

	// Only apply this cleanup if response is suspiciously long (>15 lines)
	// This prevents removing valid short completions
	if len(respLines) < 15 {
		return response
	}

	// Look for function definitions that appear in both response and after
	// Only remove if they're in the SECOND HALF of the response (likely duplicates)
	startCheckFrom := len(respLines) / 2

	for i := startCheckFrom; i < len(respLines); i++ {
		line := strings.TrimSpace(respLines[i])
		// Check if this line starts a function definition
		if strings.HasPrefix(line, "func ") || strings.HasPrefix(line, "func(") {
			// Check if this exact function signature appears in first 10 lines of after
			for j := 0; j < minInt(10, len(afterLines)); j++ {
				afterLine := strings.TrimSpace(afterLines[j])
				if line == afterLine {
					// Found matching function in after context - likely a duplicate
					p.logger.Log("Removing duplicate function from completion:", line)
					result := strings.TrimRight(strings.Join(respLines[:i], "\n"), " \t\n")
					// Safety: don't remove more than 70% of response
					if len(result) > len(response)/3 {
						return result
					}
					p.logger.Log("Skipping removal - would remove too much content")
				}
			}
		}
	}

	return response
}

func (p *OllamaProvider) Chat(ctx context.Context, systemPrompt, userPrompt string) (*ChatResponse, error) {
	apiReq := ollamaChatRequest{
		Model: p.chatModel,
		Messages: []ollamaMsg{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream: false,
		Options: map[string]any{
			"temperature": 0.1,
			"num_predict": 2048,
		},
	}

	resp, err := p.doRequest(ctx, "/api/chat", apiReq)
	if err != nil {
		return nil, err
	}

	var apiResp ollamaChatResponse
	if err := json.Unmarshal(resp, &apiResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if apiResp.Message == nil || apiResp.Message.Content == "" {
		return nil, fmt.Errorf("no response from model")
	}

	result := p.cleanChatResponse(apiResp.Message.Content)
	return &ChatResponse{Result: result}, nil
}

func (p *OllamaProvider) cleanChatResponse(response string) string {
	// Remove markdown code blocks
	codeBlockRe := regexp.MustCompile("(?s)```[a-z]*\\n?(.*?)```")
	if matches := codeBlockRe.FindStringSubmatch(response); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return strings.TrimSpace(response)
}

func (p *OllamaProvider) doRequest(ctx context.Context, endpoint string, body any) ([]byte, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := p.endpoint + endpoint
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Use a client without timeout since we rely on context for cancellation
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read body with context cancellation support
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(resp.Body)
		done <- result{data, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("read response: %w", r.err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(r.data))
		}
		return r.data, nil
	}
}
