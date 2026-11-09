package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/PHPCraftdream/cc-go-proxy/internal/config"
	"github.com/PHPCraftdream/cc-go-proxy/internal/converter"
	"github.com/PHPCraftdream/cc-go-proxy/pkg/models"
	"github.com/gofiber/fiber/v2"
)

// addOpenRouterHeaders adds OpenRouter-specific HTTP headers for better rate limits.
// Sets HTTP-Referer and X-Title headers when configured, which helps with OpenRouter's
// rate limiting and usage tracking.
func addOpenRouterHeaders(req *http.Request, cfg *config.Config) {
	if cfg.OpenRouterAppURL != "" {
		req.Header.Set("HTTP-Referer", cfg.OpenRouterAppURL)
	}
	if cfg.OpenRouterAppName != "" {
		req.Header.Set("X-Title", cfg.OpenRouterAppName)
	}
}

// handleMessages is the main handler for /v1/messages endpoint.
// It parses Claude requests, converts them to OpenAI format, and routes to either
// streaming or non-streaming handlers based on the request's stream parameter.
func handleMessages(c *fiber.Ctx, cfg *config.Config) error {
	// Debug: Log raw request
	if cfg.Debug {
		fmt.Printf("\n=== CLAUDE REQUEST ===\n%s\n===================\n", string(c.Body()))
	}

	// Parse Claude request
	var claudeReq models.ClaudeRequest
	if err := c.BodyParser(&claudeReq); err != nil {
		// Log the error and raw body for debugging
		fmt.Printf("[ERROR] Failed to parse request body: %v\n", err)
		fmt.Printf("[ERROR] Raw body: %s\n", string(c.Body()))
		return c.Status(400).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "invalid_request_error",
				"message": fmt.Sprintf("Invalid request body: %v", err),
			},
		})
	}

	// Validate API key (if configured)
	if cfg.AnthropicAPIKey != "" {
		apiKey := ownString(c.Get("x-api-key"))
		if apiKey != cfg.AnthropicAPIKey {
			return c.Status(401).JSON(fiber.Map{
				"type": "error",
				"error": fiber.Map{
					"type":    "authentication_error",
					"message": "Invalid API key",
				},
			})
		}
	}

	// Convert Claude request to OpenAI format
	openaiReq, err := converter.ConvertRequest(claudeReq, cfg)
	if err == nil {
		c.Locals(localsModel, claudeReq.Model+" -> "+openaiReq.Model)
	}
	if err != nil {
		return c.Status(400).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "invalid_request_error",
				"message": err.Error(),
			},
		})
	}

	// Debug: Log converted OpenAI request
	if cfg.Debug {
		openaiReqJSON, _ := json.MarshalIndent(openaiReq, "", "  ")
		fmt.Printf("\n=== OPENAI REQUEST ===\n%s\n===================\n", string(openaiReqJSON))
		if len(claudeReq.Tools) > 0 {
			fmt.Printf("[DEBUG] Request has %d tools\n", len(claudeReq.Tools))
			for i, tool := range openaiReq.Tools {
				fmt.Printf("[DEBUG] Tool %d: %s\n", i, tool.Function.Name)
			}
		}
	}

	// Debug: Check Stream field
	if cfg.Debug {
		if openaiReq.Stream == nil {
			fmt.Printf("[DEBUG] Stream field is nil\n")
		} else {
			fmt.Printf("[DEBUG] Stream field = %v\n", *openaiReq.Stream)
		}
	}

	// Handle streaming vs non-streaming
	if openaiReq.Stream != nil && *openaiReq.Stream {
		return handleStreamingMessages(c, openaiReq, cfg)
	}

	// Track timing for simple log
	startTime := time.Now()

	// Non-streaming response
	openaiResp, err := callOpenAI(openaiReq, cfg)
	if err != nil {
		return respondOpenAIError(c, cfg, err)
	}

	// Debug: Log OpenAI response
	if cfg.Debug {
		openaiRespJSON, _ := json.MarshalIndent(openaiResp, "", "  ")
		fmt.Printf("\n=== OPENAI RESPONSE ===\n%s\n====================\n", string(openaiRespJSON))
		if len(openaiResp.Choices) > 0 {
			choice := openaiResp.Choices[0]
			fmt.Printf("[DEBUG] OpenAI response has %d tool_calls\n", len(choice.Message.ToolCalls))
			for i, tc := range choice.Message.ToolCalls {
				fmt.Printf("[DEBUG] ToolCall %d: ID=%s, Name=%s\n", i, tc.ID, tc.Function.Name)
			}
		}
	}

	// Convert OpenAI response to Claude format
	claudeResp, err := converter.ConvertResponse(openaiResp, claudeReq.Model)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{
			"type": "error",
			"error": fiber.Map{
				"type":    "api_error",
				"message": fmt.Sprintf("Response conversion error: %v", err),
			},
		})
	}

	// Debug: Log Claude response
	if cfg.Debug {
		claudeRespJSON, _ := json.MarshalIndent(claudeResp, "", "  ")
		fmt.Printf("\n=== CLAUDE RESPONSE ===\n%s\n====================\n\n", string(claudeRespJSON))
		fmt.Printf("[DEBUG] Claude response has %d content blocks\n", len(claudeResp.Content))
		for i, block := range claudeResp.Content {
			fmt.Printf("[DEBUG] Block %d: type=%s", i, block.Type)
			if block.Type == "tool_use" {
				fmt.Printf(", name=%s, id=%s", block.Name, block.ID)
			}
			fmt.Printf("\n")
		}
	}

	// Simple log: one-line summary
	if cfg.SimpleLog {
		duration := time.Since(startTime).Seconds()
		tokensPerSec := 0.0
		if duration > 0 && claudeResp.Usage.OutputTokens > 0 {
			tokensPerSec = float64(claudeResp.Usage.OutputTokens) / duration
		}
		timestamp := time.Now().Format("15:04:05")
		fmt.Printf("[%s] [REQ] %s model=%s in=%d out=%d tok/s=%.1f\n",
			timestamp,
			cfg.OpenAIBaseURL,
			openaiReq.Model,
			claudeResp.Usage.InputTokens,
			claudeResp.Usage.OutputTokens,
			tokensPerSec)
	}

	return c.JSON(claudeResp)
}

// handleStreamingMessages handles streaming SSE responses from the provider.
// It forwards the OpenAI request, receives streaming chunks, and converts them to
// Claude's SSE event format in real-time using streamOpenAIToClaude.
func handleStreamingMessages(c *fiber.Ctx, openaiReq *models.OpenAIRequest, cfg *config.Config) error {
	startTime := time.Now()
	streamCtx, cancel := context.WithCancel(upstreamCtx(context.Background(), cfg))
	resp, err := callOpenAIStream(openaiReq, cfg, streamCtx)
	if err != nil {
		cancel()
		return respondOpenAIError(c, cfg, err)
	}

	// Set SSE headers
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	release := deferActive(c)
	c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer release()
		if cfg.Debug {
			fmt.Printf("[DEBUG] StreamWriter: Starting\n")
			fmt.Printf("[DEBUG] StreamWriter: Got response, starting streamOpenAIToClaude conversion\n")
		}

		// The handler owns cancellation and response-body cleanup for the stream.
		err := streamOpenAIResponse(w, resp, openaiReq.Model, cfg, startTime, cancel)
		if cfg.Debug {
			if err != nil {
				fmt.Printf("[DEBUG] StreamWriter: Stream failed: %v\n", err)
			} else {
				fmt.Printf("[DEBUG] StreamWriter: Completed\n")
			}
		}
	})

	return nil
}

// ToolCallState tracks the state of a tool call during streaming
type ToolCallState struct {
	ID          string // Tool call ID from OpenAI
	Name        string // Function name
	ClaudeIndex int    // The content block index for Claude
	Started     bool   // Flag if content_block_start was sent
}

// streamOpenAIResponse owns the upstream body and cancels its request when the
// stream is closed.
func streamOpenAIResponse(w *bufio.Writer, resp *http.Response, providerModel string, cfg *config.Config, startTime time.Time, cancel context.CancelFunc) (err error) {
	defer cancel()
	defer func() {
		if closeErr := resp.Body.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	return streamOpenAIToClaude(w, resp.Body, providerModel, cfg, startTime)
}

// streamOpenAIToClaude converts OpenAI streaming responses to Claude's SSE event format.
//
// It processes the OpenAI SSE stream chunk-by-chunk, generating the proper sequence of
// Claude events: message_start, content_block_start, content_block_delta, content_block_stop,
// message_delta, and message_stop.
//
// Handles:
//   - Thinking blocks from reasoning models (OpenRouter's reasoning_details, OpenAI's reasoning_content)
//   - Text content deltas
//   - Tool call deltas (forwards each JSON argument fragment)
//   - Token usage tracking and throughput calculation for simple log mode
//
// The function maintains state to track content block indices and tool call starts,
// and ensures proper event ordering for Claude Code compatibility.
func streamOpenAIToClaude(w *bufio.Writer, reader io.Reader, providerModel string, cfg *config.Config, startTime time.Time) error {
	if cfg.Debug {
		fmt.Printf("[DEBUG] streamOpenAIToClaude: Starting conversion\n")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // Increase buffer size

	// State variables
	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	textBlockIndex := 1   // Text block is index 1 (thinking is 0)
	toolBlockCounter := 2 // Tool calls start at index 2
	currentToolCalls := make(map[int]*ToolCallState)
	finalStopReason := "end_turn"
	usageData := map[string]interface{}{
		"input_tokens":                0,
		"output_tokens":               0,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
		"cache_creation": map[string]interface{}{
			"ephemeral_5m_input_tokens": 0,
			"ephemeral_1h_input_tokens": 0,
		},
	}

	// Thinking block tracking (to show thinking indicator in Claude Code)
	thinkingBlockIndex := 0 // Thinking block is always index 0
	thinkingBlockStarted := false
	thinkingBlockHasContent := false
	textBlockStarted := false // Track if we've sent text block_start
	startThinkingBlock := func(thinking string) error {
		if thinking == "" {
			return nil
		}
		if !thinkingBlockStarted {
			if err := writeSSEEvent(w, "content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": thinkingBlockIndex,
				"content_block": map[string]interface{}{
					"type":     "thinking",
					"thinking": "",
				},
			}); err != nil {
				return err
			}
			thinkingBlockStarted = true
			if err := w.Flush(); err != nil {
				return err
			}
		}
		if err := writeSSEEvent(w, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": thinkingBlockIndex,
			"delta": map[string]interface{}{
				"type":     "thinking_delta",
				"thinking": thinking,
			},
		}); err != nil {
			return err
		}
		thinkingBlockHasContent = true
		return w.Flush()
	}

	// Send initial SSE events
	if err := writeSSEEvent(w, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         providerModel,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":                0,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
				"cache_creation": map[string]interface{}{
					"ephemeral_5m_input_tokens": 0,
					"ephemeral_1h_input_tokens": 0,
				},
			},
		},
	}); err != nil {
		return err
	}

	if err := writeSSEEvent(w, "ping", map[string]interface{}{
		"type": "ping",
	}); err != nil {
		return err
	}

	if err := w.Flush(); err != nil {
		return err
	}

	// Process streaming chunks
	terminal := false
	for scanner.Scan() {
		line := scanner.Bytes()

		// Skip empty lines and comments
		if len(line) == 0 || bytes.HasPrefix(line, []byte(":")) {
			continue
		}

		// Parse data lines and stop only on the exact [DONE] payload.
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}

		dataJSON := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(dataJSON, []byte("[DONE]")) {
			terminal = true
			break
		}

		var chunk map[string]interface{}
		if err := json.Unmarshal(dataJSON, &chunk); err != nil {
			continue
		}

		// Log every chunk to see what OpenRouter is sending
		if cfg.Debug {
			fmt.Printf("[DEBUG] Raw chunk from OpenRouter: %s\n", dataJSON)
		}

		// Handle usage data
		if usage, ok := chunk["usage"].(map[string]interface{}); ok {
			if cfg.Debug {
				usageJSON, _ := json.Marshal(usage)
				fmt.Printf("[DEBUG] Received usage from OpenAI: %s\n", string(usageJSON))
			}

			// Convert float64 to int for token counts (JSON unmarshals numbers as float64)
			inputTokens := 0
			outputTokens := 0
			if val, ok := usage["prompt_tokens"].(float64); ok {
				inputTokens = int(val)
			}
			if val, ok := usage["completion_tokens"].(float64); ok {
				outputTokens = int(val)
			}

			usageData = map[string]interface{}{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
			}

			// Add cache metrics if present
			if promptTokensDetails, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
				if cachedTokens, ok := promptTokensDetails["cached_tokens"].(float64); ok && cachedTokens > 0 {
					usageData["cache_read_input_tokens"] = int(cachedTokens)
				}
			}
			if cfg.Debug {
				usageDataJSON, _ := json.Marshal(usageData)
				fmt.Printf("[DEBUG] Accumulated usageData: %s\n", string(usageDataJSON))
			}
		}

		// Extract delta from choices
		choices, ok := chunk["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}

		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			return writeSSEError(w, fmt.Sprintf("invalid stream choice: expected object, got %T", choices[0]))
		}

		finishReason, hasFinishReason := choice["finish_reason"].(string)
		if hasFinishReason && finishReason != "" {
			terminal = true
			switch finishReason {
			case "length":
				finalStopReason = "max_tokens"
			case "tool_calls", "function_call":
				finalStopReason = "tool_use"
			case "stop":
				finalStopReason = "end_turn"
			default:
				finalStopReason = "end_turn"
			}
		}

		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			continue
		}

		// Handle reasoning delta (thinking blocks)
		// Support both OpenRouter and OpenAI formats:
		// - OpenRouter: delta.reasoning_details (array)
		// - OpenAI o1/o3: delta.reasoning_content (string)

		// First, check for OpenAI's reasoning_content format (o1/o3 models)
		if reasoningContent, ok := delta["reasoning_content"].(string); ok && reasoningContent != "" {
			if err := startThinkingBlock(reasoningContent); err != nil {
				return err
			}
		}

		// Then, check for OpenRouter's reasoning_details format
		// Only process reasoning_details if we haven't already processed reasoning field
		if reasoningDetailsRaw, ok := delta["reasoning_details"]; ok && delta["reasoning"] == nil {
			if reasoningDetails, ok := reasoningDetailsRaw.([]interface{}); ok && len(reasoningDetails) > 0 {
				for _, detailRaw := range reasoningDetails {
					if detail, ok := detailRaw.(map[string]interface{}); ok {
						// Extract reasoning text from the detail
						thinkingText := ""
						detailType, _ := detail["type"].(string)

						switch detailType {
						case "reasoning.text":
							if text, ok := detail["text"].(string); ok {
								thinkingText = text
							}
						case "reasoning.summary":
							if summary, ok := detail["summary"].(string); ok {
								thinkingText = summary
							}
						case "reasoning.encrypted":
							// Skip encrypted/redacted reasoning in streaming
							continue
						}

						if thinkingText != "" {
							if err := startThinkingBlock(thinkingText); err != nil {
								return err
							}
						}
					}
				}
			}
		}

		// Handle reasoning field directly (simpler format from some models)
		if reasoning, ok := delta["reasoning"].(string); ok && reasoning != "" {
			if err := startThinkingBlock(reasoning); err != nil {
				return err
			}
		}

		// Handle text delta
		if content, ok := delta["content"].(string); ok && content != "" {
			// Send content_block_start for text block on first text delta
			if !textBlockStarted {
				if err := writeSSEEvent(w, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": textBlockIndex,
					"content_block": map[string]interface{}{
						"type": "text",
						"text": "",
					},
				}); err != nil {
					return err
				}
				textBlockStarted = true
				if err := w.Flush(); err != nil {
					return err
				}
			}

			if err := writeSSEEvent(w, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": textBlockIndex,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": content,
				},
			}); err != nil {
				return err
			}
			if err := w.Flush(); err != nil {
				return err
			}
		}

		// Handle tool call deltas
		if toolCallsRaw, ok := delta["tool_calls"]; ok {
			// Debug: Log raw tool_calls from provider
			if cfg.Debug {
				toolCallsJSON, _ := json.Marshal(toolCallsRaw)
				fmt.Printf("[DEBUG] Raw tool_calls delta: %s\n", string(toolCallsJSON))
			}

			toolCalls, ok := toolCallsRaw.([]interface{})
			if ok && len(toolCalls) > 0 {
				for _, tcRaw := range toolCalls {
					tcDelta, ok := tcRaw.(map[string]interface{})
					if !ok {
						continue
					}

					// Get tool call index
					tcIndex := 0
					if idx, ok := tcDelta["index"].(float64); ok {
						tcIndex = int(idx)
					}

					// Initialize tool call tracking if not exists
					if _, exists := currentToolCalls[tcIndex]; !exists {
						currentToolCalls[tcIndex] = &ToolCallState{
							ID:          "",
							Name:        "",
							ClaudeIndex: 0,
							Started:     false,
						}
					}

					toolCall := currentToolCalls[tcIndex]

					// Update tool call ID if provided
					if id, ok := tcDelta["id"].(string); ok {
						toolCall.ID = id
					}

					// Update function name
					if functionData, ok := tcDelta["function"].(map[string]interface{}); ok {
						if name, ok := functionData["name"].(string); ok {
							toolCall.Name = name
						}

						// Start content block when we have complete initial data
						if toolCall.ID != "" && toolCall.Name != "" && !toolCall.Started {
							toolBlockCounter++
							claudeIndex := textBlockIndex + toolBlockCounter
							toolCall.ClaudeIndex = claudeIndex
							toolCall.Started = true

							if err := writeSSEEvent(w, "content_block_start", map[string]interface{}{
								"type":  "content_block_start",
								"index": claudeIndex,
								"content_block": map[string]interface{}{
									"type":  "tool_use",
									"id":    toolCall.ID,
									"name":  toolCall.Name,
									"input": map[string]interface{}{},
								},
							}); err != nil {
								return err
							}
							if err := w.Flush(); err != nil {
								return err
							}
						}

						// Handle function arguments
						if args, ok := functionData["arguments"].(string); ok && toolCall.Started && args != "" {
							if err := writeSSEEvent(w, "content_block_delta", map[string]interface{}{
								"type":  "content_block_delta",
								"index": toolCall.ClaudeIndex,
								"delta": map[string]interface{}{
									"type":         "input_json_delta",
									"partial_json": args,
								},
							}); err != nil {
								return err
							}
							if err := w.Flush(); err != nil {
								return err
							}
						}
					}
				}
			}
		}

		// Continue processing to capture usage chunk (don't break)
	}

	if err := scanner.Err(); err != nil {
		return writeSSEError(w, fmt.Sprintf("stream read error: %v", err))
	}
	if !terminal {
		return writeSSEError(w, "stream ended before a terminal marker")
	}

	// Send final SSE events

	// Send content_block_stop for text block if it was started
	if textBlockStarted {
		if err := writeSSEEvent(w, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": textBlockIndex,
		}); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	// Send content_block_stop for each tool call
	for _, toolData := range currentToolCalls {
		// Check both Started AND claude_index is not None
		if toolData.Started && toolData.ClaudeIndex != 0 {
			if err := writeSSEEvent(w, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": toolData.ClaudeIndex,
			}); err != nil {
				return err
			}
			if err := w.Flush(); err != nil {
				return err
			}
		}
	}

	// Send content_block_stop for thinking block if it had content
	if thinkingBlockStarted && thinkingBlockHasContent {
		if err := writeSSEEvent(w, "content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": thinkingBlockIndex,
		}); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	// Debug: Check if usage data was received
	if cfg.Debug {
		inputTokens, _ := usageData["input_tokens"].(int)
		outputTokens, _ := usageData["output_tokens"].(int)
		if inputTokens == 0 && outputTokens == 0 {
			fmt.Printf("[DEBUG] OpenRouter streaming: Usage data unavailable (expected limitation of streaming API)\n")
		}
	}

	// Send message_delta with stop_reason and accumulated usage data
	// NOTE: We send the actual accumulated usage to fix the "0 tokens" issue in Claude Code
	if cfg.Debug {
		usageDataJSON, _ := json.Marshal(usageData)
		fmt.Printf("[DEBUG] Sending message_delta with usageData: %s\n", string(usageDataJSON))
	}
	if err := writeSSEEvent(w, "message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   finalStopReason,
			"stop_sequence": nil,
		},
		"usage": usageData,
	}); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Send message_stop
	if err := writeSSEEvent(w, "message_stop", map[string]interface{}{
		"type": "message_stop",
	}); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Simple log: one-line summary
	if cfg.SimpleLog {
		inputTokens := 0
		outputTokens := 0

		// Try to extract tokens from various possible formats
		if val, ok := usageData["input_tokens"].(int); ok {
			inputTokens = val
		} else if val, ok := usageData["input_tokens"].(float64); ok {
			inputTokens = int(val)
		}

		if val, ok := usageData["output_tokens"].(int); ok {
			outputTokens = val
		} else if val, ok := usageData["output_tokens"].(float64); ok {
			outputTokens = int(val)
		}

		// Debug: show what we actually have in usageData
		if cfg.Debug {
			fmt.Printf("[DEBUG] usageData: %+v\n", usageData)
		}

		// Calculate tokens per second
		duration := time.Since(startTime).Seconds()
		tokensPerSec := 0.0
		if duration > 0 && outputTokens > 0 {
			tokensPerSec = float64(outputTokens) / duration
		}

		timestamp := time.Now().Format("15:04:05")
		fmt.Printf("[%s] [REQ] %s model=%s in=%d out=%d tok/s=%.1f\n",
			timestamp,
			cfg.OpenAIBaseURL,
			providerModel,
			inputTokens,
			outputTokens,
			tokensPerSec)
	}

	return nil
}

// writeSSEEvent writes a Server-Sent Event.
func writeSSEEvent(w *bufio.Writer, event string, data interface{}) error {
	dataJSON, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", string(dataJSON))
	return err
}

// writeSSEError writes an error event and flushes it.
func writeSSEError(w *bufio.Writer, message string) error {
	if err := writeSSEEvent(w, "error", map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": message,
		},
	}); err != nil {
		return err
	}
	return w.Flush()
}

// callOpenAI makes an HTTP request to the OpenAI API with automatic retry logic
// for max_completion_tokens parameter errors. Uses per-model capability caching.
func callOpenAI(req *models.OpenAIRequest, cfg *config.Config) (*models.OpenAIResponse, error) {
	// Try the request with the configured parameters
	resp, err := callOpenAIInternal(req, cfg)
	if err != nil {
		// Check if this is a max_tokens parameter error
		if isMaxTokensParameterError(err.Error()) {
			if cfg.Debug {
				fmt.Printf("[DEBUG] Detected max_completion_tokens parameter error for model %s, retrying without it\n", req.Model)
			}
			// Retry without max_completion_tokens and cache the capability per model
			return retryWithoutMaxCompletionTokens(req, cfg)
		}
		// Other errors - return as-is
		return nil, err
	}

	// Success on first try - cache that this (provider, model) supports max_completion_tokens
	// Only cache if we actually sent max_completion_tokens
	if req.MaxCompletionTokens > 0 {
		cacheKey := config.CacheKey{
			BaseURL: cfg.OpenAIBaseURL,
			Model:   req.Model,
		}
		config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
			UsesMaxCompletionTokens: true,
		})
		if cfg.Debug {
			fmt.Printf("[DEBUG] Cached: model %s supports max_completion_tokens\n", req.Model)
		}
	}

	return resp, nil
}

// callOpenAIStream makes a streaming HTTP request with retry logic for parameter errors.
// Uses per-model capability caching.
func callOpenAIStream(req *models.OpenAIRequest, cfg *config.Config, ctx context.Context) (*http.Response, error) {
	// Try with configured parameters
	resp, err := callOpenAIStreamInternal(req, cfg, ctx)
	if err != nil {
		// Check if this is a max_tokens parameter error
		if isMaxTokensParameterError(err.Error()) {
			if cfg.Debug {
				fmt.Printf("[DEBUG] Detected max_completion_tokens parameter error in stream for model %s, retrying without it\n", req.Model)
			}
			// Create retry request without max tokens
			retryReq := *req
			retryReq.MaxCompletionTokens = 0
			retryReq.MaxTokens = 0

			// Cache that this (provider, model) doesn't support max_completion_tokens
			cacheKey := config.CacheKey{
				BaseURL: cfg.OpenAIBaseURL,
				Model:   req.Model,
			}
			config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
				UsesMaxCompletionTokens: false,
			})

			return callOpenAIStreamInternal(&retryReq, cfg, ctx)
		}
		return nil, err
	}

	// Success - cache capability if we sent max_completion_tokens
	if req.MaxCompletionTokens > 0 {
		cacheKey := config.CacheKey{
			BaseURL: cfg.OpenAIBaseURL,
			Model:   req.Model,
		}
		config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
			UsesMaxCompletionTokens: true,
		})
		if cfg.Debug {
			fmt.Printf("[DEBUG] Cached: model %s supports max_completion_tokens (streaming)\n", req.Model)
		}
	}

	return resp, nil
}

// callOpenAIStreamInternal makes a streaming HTTP request without retry logic
func callOpenAIStreamInternal(req *models.OpenAIRequest, cfg *config.Config, ctx context.Context) (*http.Response, error) {
	// Marshal request to JSON
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Build API URL
	apiURL := cfg.OpenAIBaseURL + "/chat/completions"

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", apiURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")

	// Skip auth for Ollama (localhost)
	if !cfg.IsLocalhost() {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	}

	// OpenRouter-specific headers
	if cfg.DetectProvider() == config.ProviderOpenRouter {
		addOpenRouterHeaders(httpReq, cfg)
	}

	client := &http.Client{Transport: openAITransport}

	// Make request with a deadline that remains active while the body is read.
	resp, err := doOpenAIRequest(ctx, client, httpReq, 300*time.Second)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, newOpenAIUpstreamError(resp.StatusCode, resp.Header, body)
	}

	return resp, nil
}

// isMaxTokensParameterError checks if the error message indicates an unsupported
// max_tokens or max_completion_tokens parameter issue.
// Uses broad keyword matching to handle different error message formats across providers.
// No status code checking - relies on message content alone.
func isMaxTokensParameterError(errorMessage string) bool {
	errorLower := strings.ToLower(errorMessage)

	// Check for parameter error indicators
	hasParamIndicator := strings.Contains(errorLower, "parameter") ||
		strings.Contains(errorLower, "unsupported") ||
		strings.Contains(errorLower, "invalid")

	// Check for our specific parameter names
	hasOurParam := strings.Contains(errorLower, "max_tokens") ||
		strings.Contains(errorLower, "max_completion_tokens")

	// Require both indicators to reduce false positives
	return hasParamIndicator && hasOurParam
}

// retryWithoutMaxCompletionTokens attempts the request again without max_completion_tokens.
// Caches the result per (provider, model) combination for future requests.
func retryWithoutMaxCompletionTokens(req *models.OpenAIRequest, cfg *config.Config) (*models.OpenAIResponse, error) {
	// Create a copy of the request without max_completion_tokens
	retryReq := *req
	retryReq.MaxCompletionTokens = 0
	retryReq.MaxTokens = 0 // Also clear max_tokens to avoid issues

	if cfg.Debug {
		fmt.Printf("[DEBUG] Retrying without max_completion_tokens/max_tokens for model: %s\n", req.Model)
	}

	// Cache that this specific (provider, model) doesn't support max_completion_tokens
	cacheKey := config.CacheKey{
		BaseURL: cfg.OpenAIBaseURL,
		Model:   req.Model,
	}
	config.SetModelCapabilities(cacheKey, &config.ModelCapabilities{
		UsesMaxCompletionTokens: false,
	})

	// Make the retry request
	return callOpenAIInternal(&retryReq, cfg)
}

// callOpenAIInternal is the internal implementation without retry logic
func callOpenAIInternal(req *models.OpenAIRequest, cfg *config.Config) (*models.OpenAIResponse, error) {
	// Marshal request to JSON
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Build API URL
	apiURL := cfg.OpenAIBaseURL + "/chat/completions"

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(upstreamCtx(context.Background(), cfg), "POST", apiURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")

	// Skip auth for Ollama (localhost) - Ollama doesn't require authentication
	if !cfg.IsLocalhost() {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	}

	// OpenRouter-specific headers for better rate limits
	if cfg.DetectProvider() == config.ProviderOpenRouter {
		addOpenRouterHeaders(httpReq, cfg)
	}

	client := &http.Client{Transport: openAITransport}

	// Make request with a deadline that remains active while the body is read.
	resp, err := doOpenAIRequest(upstreamCtx(context.Background(), cfg), client, httpReq, 90*time.Second)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check for errors
	if resp.StatusCode != http.StatusOK {
		return nil, newOpenAIUpstreamError(resp.StatusCode, resp.Header, respBody)
	}

	// Parse response
	var openaiResp models.OpenAIResponse
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &openaiResp, nil
}

func doOpenAIRequest(ctx context.Context, client *http.Client, req *http.Request, timeout time.Duration) (*http.Response, error) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	resp, err := client.Do(req.WithContext(requestCtx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelReadCloser) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

// handleCountTokens answers with a length-based estimate: neither the Codex
// backend nor Chat Completions has a counting endpoint.
func handleCountTokens(c *fiber.Ctx, cfg *config.Config) error {
	return c.JSON(fiber.Map{
		"input_tokens": estimateTokens(c.Body()),
	})
}
