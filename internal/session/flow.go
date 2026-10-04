package session

import (
	"context"
	"fmt"
)

// ---------------------------------------------------------------------------
// CR-CHAT-015 — the request→thread flow (§1.4 acceptance 3).
//
// A request IS a thread: sending a request does not append to a flat log, it
// opens a thread whose root is that request, and the replies live inside it.
// The flow below is the named surface for that, and it writes through the SAME
// §5.1 records every other message uses — there is no second delivery path and
// no second identity (D1, §4.1).
// ---------------------------------------------------------------------------

// ThreadWriter is the pair the request→thread flow needs: append one record
// (Log) and read the transcript back to resolve a reply's thread (View). Both
// JSONLStore and PostgresStore satisfy it.
type ThreadWriter interface {
	Log
	View
}

// OpenRequest posts a REQUEST message as the root of a NEW thread and returns
// the thread id it created — the request's own message id (§4.3, "a thread
// root's thread_id is its own message id"). The request and every reply to it
// are therefore one addressable conversation, which is the property that makes
// two features separable at the top level instead of one interleaved stream.
//
// A request must be a root: a message that already names a parent_id, or names
// a thread other than its own id, is refused rather than quietly re-rooted.
func OpenRequest(ctx context.Context, w ThreadWriter, req *Message) (string, error) {
	if req == nil {
		return "", fmt.Errorf("%w: nil request", ErrInvalidRecord)
	}
	if req.ID == "" {
		return "", fmt.Errorf("%w: request without a message id", ErrInvalidRecord)
	}
	if req.ParentID != "" {
		return "", fmt.Errorf("%w: a request is a thread root and must carry no parent_id, got %q",
			ErrInvalidRecord, req.ParentID)
	}
	if req.ThreadID != "" && req.ThreadID != req.ID {
		return "", fmt.Errorf("%w: a request's thread_id %q must be its own message id %q (§4.3)",
			ErrInvalidRecord, req.ThreadID, req.ID)
	}
	req.ThreadID = req.ID
	if err := w.Append(ctx, req.Record()); err != nil {
		return "", err
	}
	return req.ID, nil
}

// Reply posts a message inside the thread its parent belongs to. The caller
// names only what it replies TO (parent_id); the flow resolves the thread from
// the transcript and refuses to move the record out of it (D11, §4.5).
//
//   - a parent_id that is not in the session is ErrMessageNotFound;
//   - a reply that names a thread other than its parent's is ErrThreadMismatch;
//   - an empty thread_id is FILLED with the parent's thread.
//
// Neither case is silently repaired: a reply cannot fabricate or cross a
// conversation. This is the read-then-write CHAT-THREADING §5.2 places in the
// API layer — the record builders themselves never read a transcript to write a
// record.
func Reply(ctx context.Context, w ThreadWriter, reply *Message) error {
	if reply == nil {
		return fmt.Errorf("%w: nil reply", ErrInvalidRecord)
	}
	if reply.ID == "" {
		return fmt.Errorf("%w: reply without a message id", ErrInvalidRecord)
	}
	if reply.SessionID == "" {
		return fmt.Errorf("%w: reply without a session_id", ErrInvalidRecord)
	}
	if reply.ParentID == "" {
		return fmt.Errorf("%w: a reply must name the parent_id it replies to (a thread root is posted with OpenRequest)",
			ErrInvalidRecord)
	}
	st, err := w.Load(ctx, reply.SessionID)
	if err != nil {
		return err
	}
	parent := st.Message(reply.ParentID)
	if parent == nil {
		return fmt.Errorf("%w: parent %q is not in session %q",
			ErrMessageNotFound, reply.ParentID, reply.SessionID)
	}
	if parent.ThreadID == "" {
		return fmt.Errorf("%w: parent %q states no thread, so a reply has no thread to land in",
			ErrMessageNotFound, reply.ParentID)
	}
	if reply.ThreadID != "" && reply.ThreadID != parent.ThreadID {
		return fmt.Errorf("%w: reply names thread %q but its parent %q is in thread %q (a reply never moves out of its thread, D11 §4.5)",
			ErrThreadMismatch, reply.ThreadID, reply.ParentID, parent.ThreadID)
	}
	reply.ThreadID = parent.ThreadID
	return w.Append(ctx, reply.Record())
}
