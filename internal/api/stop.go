package api

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// The parameter has two shapes, so try the string first and fall back
	// to the array. If neither decodes, the JSON type is wrong.
	var seqs []string
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		seqs = []string{one}
	} else if err := json.Unmarshal(raw, &seqs); err != nil {
		return nil, errors.New("expected a string or an array of strings")
	}

	if len(seqs) > maxStopSequences {
		return nil, fmt.Errorf("expected at most %d stop sequences, got %d", maxStopSequences, len(seqs))
	}
	for _, s := range seqs {
		// A JSON null inside the array also decodes to "", so this check
		// rejects it too.
		if s == "" {
			return nil, errors.New("stop sequences must not be empty")
		}
	}
	if len(seqs) == 0 {
		return nil, nil
	}
	return seqs, nil
}
