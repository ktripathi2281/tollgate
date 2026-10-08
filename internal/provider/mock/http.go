package mock

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ktripathi2281/tollgate/internal/api"
	"github.com/ktripathi2281/tollgate/internal/provider"
)

// maxBodyBytes caps request bodies sent to the mock server.
const maxBodyBytes = 4 << 20

// Handler returns an OpenAI-compatible HTTP API backed by p:
// POST /v1/chat/completions, streaming or not, and GET /v1/models.
//
// Unlike the gateway, it parses requests leniently, as a real provider
// would: fields it doesn't model are ignored.
func Handler(p *Provider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		handleChat(w, r, p)
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		api.WriteJSON(w, http.StatusOK, api.NewModelList([]string{"mock-1"}, time.Now()))
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
	StreamOptions       struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func handleChat(w http.ResponseWriter, r *http.Request, p *Provider) {
	var in chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&in); err != nil {
		api.WriteError(w, &api.Error{Status: 400, Type: api.TypeInvalidRequest, Code: api.CodeInvalidJSON,
			Message: fmt.Sprintf("mock: invalid JSON body: %v", err)})
		return
	}
	req := &provider.ChatRequest{Model: in.Model, MaxTokens: max(in.MaxTokens, in.MaxCompletionTokens)}
	for _, m := range in.Messages {
		req.Messages = append(req.Messages, provider.Message{
			Role:  provider.Role(m.Role),
			Parts: textParts(m.Content),
		})
	}

	if in.Stream {
		streamChat(w, r, p, req, in.StreamOptions.IncludeUsage)
		return
	}

	resp, err := p.Chat(r.Context(), req)
	if err != nil {
		if provider.ClassOf(err) == provider.Cancelled {
			return // the client has gone; there is no one to answer
		}
		writeProviderError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, api.NewChatCompletion("chatcmpl-mock-"+rand.Text(), time.Now(), resp))
}

// streamChat answers with server-sent events in OpenAI's format: a role
// chunk, one chunk per word, a finish chunk, a usage chunk if the client
// asked for one, then [DONE]. A failure mid-stream is sent as an error
// object in place of a chunk, and the stream ends without [DONE].
func streamChat(w http.ResponseWriter, r *http.Request, p *Provider, req *provider.ChatRequest, includeUsage bool) {
	s, err := p.ChatStream(r.Context(), req)
	if err != nil {
		if provider.ClassOf(err) != provider.Cancelled {
			writeProviderError(w, err)
		}
		return
	}
	defer s.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	sse := api.NewSSEWriter(w)
	chunks := api.NewChunks("chatcmpl-mock-"+rand.Text(), time.Now(), req.Model)

	// A failed write means the client has gone, so every write that fails
	// ends the stream.
	if sse.WriteJSON(chunks.Role()) != nil {
		return
	}
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if provider.ClassOf(err) != provider.Cancelled {
				_ = sse.WriteJSON((&api.Error{Status: 500, Type: api.TypeServer, Message: err.Error()}).Envelope())
			}
			return
		}
		if c.Delta != "" && sse.WriteJSON(chunks.Content(c.Delta)) != nil {
			return
		}
		if c.FinishReason != "" && sse.WriteJSON(chunks.Finish(c.FinishReason)) != nil {
			return
		}
		if c.Usage != nil && includeUsage && sse.WriteJSON(chunks.Usage(*c.Usage)) != nil {
			return
		}
	}
	_ = sse.WriteDone()
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
		api.WriteError(w, &api.Error{Status: 500, Type: api.TypeServer, Message: err.Error()})
		return
	}
	typ := api.TypeInvalidRequest
	if perr.Status >= 500 {
		typ = api.TypeServer
	}
	api.WriteError(w, &api.Error{Status: perr.Status, Type: typ, Message: perr.Message, RetryAfter: perr.RetryAfter})
}
