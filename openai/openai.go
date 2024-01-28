package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmorganca/ollama/api"
)

type Error struct {
	Message string      `json:"message"`
	Type    string      `json:"type"`
	Param   interface{} `json:"param"`
	Code    *string     `json:"code"`
}

type ErrorResponse struct {
	Error Error `json:"error"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Message `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ResponseFormat struct {
	Type string `json:"type"`
}

type Request struct {
	Model            string
	Messages         []Message       `json:"messages"`
	Stream           bool            `json:"stream"`
	MaxTokens        *int            `json:"max_tokens"`
	Seed             *int            `json:"seed"`
	Stop             any             `json:"stop"`
	Temperature      *float64        `json:"temperature"`
	FrequencyPenalty *float64        `json:"frequency_penalty"`
	PresencePenalty  *float64        `json:"presence_penalty_penalty"`
	TopP             *float64        `json:"top_p"`
	ResponseFormat   *ResponseFormat `json:"response_format"`
}

type Completion struct {
	Id                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	SystemFingerprint string   `json:"system_fingerprint"`
	Choices           []Choice `json:"choices"`
	Usage             Usage    `json:"usage,omitempty"`
}

type Chunk struct {
	Id                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	SystemFingerprint string        `json:"system_fingerprint"`
	Choices           []ChunkChoice `json:"choices"`
}

func NewError(code int, message string) ErrorResponse {
	var etype string
	switch code {
	case 400:
		etype = "invalid_request_error"
	case 404:
		etype = "not_found_error"
	default:
		etype = "api_error"
	}

	return ErrorResponse{Error{Type: etype, Message: message}}
}

func toCompletion(id string, r api.ChatResponse) Completion {
	return Completion{
		Id:                id,
		Object:            "chat.completion",
		Created:           r.CreatedAt.Unix(),
		Model:             r.Model,
		SystemFingerprint: "fp_ollama",
		Choices: []Choice{{
			Index:   0,
			Message: Message{Role: r.Message.Role, Content: r.Message.Content},
			FinishReason: func(done bool) *string {
				if done {
					reason := "stop"
					return &reason
				}
				return nil
			}(r.Done),
		}},
		Usage: Usage{
			// TODO: ollama returns 0 for prompt eval if the prompt was cached, but openai returns the actual count
			PromptTokens:     r.PromptEvalCount,
			CompletionTokens: r.EvalCount,
			TotalTokens:      r.PromptEvalCount + r.EvalCount,
		},
	}
}

func toChunk(id string, r api.ChatResponse) Chunk {
	return Chunk{
		Id:                id,
		Object:            "chat.completion.chunk",
		Created:           time.Now().Unix(),
		Model:             r.Model,
		SystemFingerprint: "fp_ollama",
		Choices: []ChunkChoice{
			{
				Index: 0,
				Delta: Message{Role: "assistant", Content: r.Message.Content},
				FinishReason: func(done bool) *string {
					if done {
						reason := "stop"
						return &reason
					}
					return nil
				}(r.Done),
			},
		},
	}
}

func fromRequest(r Request) api.ChatRequest {
	var messages []api.Message
	for _, msg := range r.Messages {
		messages = append(messages, api.Message{Role: msg.Role, Content: msg.Content})
	}

	options := make(map[string]interface{})

	switch stop := r.Stop.(type) {
	case string:
		options["stop"] = []string{stop}
	case []interface{}:
		var stops []string
		for _, s := range stop {
			if str, ok := s.(string); ok {
				stops = append(stops, str)
			}
		}
		options["stop"] = stops
	}

	if r.MaxTokens != nil {
		options["num_predict"] = *r.MaxTokens
	}

	if r.Temperature != nil {
		options["temperature"] = *r.Temperature
	}

	if r.Seed != nil {
		options["seed"] = *r.Seed

		// temperature=0 is required for reproducible outputs
		options["temperature"] = 0.0
	}

	if r.FrequencyPenalty != nil {
		options["frequency_penalty"] = (*r.FrequencyPenalty + 2.0) / 4.0
	}

	if r.PresencePenalty != nil {
		options["presence_penalty"] = (*r.PresencePenalty + 2.0) / 4.0
	}

	if r.TopP != nil {
		options["top_p"] = *r.TopP
	}

	var format string
	if r.ResponseFormat != nil && r.ResponseFormat.Type == "json_object" {
		format = "json"
	}

	return api.ChatRequest{
		Model:    r.Model,
		Messages: messages,
		Format:   format,
		Options:  options,
		Stream:   &r.Stream,
	}
}

type writer struct {
	stream bool
	id     string
	gin.ResponseWriter
}

func (w *writer) Write(data []byte) (int, error) {
	if w.ResponseWriter.Status() != http.StatusOK {
		var serr api.StatusError
		if err := json.Unmarshal(data, &serr); err == nil {
			// error
			w.ResponseWriter.Header().Set("Content-Type", "application/json")
			if d, err := json.Marshal(NewError(http.StatusInternalServerError, serr.Error())); err == nil {
				return w.ResponseWriter.Write(d)
			}
		}

		return len(data), nil
	}

	var chatResponse api.ChatResponse
	if err := json.Unmarshal(data, &chatResponse); err == nil {
		if !w.stream {
			// chat completion
			if d, err := json.Marshal(toCompletion(w.id, chatResponse)); err == nil {
				w.ResponseWriter.Header().Set("Content-Type", "application/json")
				return w.ResponseWriter.Write(d)
			}
		}

		// chat chunk
		if d, err := json.Marshal(toChunk(w.id, chatResponse)); err == nil {
			w.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
			_, err := w.ResponseWriter.Write([]byte(fmt.Sprintf("data: %s\n\n", d)))
			if err != nil {
				return 0, err
			}

			if chatResponse.Done {
				return w.ResponseWriter.Write([]byte("data: [DONE]\n\n"))
			}
		}
	}

	return len(data), nil
}

func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		var req Request
		err := c.ShouldBindJSON(&req)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, NewError(http.StatusBadRequest, err.Error()))
			return
		}

		if len(req.Messages) == 0 {
			c.AbortWithStatusJSON(http.StatusBadRequest, NewError(http.StatusBadRequest, "[] is too short - 'messages'"))
			return
		}

		data, err := json.Marshal(fromRequest(req))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, NewError(http.StatusInternalServerError, err.Error()))
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewBuffer(data))

		w := &writer{
			ResponseWriter: c.Writer,
			stream:         req.Stream,
			id:             fmt.Sprintf("chatcmpl-%d", rand.Intn(999)),
		}

		c.Writer = w

		c.Next()
	}
}