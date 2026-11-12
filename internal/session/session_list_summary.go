package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/crier-dev/crier/internal/namespace"
)

// SessionListReader is the bounded read surface for GET /sessions. Implementations
// return the session projection and membership rows without assembling State.
// Stores that do not implement it deliberately keep the State-loading fallback.
type SessionListReader interface {
	SessionSummaries(context.Context) ([]SessionSummary, error)
	SessionMemberships(context.Context) ([]SessionMembership, error)
}

// SessionSummary is the aggregate portion of a session list row.
type SessionSummary struct {
	ID               string
	Namespace        string
	Kind             Kind
	Title            string
	CreatedAt        time.Time
	CreatedBy        AuthorRef
	State            SessionState
	ClosedAt         *time.Time
	Visibility       string
	Harness          string
	HarnessSessionID string
	MessageCount     int
	ThreadCount      int
	LastMessageAt    *time.Time
	LastPayload      json.RawMessage
}

// SessionMembership is one projected membership row. ActiveAt is evaluated by
// the handler so the bounded path has the same time semantics as State.
type SessionMembership struct {
	SessionID  string
	MemberType MemberType
	MemberID   string
	AddedAt    time.Time
	RemovedAt  *time.Time
}

func (m SessionMembership) ActiveAt(now time.Time) bool {
	if m.AddedAt.After(now) {
		return false
	}
	return m.RemovedAt == nil || m.RemovedAt.After(now)
}

func (h *Handler) listSessionSummaries(ctx context.Context, reader SessionListReader, realm string, stateFilter SessionState, kindFilter Kind, minMessages, minParticipants int) (sessionsResponse, error) {
	summaries, err := reader.SessionSummaries(ctx)
	if err != nil {
		return sessionsResponse{}, err
	}
	members, err := reader.SessionMemberships(ctx)
	if err != nil {
		return sessionsResponse{}, err
	}
	bySession := make(map[string][]SessionMembership, len(summaries))
	for _, member := range members {
		bySession[member.SessionID] = append(bySession[member.SessionID], member)
	}

	out := sessionsResponse{Sessions: make([]sessionView, 0, len(summaries))}
	now := h.now()
	for _, summary := range summaries {
		if realm != "" && namespace.Canonical(summary.Namespace) != realm {
			continue
		}
		if stateFilter != "" && stateOrDefault(summary.State) != stateFilter {
			continue
		}
		if kindFilter != "" && kindOrDefault(summary.Kind) != kindFilter {
			continue
		}
		view := sessionView{
			ID: summary.ID, Namespace: namespace.Canonical(summary.Namespace),
			Kind: string(kindOrDefault(summary.Kind)), Title: summary.Title,
			CreatedAt: summary.CreatedAt, CreatedBy: summary.CreatedBy,
			State: string(stateOrDefault(summary.State)), ClosedAt: summary.ClosedAt,
			Visibility: summary.Visibility, MessageCount: summary.MessageCount,
			ThreadCount: summary.ThreadCount, LastMessageAt: summary.LastMessageAt,
			Preview: payloadPreview(summary.LastPayload), Harness: summary.Harness,
			HarnessSessionID: summary.HarnessSessionID,
		}
		for _, member := range bySession[summary.ID] {
			if !member.ActiveAt(now) {
				continue
			}
			view.ParticipantCount++
			if member.MemberType == MemberAgent {
				view.AudienceCount++
			}
		}
		if view.MessageCount < minMessages || view.ParticipantCount < minParticipants {
			continue
		}
		out.Sessions = append(out.Sessions, view)
	}
	sort.SliceStable(out.Sessions, func(i, j int) bool { return out.Sessions[i].ID < out.Sessions[j].ID })
	out.Count = len(out.Sessions)
	return out, nil
}

// SessionSummaries reads one aggregate result for the complete session list.
// It intentionally does not touch chat_deliveries or any per-session State.
func (s *SQLStore) SessionSummaries(ctx context.Context) ([]SessionSummary, error) {
	rows, err := s.query(ctx, `
		SELECT s.id, s.namespace, s.kind, s.title, s.created_by, s.created_at,
		       s.state, s.closed_at, s.visibility, s.harness, s.harness_session_id,
		       (SELECT COUNT(*) FROM chat_transcript m
			WHERE m.session_id = s.id),
		       (SELECT COUNT(*) FROM chat_threads th
			WHERE th.session_id = s.id),
		       (SELECT MAX(mlast.created_at) FROM chat_transcript mlast
			WHERE mlast.session_id = s.id),
		       (SELECT mlast.payload FROM chat_transcript mlast
			WHERE mlast.session_id = s.id ORDER BY mlast.seq DESC LIMIT 1)
		FROM chat_sessions s
		ORDER BY s.id`)
	if err != nil {
		return nil, fmt.Errorf("session %s store: list summaries: %w", s.dialect.Name(), err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionSummary
	for rows.Next() {
		var summary SessionSummary
		var createdBy, createdAt, state string
		var closedAt, lastMessageAt sql.NullString
		var payload sql.NullString
		if err := rows.Scan(&summary.ID, &summary.Namespace, &summary.Kind, &summary.Title,
			&createdBy, &createdAt, &state, &closedAt, &summary.Visibility,
			&summary.Harness, &summary.HarnessSessionID, &summary.MessageCount,
			&summary.ThreadCount, &lastMessageAt, &payload); err != nil {
			return nil, fmt.Errorf("scan session summary: %w", err)
		}
		summary.State = SessionState(state)
		var err error
		if summary.CreatedAt, err = decTime(createdAt); err != nil {
			return nil, err
		}
		if summary.ClosedAt, err = decTimePtr(closedAt); err != nil {
			return nil, err
		}
		if lastMessageAt.Valid {
			at, err := decTime(lastMessageAt.String)
			if err != nil {
				return nil, err
			}
			summary.LastMessageAt = &at
		}
		if createdBy != "" && createdBy != "null" {
			if err := json.Unmarshal([]byte(createdBy), &summary.CreatedBy); err != nil {
				return nil, fmt.Errorf("load session summary created_by: %w", err)
			}
		}
		if payload.Valid && payload.String != "" && payload.String != "null" {
			summary.LastPayload = json.RawMessage(payload.String)
		}
		out = append(out, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session %s store: list summaries: %w", s.dialect.Name(), err)
	}
	return out, nil
}

// SessionMemberships is the one bounded membership statement paired with the
// aggregate statement. Active membership is evaluated by the handler because
// the list request's clock is the authority shared with viewOf.
func (s *SQLStore) SessionMemberships(ctx context.Context) ([]SessionMembership, error) {
	rows, err := s.query(ctx, `
		SELECT session_id, member_type, member_id, added_at, removed_at
		FROM chat_session_members ORDER BY session_id, member_type, member_id`)
	if err != nil {
		return nil, fmt.Errorf("session %s store: list memberships: %w", s.dialect.Name(), err)
	}
	defer func() { _ = rows.Close() }()
	var out []SessionMembership
	for rows.Next() {
		var member SessionMembership
		var memberType, addedAt string
		var removedAt sql.NullString
		if err := rows.Scan(&member.SessionID, &memberType, &member.MemberID, &addedAt, &removedAt); err != nil {
			return nil, fmt.Errorf("scan session membership: %w", err)
		}
		member.MemberType = MemberType(memberType)
		var err error
		if member.AddedAt, err = decTime(addedAt); err != nil {
			return nil, err
		}
		if member.RemovedAt, err = decTimePtr(removedAt); err != nil {
			return nil, err
		}
		out = append(out, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session %s store: list memberships: %w", s.dialect.Name(), err)
	}
	return out, nil
}
