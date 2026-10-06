// groups.go — the NAMED agent group (CR-CHAT-013, specs/CHAT-ADDRESSING.md §1.4,
// specs/CHAT-SESSIONS.md §2.1).
//
// A named group is a curated, editable SET OF AGENTS addressed as `@team:x` —
// an address target with a roster, and nothing else: no participants, no
// transcript, no state. It is deliberately NOT a session (§2.1) and NOT a
// capability pool: a capability target resolves to ONE live holder at delivery
// time, a named group delivers to EVERY member it currently holds (§3.4
// rule 7, D8).
//
// Two properties are load-bearing and tested here:
//
//   - the roster is INSPECTABLE — who is in it, who created it, who last
//     edited it (who may edit it in the GRANT sense is CR-CHAT-003/022; this
//     package records the actors and keeps the roster readable);
//   - membership is read AT SEND TIME — a roster edit routes the NEXT send to
//     the CURRENT members, never a cached set (§1.4 consequence 1). The store
//     therefore exposes only point reads, and the fan-out resolves the roster
//     fresh on every send (see sendMessage).
//
// The store is an append-log in the same spirit as the session JSONL log
// (CHAT-STORAGE.md §5.1): one full group snapshot per line, keep-LAST per name
// on read, fsync before return. It is a file store regardless of the session
// backend so the group roster survives every CR_SESSION_BACKEND selection.
package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// groupNamesFile is the log file inside the group root: one line per
// group-version, keep-LAST per name.
const groupNamesFile = "groups.jsonl"

// groupIDRE is the group-name rule (specs/CHAT-ADDRESSING.md §1.1
// group_ident): the same charset as ident — an ident_start followed by
// ident_char*. This is the SAME rule the addressing parser applies to the
// `@team:<name>` token, so a name that is accepted here is always addressable
// as @team:<name> and vice versa.
var groupIDRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// ValidGroupName reports whether name is addressable as `@team:<name>` (the
// §1.1 group_ident rule).
func ValidGroupName(name string) bool { return groupIDRE.MatchString(name) }

// Group is ONE named, curated set of agents (§1.4).
type Group struct {
	// Name is the group's identity and its address: `@team:<name>`.
	Name string `json:"name"`
	// Namespace is the realm the group lives in (§1.2's one-realm rule,
	// carried for the same reason a session carries it). "" is the default
	// realm.
	Namespace string `json:"namespace,omitempty"`
	// Members are the CURRENT member agent ids, sorted, de-duplicated.
	Members []string `json:"members"`
	// CreatedAt / CreatedBy record who opened the roster.
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	// UpdatedAt / UpdatedBy record the LAST membership edit — the roster's
	// edit history beyond the last one lives in the log's earlier lines.
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// GroupStore is the roster store the send path reads AT SEND TIME (§1.4
// consequence 1) and the group API reads and writes. A nil store, or one that
// answers not-found, means a `group` audience target is a RECORDED skip, not
// a guessed delivery (FanoutRecipients).
type GroupStore interface {
	// Get returns the CURRENT roster of one group by name.
	Get(ctx context.Context, name string) (*Group, error)
	// Create appends the group's first version; an existing name is
	// ErrGroupExists.
	Create(ctx context.Context, g *Group) error
	// Update appends a new version of the group (keep-LAST per name).
	Update(ctx context.Context, g *Group) error
	// List returns every group's current version, sorted by name.
	List(ctx context.Context) ([]*Group, error)
}

// The group store's named errors. Callers branch on them with errors.Is.
var (
	// ErrGroupNotFound — no group by that name.
	ErrGroupNotFound = errors.New("group not found")
	// ErrGroupExists — the name is already taken.
	ErrGroupExists = errors.New("group already exists")
	// ErrInvalidGroup — a malformed group (empty or unaddressable name, an
	// empty or non-ident member id).
	ErrInvalidGroup = errors.New("invalid group")
)

// groupLine is one log line: a full group snapshot. It is its own type only so
// the `v` version field can lead the line, exactly as a session Record does.
type groupLine struct {
	V     int    `json:"v"`
	Group *Group `json:"group"`
}

// JSONLGroupStore is the file-backed GroupStore: one groups.jsonl under root,
// append-only, keep-LAST per name on read.
type JSONLGroupStore struct {
	root string
	mu   sync.Mutex // serializes read-modify-write (Create/Update read to validate)
}

var _ GroupStore = (*JSONLGroupStore)(nil)

// NewJSONLGroupStore opens (creating when absent) the group roster log rooted
// at dir. The file is <dir>/groups.jsonl.
func NewJSONLGroupStore(dir string) (*JSONLGroupStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: group log root is empty", ErrInvalidGroup)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("group store: create root: %w", err)
	}
	return &JSONLGroupStore{root: dir}, nil
}

func (s *JSONLGroupStore) path() string { return filepath.Join(s.root, groupNamesFile) }

// validate checks the WRITE shape of a group: an addressable name and a
// de-duplicated, non-empty member set of ident-shaped agent ids.
func validateGroup(g *Group) error {
	if g == nil {
		return fmt.Errorf("%w: nil group", ErrInvalidGroup)
	}
	if !ValidGroupName(g.Name) {
		return fmt.Errorf("%w: name %q is not addressable as @team:<name> (want [A-Za-z0-9_][A-Za-z0-9_.-]*)", ErrInvalidGroup, g.Name)
	}
	g.Members = dedupeSorted(g.Members)
	for _, m := range g.Members {
		if !ValidGroupName(m) {
			return fmt.Errorf("%w: member id %q is not an ident", ErrInvalidGroup, m)
		}
	}
	return nil
}

// Create appends the group's first version.
func (s *JSONLGroupStore) Create(_ context.Context, g *Group) error {
	if err := validateGroup(g); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.read(g.Name); err == nil {
		return fmt.Errorf("%w: %q", ErrGroupExists, g.Name)
	} else if !errors.Is(err, ErrGroupNotFound) {
		return err
	}
	g.CreatedAt = g.CreatedAt.UTC()
	g.UpdatedAt = g.CreatedAt
	return s.append(g)
}

// Update appends a new version of the group (keep-LAST per name). The new
// version carries its own UpdatedAt/UpdatedBy.
func (s *JSONLGroupStore) Update(_ context.Context, g *Group) error {
	if err := validateGroup(g); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.read(g.Name)
	if err != nil {
		return err
	}
	// Preserve the creation facts: an update is never a re-create (the same
	// rule §2.4 applies to a session's membership events).
	g.CreatedAt = cur.CreatedAt
	g.CreatedBy = cur.CreatedBy
	g.UpdatedAt = g.UpdatedAt.UTC()
	return s.append(g)
}

// Get returns the CURRENT roster of one group.
func (s *JSONLGroupStore) Get(_ context.Context, name string) (*Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(name)
}

// List returns every group's current version, sorted by name.
func (s *JSONLGroupStore) List(_ context.Context) ([]*Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAll()
	if err != nil {
		return nil, err
	}
	out := make([]*Group, 0, len(all))
	for _, g := range all {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// read returns the current version of one name (keep-LAST), or
// ErrGroupNotFound.
func (s *JSONLGroupStore) read(name string) (*Group, error) {
	all, err := s.readAll()
	if err != nil {
		return nil, err
	}
	g, ok := all[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrGroupNotFound, name)
	}
	return g, nil
}

// readAll reduces the log to the current version per name (keep-LAST per
// name), the same reduction Records applies per (session_id, seq).
func (s *JSONLGroupStore) readAll() (map[string]*Group, error) {
	f, err := os.Open(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]*Group{}, nil
		}
		return nil, fmt.Errorf("group store: open log: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecordLineBytes)
	last := map[string]int{} // name -> line index
	var lines []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("group store: read log: %w", err)
	}
	out := map[string]*Group{}
	for i, line := range lines {
		var gl groupLine
		if err := json.Unmarshal([]byte(line), &gl); err != nil || gl.Group == nil {
			// A corrupt line is a finding, not a silent skip — but the
			// roster read must stay usable, so the line is refused loudly
			// here rather than dropped.
			return nil, fmt.Errorf("group store: corrupt line %d: %v", i+1, err)
		}
		last[gl.Group.Name] = i
	}
	// Second pass applies keep-LAST per name.
	for name, i := range last {
		var gl groupLine
		if err := json.Unmarshal([]byte(lines[i]), &gl); err != nil || gl.Group == nil {
			return nil, fmt.Errorf("group store: corrupt line %d: %v", i+1, err)
		}
		out[name] = gl.Group
	}
	return out, nil
}

// append writes one full snapshot line and fsyncs before returning — the same
// durability rule the session JSONL log applies (§2.3).
func (s *JSONLGroupStore) append(g *Group) error {
	line, err := json.Marshal(groupLine{V: 1, Group: g})
	if err != nil {
		return fmt.Errorf("group store: marshal: %w", err)
	}
	line = append(line, '\n')
	f, err := os.OpenFile(s.path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("group store: open log: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("group store: append: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("group store: fsync: %w", err)
	}
	return nil
}
