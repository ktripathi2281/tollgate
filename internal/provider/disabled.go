package provider

import "context"

// Disabled returns a provider that fails every call with an AuthOrConfig
// error, for a provider the gateway can't use, such as one whose API key
// isn't set. The router falls back past it and the failure is logged at
// error level on every request, so the gap can't go unnoticed, while
// aliases that don't use it keep working.
func Disabled(name, reason string) Provider {
	return disabled{name: name, reason: reason}
}

type disabled struct{ name, reason string }

func (d disabled) Name() string { return d.name }

func (d disabled) Chat(context.Context, *ChatRequest) (*ChatResponse, error) {
	return nil, d.err()
}

func (d disabled) ChatStream(context.Context, *ChatRequest) (Stream, error) {
	return nil, d.err()
}

func (d disabled) err() error {
	return &Error{Provider: d.name, Class: AuthOrConfig, Message: "provider disabled: " + d.reason}
}
