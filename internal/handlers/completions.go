package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leona/helix-assist/internal/config"
	"github.com/leona/helix-assist/internal/lsp"
	"github.com/leona/helix-assist/internal/providers"
	"github.com/leona/helix-assist/internal/util"
)

type CompletionHandler struct {
	cfg      *config.Config
	registry *providers.Registry

	mu            sync.Mutex
	cancelCurrent context.CancelFunc
	timer         *time.Timer
	requestID     atomic.Uint64
	lastTrigger   time.Time
	lastContent   string
	pendingMsgID  *int
}

func NewCompletionHandler(cfg *config.Config, registry *providers.Registry) *CompletionHandler {
	return &CompletionHandler{
		cfg:      cfg,
		registry: registry,
	}
}

func (h *CompletionHandler) Register(svc *lsp.Service) {
	svc.On(lsp.EventCompletion, func(svc *lsp.Service, msg *lsp.JSONRPCMessage) {
		var params lsp.CompletionParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			svc.Logger.Log("completion parse error:", err.Error())
			h.sendEmptyCompletion(svc, msg.ID)
			return
		}

		buffer, ok := svc.Buffers.Get(params.TextDocument.URI)
		if !ok {
			h.sendEmptyCompletion(svc, msg.ID)
			return
		}

		content := util.GetContent(buffer.Text, params.Position.Line, params.Position.Character)

		// Skip completion in certain cases
		if h.shouldSkip(content, buffer.Text) {
			svc.Logger.Log("skipping completion - invalid context")
			h.sendEmptyCompletion(svc, msg.ID)
			return
		}

		// Schedule the completion with debouncing and cancellation
		h.scheduleCompletion(svc, msg, params, buffer, content)
	})
}

func (h *CompletionHandler) shouldSkip(content util.ContentParts, fullText string) bool {
	lastChar := content.LastCharacter

	// Skip if cursor is after a dot (method/property access)
	if lastChar == "." {
		return true
	}

	// Skip if we're in a comment
	lastLine := strings.TrimSpace(content.LastLine)
	if strings.HasPrefix(lastLine, "//") || strings.HasPrefix(lastLine, "#") {
		return true
	}

	// Skip if the line is empty or just whitespace (except for indentation-based completion)
	trimmedLine := strings.TrimSpace(content.LastLine)
	if trimmedLine == "" {
		return true
	}

	// Skip if line only has whitespace and no keywords
	if len(trimmedLine) < 2 {
		return true
	}

	return false
}

func (h *CompletionHandler) scheduleCompletion(svc *lsp.Service, msg *lsp.JSONRPCMessage, params lsp.CompletionParams, buffer *lsp.Buffer, content util.ContentParts) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Cancel any previous pending request
	if h.cancelCurrent != nil {
		h.cancelCurrent()
		h.cancelCurrent = nil
	}
	if h.timer != nil {
		if h.timer.Stop() && h.pendingMsgID != nil {
			// Timer stopped before firing - executeCompletion never ran,
			// so the editor is still waiting for a response.
			h.sendEmptyCompletion(svc, h.pendingMsgID)
		}
		h.timer = nil
	}
	h.pendingMsgID = nil

	// Check if content is same as last request (duplicate trigger)
	contentKey := content.ContentBefore
	if h.lastContent == contentKey && time.Since(h.lastTrigger) < 500*time.Millisecond {
		svc.Logger.Log("skipping duplicate completion request")
		h.sendEmptyCompletion(svc, msg.ID)
		return
	}
	h.lastContent = contentKey
	h.lastTrigger = time.Now()

	// Capture values for the goroutine
	reqID := h.requestID.Add(1)
	version := buffer.Version
	uri := params.TextDocument.URI
	languageID := buffer.LanguageID

	ctx, cancel := context.WithCancel(context.Background())
	h.cancelCurrent = cancel
	h.pendingMsgID = msg.ID

	h.timer = time.AfterFunc(time.Duration(h.cfg.Debounce)*time.Millisecond, func() {
		h.executeCompletion(ctx, svc, msg, params, version, uri, languageID, content, reqID)
	})
}

func (h *CompletionHandler) executeCompletion(ctx context.Context, svc *lsp.Service, msg *lsp.JSONRPCMessage, params lsp.CompletionParams, version int, uri, languageID string, content util.ContentParts, reqID uint64) {
	defer func() {
		if r := recover(); r != nil {
			svc.Logger.Log("completion panic:", r)
			h.sendEmptyCompletion(svc, msg.ID)
		}
	}()

	// Check if this request is still current
	if h.requestID.Load() != reqID {
		svc.Logger.Log("skipping stale completion request")
		h.sendEmptyCompletion(svc, msg.ID)
		return
	}

	// Check if buffer has changed
	buffer, ok := svc.Buffers.Get(uri)
	if !ok || buffer.Version > version {
		svc.Logger.Log("skipping completion - buffer changed")
		h.sendEmptyCompletion(svc, msg.ID)
		return
	}

	// Re-check context
	if ctx.Err() != nil {
		svc.Logger.Log("completion cancelled before execution")
		h.sendEmptyCompletion(svc, msg.ID)
		return
	}

	svc.Logger.Log("executing completion for language:", languageID)

	// Start progress indicator
	var progress *util.ProgressIndicator
	if h.cfg.EnableProgressSpinner {
		progress = util.NewProgressIndicator(svc, h.cfg)
		progress.Start()
		defer progress.Stop()
	}

	// Create timeout context
	ctx, cancel := context.WithTimeout(ctx, time.Duration(h.cfg.CompletionTimeout)*time.Millisecond)
	defer cancel()

	// Build content after
	contentAfter := content.ContentImmediatelyAfter
	if content.ContentAfter != "" {
		if contentAfter != "" {
			contentAfter += "\n" + content.ContentAfter
		} else {
			contentAfter = content.ContentAfter
		}
	}

	hints, err := h.registry.Completion(ctx, providers.CompletionRequest{
		ContentBefore: content.ContentBefore,
		ContentAfter:  contentAfter,
	}, uri, languageID, h.cfg.NumSuggestions)

	if err != nil {
		if ctx.Err() != nil {
			svc.Logger.Log("completion cancelled:", ctx.Err())
		} else {
			svc.Logger.Log("completion error:", err.Error())
		}
		h.sendEmptyCompletion(svc, msg.ID)
		return
	}

	// Filter out empty or invalid completions
	validHints := make([]string, 0, len(hints))
	for _, hint := range hints {
		cleaned := strings.TrimSpace(hint)
		if cleaned != "" && len(cleaned) >= 2 {
			validHints = append(validHints, hint)
		}
	}

	svc.Logger.Log("completion results:", len(validHints))

	if len(validHints) == 0 {
		h.sendEmptyCompletion(svc, msg.ID)
		return
	}

	items := make([]lsp.CompletionItem, 0, len(validHints))
	for i, hint := range validHints {
		item := h.buildCompletionItem(hint, content, params.Position, i)
		items = append(items, item)
	}

	svc.Send(&lsp.JSONRPCMessage{
		ID: msg.ID,
		Result: lsp.CompletionList{
			IsIncomplete: false,
			Items:        items,
		},
	})
}

func (h *CompletionHandler) buildCompletionItem(hint string, content util.ContentParts, position lsp.Position, index int) lsp.CompletionItem {
	// Trim leading newlines and trailing whitespace, preserve leading spaces
	hint = strings.TrimLeft(hint, "\n")
	hint = strings.TrimRight(hint, " \t\n")

	// Get the last line before cursor for overlap detection
	lastLineTrimmed := strings.TrimSpace(content.LastLine)

	// Check if hint starts with part of the last line (model repeating context)
	if lastLineTrimmed != "" && strings.HasPrefix(strings.TrimSpace(hint), lastLineTrimmed) {
		hint = strings.TrimSpace(hint[len(lastLineTrimmed):])
	}

	lines := strings.Split(hint, "\n")

	// Calculate end position
	endLine := position.Line + len(lines) - 1
	endChar := len(lines[len(lines)-1])
	if endLine == position.Line {
		endChar += position.Character
	}

	// Build label (first line, truncated) with AI prefix
	label := "AI: " + lines[0]
	if len(label) > 40 {
		label = label[:40] + "..."
	}

	// Handle overlap with content after cursor
	var additionalEdits []lsp.TextEdit
	overlapLen := findOverlapSuffix(hint, content.ContentImmediatelyAfter)

	if overlapLen > 0 {
		additionalEdits = append(additionalEdits, lsp.TextEdit{
			Range: lsp.Range{
				Start: lsp.Position{Line: endLine, Character: endChar},
				End:   lsp.Position{Line: endLine, Character: endChar + overlapLen},
			},
			NewText: "",
		})
	}

	return lsp.CompletionItem{
		Label:            label,
		Kind:             1, // Text
		Detail:           hint,
		InsertTextFormat: 1, // PlainText
		TextEdit: &lsp.TextEdit{
			Range: lsp.Range{
				Start: position,
				End:   position,
			},
			NewText: hint,
		},
		SortText:            "", // Empty string sorts before all other values
		Preselect:           true, // Preselect all AI completions
		AdditionalTextEdits: additionalEdits,
	}
}

func findOverlapSuffix(hint, suffix string) int {
	if suffix == "" {
		return 0
	}

	hint = strings.TrimRight(hint, " \t")
	maxOverlap := len(hint)
	if len(suffix) < maxOverlap {
		maxOverlap = len(suffix)
	}

	for i := maxOverlap; i > 0; i-- {
		if hint[len(hint)-i:] == suffix[:i] {
			return i
		}
	}

	return 0
}

func (h *CompletionHandler) sendEmptyCompletion(svc *lsp.Service, id *int) {
	svc.Send(&lsp.JSONRPCMessage{
		ID: id,
		Result: lsp.CompletionList{
			IsIncomplete: false,
			Items:        []lsp.CompletionItem{},
		},
	})
}
