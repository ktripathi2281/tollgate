package mock

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/provider"
)

// maxBodyBytes caps request bodies sent to the mock server.
const maxBodyBytes = 4 << 20

// Handler returns an OpenAI-compatible HTTP API backed by p:
// POST /v1/chat/completions (non-streaming) and GET /v1/models.
//
// Unlike the gateway, it parses requests leniently, as a real provider
// would: fields it doesn't model are ignored.
func Handler(p *Provider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		handleChat(w, r, p)
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, api.NewModelList([]string{"mock-1"}, time.Now()))
	})
	return mux
}

// chatRequest is the part of an OpenAI request the mock reads.
type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	MaxTokens           int  `json:"max_tokens"`
	MaxCompletionTokens int  `json:"max_completion_tokens"`
	Stream              bool `json:"stream"`
}

func handleChat(w http.ResponseWriter, r *http.Request, p *Provider) {
	var in chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&in); err != nil {
		writeError(w, &api.Error{Status: 400, Type: api.TypeInvalidRequest, Code: api.CodeInvalidJSON,
			Message: fmt.Sprintf("mock: invalid JSON body: %v", err)})
		return
	}
	if in.Stream {
		writeError(w, &api.Error{Status: 400, Type: api.TypeInvalidRequest, Code: api.CodeUnsupportedValue,
			Param: "stream", Message: "mock: streaming is not implemented yet"})
		return
	}

	req := &provider.ChatRequest{Model: in.Model, MaxTokens: max(in.MaxTokens, in.MaxCompletionTokens)}
	for _, m := range in.Messages {
		req.Messages = append(req.Messages, provider.Message{
			Role:  provider.Role(m.Role),
			Parts: textParts(m.Content),
		})
	}

	resp, err := p.Chat(r.Context(), req)
	if err != nil {
		if provider.ClassOf(err) == provider.Cancelled {
			return // the client has gone; there is no one to answer
		}
		writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.NewChatCompletion("chatcmpl-mock-"+rand.Text(), time.Now(), resp))
}

// textParts reads message content leniently: a string, or the text of an
// array of parts. Anything else counts as no text.
func textParts(raw json.RawMessage) []string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		texts = append(texts, part.Text)
	}
	return texts
}

func writeProviderError(w http.ResponseWriter, err error) {
	perr, ok := errors.AsType[*provider.Error](err)
	if !ok {
		writeError(w, &api.Error{Status: 500, Type: api.TypeServer, Message: err.Error()})
		return
	}
	typ := api.TypeInvalidRequest
	if perr.Status >= 500 {
		typ = api.TypeServer
	}
	if perr.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(perr.RetryAfter.Seconds())))
	}
	writeError(w, &api.Error{Status: perr.Status, Type: typ, Message: perr.Message})
}

func writeError(w http.ResponseWriter, e *api.Error) {
	writeJSON(w, e.Status, e.Envelope())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
