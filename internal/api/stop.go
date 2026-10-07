package api

import (
	"encoding/json"
	"errors"
)

// maxStopSequences is OpenAI's limit on stop sequences.
const maxStopSequences = 4

// parseStop decodes the "stop" parameter. It is either one stop sequence as
// a JSON string, or up to maxStopSequences of them as an array of strings.
// An empty array means no stop sequences. Every sequence must be non-empty.
// The caller has already handled an absent or null "stop".
//
// The returned error describes the problem in a few words, such as "expected
// at most 4 stop sequences"; the caller adds the parameter name.
func parseStop(raw json.RawMessage) ([]string, error) {
	// EXERCISE: implement parseStop; stop_test.go has every case it must pass.
	// Hint: try json.Unmarshal into a string first and then into a []string; if both
	// fail, the type is wrong. Check the count and empty sequences after decoding.
	_ = raw
	return nil, errors.New("parseStop is not implemented yet")
}
