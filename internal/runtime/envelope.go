package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Error codes this plugin puts on the wire. A plugin.quiesce answered with
// unknown_method is read as "quiesce unsupported" and downgraded to a debug
// log (internal/pluginhost/host.go:884); every other method's error envelope
// is a failure whatever code it carries, so an unimplemented lifecycle method
// answers with exactly that code.
const (
	codeUnknownMethod = "unknown_method"
	codePluginError   = "plugin_error"
	codePluginPanic   = "plugin_panic"
)

// okEnvelope wraps a result in a success envelope.
func okEnvelope(result any) ([]byte, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	return json.Marshal(Envelope{OK: true, Result: encoded})
}

// errorEnvelope encodes a failure envelope. It cannot fail: when marshalling
// itself fails it falls back to a literal, so an error path always has bytes
// to hand the host.
func errorEnvelope(code, message string) []byte {
	raw, err := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"internal","message":"failed to encode error"}}`)
	}
	return raw
}

// emptyResult is the `{}` every hook without a payload answers with.
var emptyResult = struct{}{}

// unwrapEnvelope decodes a host callback's {ok,result,error} envelope into
// out. A nil out discards the result.
func unwrapEnvelope(raw []byte, out any) error {
	if len(raw) == 0 {
		return errors.New("empty host response")
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode host envelope: %w", err)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("host error %s: %s", env.Error.Code, env.Error.Message)
		}
		return errors.New("host reported failure")
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}
