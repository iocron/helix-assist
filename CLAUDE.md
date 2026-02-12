# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

helix-assist is a Go-based LSP (Language Server Protocol) server that provides AI-powered code completions and code actions for the Helix editor. It supports three LLM providers: OpenAI, Anthropic, and Ollama. The application operates as an LSP server, communicating over stdio using JSON-RPC protocol.

## Build & Development Commands

### Building
```bash
make build           # Build for current platform (output: build/helix-assist)
make install         # Install to $GOPATH/bin
make build-all       # Build for all platforms
make clean          # Remove build artifacts
```

### Testing
```bash
go test ./...                                    # Run standard Go tests (currently minimal)
make run-tests                                   # Run completion tests with test tool
make run-tests PROVIDER=anthropic               # Run tests with specific provider
make build-test                                  # Build the helix-assist-test tool
```

The main testing approach uses a custom test runner (`cmd/helix-assist-test`) that loads test files from `tests/completions/` directory and verifies completion behavior against actual providers.

### Debug Mode
```bash
# Test provider directly without LSP:
./helix-assist --debug-query "package main\n\nfunc " --handler anthropic
```

## Architecture

### Core Components

**LSP Service (`internal/lsp/service.go`)**
- Manages JSON-RPC communication over stdio
- Handles LSP lifecycle events (initialize, didOpen, didChange, shutdown, exit)
- Maintains a BufferStore tracking open document state
- Event-driven architecture using handler registration via `Service.On()`

**Provider Registry (`internal/providers/`)**
- Abstraction layer over LLM providers (OpenAI, Anthropic, Ollama)
- `Provider` interface with two methods:
  - `Completion()`: Generates code completions from cursor context
  - `Chat()`: Powers code actions with conversational requests
- Registry pattern allows switching providers at runtime via config

**Handlers (`internal/handlers/`)**
- `CompletionHandler`: Manages debounced, cancellable completion requests
  - Implements smart context detection (skips dots, comments, empty lines)
  - Deduplication logic to prevent redundant API calls
  - Handles overlap detection between completions and existing code
- `ActionHandler`: Executes code actions (resolve diagnostics, improve code, refactor from comment)
  - Transforms selected code via Chat API
  - Applies edits via LSP workspace edit protocol

**Configuration (`internal/config/config.go`)**
- Dual configuration via environment variables and CLI flags (CLI takes precedence)
- Key settings: handler selection, API keys, model names, timeouts, debounce delays
- Validation ensures required keys are present for selected provider

### LSP Communication Flow

1. Helix sends JSON-RPC messages via stdin
2. `Service.Start()` reads messages, parses headers and JSON payloads
3. Events trigger registered handlers via `Service.emit()`
4. Handlers run in goroutines, call provider APIs, build responses
5. `Service.Send()` writes JSON-RPC responses to stdout

### Completion Request Flow

1. User types trigger character (`{`, `(`, or space by default)
2. Helix sends `textDocument/completion` request
3. `CompletionHandler` extracts cursor context using `util.GetContent()`
4. Request is debounced (default 200ms) and previous requests are cancelled
5. Content before/after cursor is sent to provider's Completion API
6. Response is parsed, overlaps removed, and returned as `CompletionItem[]`

### Code Action Flow

1. User selects code and triggers code action (Space + a in Helix)
2. Helix requests available actions via `textDocument/codeAction`
3. Handler returns list of available commands (resolve diagnostics, improve code, refactor)
4. User selects action, Helix sends `workspace/executeCommand`
5. Handler extracts selected code, sends to provider's Chat API with query
6. Response is applied via `workspace/applyEdit` request

## Key Patterns & Conventions

**Context Cancellation**
- All API requests use context with timeout (default 15s)
- Completion requests are cancellable - typing new characters cancels previous requests
- Prevents stale completions from appearing after user has moved on

**Progress Indicators**
- Optional animated spinner during long operations (enabled by default)
- Uses LSP `$/progress` notifications
- `ProgressIndicator` in `internal/util/progress.go` manages lifecycle

**Buffer Management**
- `BufferStore` maintains in-memory copies of open files
- Tracks URI, content, language ID, and version number
- Version checking prevents race conditions when buffer changes during API call

**Prompt Engineering**
- Prompts in `internal/providers/prompts.go`
- Separate prompts for completions vs chat actions
- Language-specific context included in requests

## Model Configuration

Each provider supports two model configurations:
- **Completion model**: Used for inline code completions (should be fast/cheap)
- **Chat model**: Used for code actions (can be more capable/expensive)

If chat model not specified, falls back to completion model.

**Defaults:**
- OpenAI: `gpt-4.1` (completion), `gpt-5` (chat)
- Anthropic: `claude-haiku-4-5` (completion), `claude-sonnet-4-5` (chat)
- Ollama: `qwen2.5-coder` (both)

## Common Debugging Tasks

**View logs:**
```bash
tail -f ~/.cache/helix-assist.log    # helix-assist logs
tail -f ~/.cache/helix/helix.log     # Helix editor logs
```

**Check LSP communication:**
All LSP messages are logged to the log file. Look for:
- `received:` - incoming requests from Helix
- `sent:` - outgoing responses to Helix

**Test provider without Helix:**
Use `--debug-query` flag to bypass LSP and test provider directly.

## Important Notes

- The project has zero external Go dependencies (pure stdlib)
- Main entry point: `cmd/helix-assist/main.go`
- Test tool entry point: `cmd/helix-assist-test/main.go`
- Ollama provider doesn't require API key (local inference)
- All provider communication is synchronous - no request queuing
- Version info embedded via ldflags during build
