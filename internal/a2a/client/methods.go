package client

import "github.com/crier-dev/crier/internal/a2a"

// The A2A JSON-RPC binding's method names are the ONE part of the wire that
// crier's own spec and the pre-1.0 peer ecosystem spell differently, so the
// client names both and lets the caller pick rather than guessing:
//
//   - WireV1 is A2A v1.0.0 §9.1's PascalCase form (`SendMessage`, `GetTask`,
//     `CancelTask`), which is also, verbatim, what crier's own JSON-RPC binding
//     serves (internal/a2a.MethodSendMessage …). It is the DEFAULT, because the
//     peer crier most often talks to first is another crier.
//   - WireLegacy is the slash form (`message/send`, `tasks/get`,
//     `tasks/cancel`) that v0.3-era SDKs and many deployed agents still emit.
//     A peer from that generation rejects the PascalCase name with
//     -32601 MethodNotFound.
//
// Nothing else about a request differs between the two: the params object, the
// envelope and the response shape are the ones internal/a2a already defines.
const (
	// MethodMessageSend is the slash-form name of §9.4.1's send operation.
	MethodMessageSend = "message/send"
	// MethodTasksGet is the slash-form name of §9.4.3's GetTask.
	MethodTasksGet = "tasks/get"
	// MethodTasksCancel is the slash-form name of §9.4.5's CancelTask.
	MethodTasksCancel = "tasks/cancel"
)

// Operations is the method-name triple one peer generation is spoken with.
// Name is the dialect's own label, used in the client's record and in errors.
type Operations struct {
	SendMessage string
	GetTask     string
	CancelTask  string
	Name        string
}

// WireV1 is A2A v1.0.0 §9.1's method names — the ones crier's server serves.
var WireV1 = Operations{
	SendMessage: a2a.MethodSendMessage,
	GetTask:     a2a.MethodGetTask,
	CancelTask:  a2a.MethodCancelTask,
	Name:        "v1",
}

// WireLegacy is the pre-1.0 slash form of the same three operations.
var WireLegacy = Operations{
	SendMessage: MethodMessageSend,
	GetTask:     MethodTasksGet,
	CancelTask:  MethodTasksCancel,
	Name:        "legacy",
}

// validate refuses an Operations value that would leave one operation unnamed:
// a half-filled triple would fail at the peer with a -32601 that names nothing
// useful, and the cause would be this side.
func (o Operations) validate() error {
	switch {
	case o.SendMessage == "":
		return newError("config", NameConfigInvalid, "the method name for message/send is empty", nil)
	case o.GetTask == "":
		return newError("config", NameConfigInvalid, "the method name for tasks/get is empty", nil)
	case o.CancelTask == "":
		return newError("config", NameConfigInvalid, "the method name for tasks/cancel is empty", nil)
	}
	return nil
}
