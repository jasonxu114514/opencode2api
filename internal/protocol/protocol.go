// Package protocol converts request bodies, response bodies and SSE streams
// between the OpenAI Chat Completions, OpenAI Responses and Anthropic Messages
// protocols through a shared bridge model.
//
// System One is recognized but never translated: Valid accepts it, Path maps
// it to its endpoint and WriteError gives it the OpenAI error envelope. It has
// no bridge encoding, so a cross-protocol conversion fails, and the gateway
// forwards its decision payloads verbatim.
package protocol

// Protocol names the wire protocol of a client-facing endpoint or of an
// upstream model.
type Protocol string

// The protocol constants name the wire protocol of a client endpoint or an
// upstream model, and each is both: the gateway serves a request in the
// client's protocol and speaks the route's protocol upstream, converting
// between the two when they differ. Chat is OpenAI Chat Completions, Responses
// is OpenAI Responses, Anthropic is Anthropic Messages, and SystemOne is
// described below.
const (
	Chat      Protocol = "chat"
	Responses Protocol = "responses"
	Anthropic Protocol = "anthropic"
	// SystemOne is OpenCode Zen's structured decision endpoint. A request pairs
	// a free-form state with typed questions and the reply carries typed
	// answers, so the payload shares no shape with the message protocols the
	// bridge converts and is forwarded as-is.
	SystemOne Protocol = "systemone"
)

// Valid reports whether p is one of the protocols defined above.
func Valid(p Protocol) bool {
	return p == Chat || p == Responses || p == Anthropic || p == SystemOne
}

// Path returns the endpoint path that serves a protocol, relative to an API
// base. The gateway's own routes and the upstream endpoints share these
// paths. An unknown protocol gets the Chat path.
func Path(protocol Protocol) string {
	switch protocol {
	case Responses:
		return "/v1/responses"
	case Anthropic:
		return "/v1/messages"
	case SystemOne:
		return "/v1/systemone"
	default:
		return "/v1/chat/completions"
	}
}
