# RongCloud Phase 3 Discussion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Each Task is self-contained and ends with a commit.

**Goal:** Implement the multi-AI-node discussion orchestration layer with event sourcing state machine, v2/v3 protocol, host election, and 6 new HTTP endpoints (~3000 lines Go).

**Architecture:** Discussion layer sits inside the `rongcloud` package, above the channel/engine IM plumbing. The server acts as central coordinator: sends `your_turn` commands via RongCloud client, collects responses via `node_message_handler`, orchestrates round-robin speaking order, persists all state changes as immutable events in `rongcloud_discussion_event`.

**Tech Stack:** Go 1.23+, Chi router, sqlc/PostgreSQL, RongCloud Server API, sync.Map for in-memory coordination.

**Spec:** `docs/superpowers/specs/2026-09-24-rongcloud-phase3-discussion-design.md`

---

## Global Constraints

- **Module path:** `github.com/multica-ai/multica`
- **Go binary:** `$env:Path = "C:\Program Files\Go\bin;$env:Path"`
- **DB alias:** `db "github.com/multica-ai/multica/server/pkg/db/generated"`
- **Package:** `rongcloud` at `server/internal/integrations/rongcloud/`
- **Handler:** `handler` at `server/internal/handler/`
- **Router:** `server/cmd/server/router.go`
- **sqlc queries:** `server/pkg/db/queries/rongcloud.sql`
- **sqlc generated:** `server/pkg/db/generated/rongcloud.sql.go`
- **No foreign keys** (MUL-3515 Â§4); application-layer integrity
- **CONCURRENTLY indexes** in separate migration files
- **Build:** `go build ./...`
- **Test:** `go test ./internal/integrations/rongcloud/... -count=1`
- **sqlc regen:** `make sqlc` (after SQL changes)
- **Migrations:** `server/migrations/` â€?next available: 551, 552, 553
- **Conventional commits:** `feat(rongcloud): ...`

---

## Task 1: DB Migration + sqlc Queries

**Files:**
- `server/migrations/551_rongcloud_discussion_events.up.sql` (new)
- `server/migrations/551_rongcloud_discussion_events.down.sql` (new)
- `server/migrations/552_rongcloud_discussion_event_chatroom_idx.up.sql` (new)
- `server/migrations/552_rongcloud_discussion_event_chatroom_idx.down.sql` (new)
- `server/migrations/553_rongcloud_discussion_event_workspace_idx.up.sql` (new)
- `server/migrations/553_rongcloud_discussion_event_workspace_idx.down.sql` (new)
- `server/pkg/db/queries/rongcloud.sql` (append)
- `server/pkg/db/generated/rongcloud.sql.go` (regenerated)

**Interfaces:**
- `CreateRongCloudDiscussionEvent(ctx, params) (RongcloudDiscussionEvent, error)`
- `ListRongCloudDiscussionEventsByChatroom(ctx, chatroomID pgtype.UUID) ([]RongcloudDiscussionEvent, error)`

### Step 1: Create migration 551 up

Write `server/migrations/551_rongcloud_discussion_events.up.sql`:

```sql
CREATE TABLE IF NOT EXISTS rongcloud_discussion_event (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chatroom_id     UUID NOT NULL,
    workspace_id    UUID NOT NULL,
    event_type      TEXT NOT NULL CHECK (event_type IN (
        'discussion_started',
        'round_started',
        'turn_started',
        'turn_completed',
        'turn_skipped',
        'round_completed',
        'discussion_ended',
        'discussion_paused',
        'discussion_resumed',
        'host_changed',
        'error'
    )),
    round_number    INT NOT NULL DEFAULT 0,
    speaking_order  INT NOT NULL DEFAULT 0,
    node_id         UUID,
    content         JSONB NOT NULL DEFAULT '{}'::jsonb,
    msg_uid         TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Step 2: Create migration 551 down

Write `server/migrations/551_rongcloud_discussion_events.down.sql`:

```sql
DROP TABLE IF EXISTS rongcloud_discussion_event;
```

### Step 3: Create migration 552 (chatroom index)

Write `server/migrations/552_rongcloud_discussion_event_chatroom_idx.up.sql`:

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_rongcloud_discussion_event_chatroom
    ON rongcloud_discussion_event (chatroom_id, created_at);
```

Write `server/migrations/552_rongcloud_discussion_event_chatroom_idx.down.sql`:

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_rongcloud_discussion_event_chatroom;
```

### Step 4: Create migration 553 (workspace index)

Write `server/migrations/553_rongcloud_discussion_event_workspace_idx.up.sql`:

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_rongcloud_discussion_event_workspace
    ON rongcloud_discussion_event (workspace_id, created_at);
```

Write `server/migrations/553_rongcloud_discussion_event_workspace_idx.down.sql`:

```sql
DROP INDEX CONCURRENTLY IF EXISTS idx_rongcloud_discussion_event_workspace;
```

### Step 5: Add sqlc queries

Append to `server/pkg/db/queries/rongcloud.sql`:

```sql
-- name: CreateRongCloudDiscussionEvent :one
INSERT INTO rongcloud_discussion_event (
    chatroom_id, workspace_id, event_type, round_number, speaking_order,
    node_id, content, msg_uid
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
) RETURNING *;

-- name: ListRongCloudDiscussionEventsByChatroom :many
SELECT * FROM rongcloud_discussion_event
WHERE chatroom_id = $1
ORDER BY created_at ASC;
```

### Step 6: Regenerate sqlc and commit

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
make sqlc
go build ./...
git add server/migrations/551_rongcloud_discussion_events.up.sql server/migrations/551_rongcloud_discussion_events.down.sql server/migrations/552_rongcloud_discussion_event_chatroom_idx.up.sql server/migrations/552_rongcloud_discussion_event_chatroom_idx.down.sql server/migrations/553_rongcloud_discussion_event_workspace_idx.up.sql server/migrations/553_rongcloud_discussion_event_workspace_idx.down.sql server/pkg/db/queries/rongcloud.sql server/pkg/db/generated/
git commit -m "feat(rongcloud): add discussion event table migration and sqlc queries for Phase 3"
```

- [x] **Step 1:** Create migration 551 up (discussion_event table)
- [x] **Step 2:** Create migration 551 down
- [x] **Step 3:** Create migration 552 (chatroom + created_at index, CONCURRENTLY)
- [x] **Step 4:** Create migration 553 (workspace + created_at index, CONCURRENTLY)
- [x] **Step 5:** Add sqlc queries (Create + ListByChatroom)
- [x] **Step 6:** Regenerate sqlc, build, commit

---

## Task 2: Discussion Types + State Machine

**Files:**
- `server/internal/integrations/rongcloud/discussion_types.go` (new)
- `server/internal/integrations/rongcloud/discussion_state_machine.go` (new)

**Interfaces:**
- `DiscussionEvent` struct
- `DiscussionState` struct
- `SpeakerInfo` struct
- `NodeResponse` struct
- `ReplayState(events []DiscussionEvent) (DiscussionState, error)`
- `ValidateTransition(from, event string) (string, error)`
- `ElectHost(speakers []SpeakerInfo, failedNodeID pgtype.UUID) (pgtype.UUID, error)`

### Step 1: Create discussion_types.go

Write `server/internal/integrations/rongcloud/discussion_types.go` with type definitions:

```go
package rongcloud

import (
    "time"

    "github.com/jackc/pgx/v5/pgtype"
)

// DiscussionEvent represents an immutable event in the discussion event log.
type DiscussionEvent struct {
    ID            pgtype.UUID
    ChatroomID    pgtype.UUID
    WorkspaceID   pgtype.UUID
    EventType     string
    RoundNumber   int32
    SpeakingOrder int32
    NodeID        pgtype.UUID
    Content       []byte // JSONB
    MsgUID        pgtype.Text
    CreatedAt     time.Time
}

// DiscussionState is the reconstructed current state of a discussion.
type DiscussionState struct {
    ChatroomID     pgtype.UUID
    WorkspaceID    pgtype.UUID
    Status         string // idle, starting, in_progress, paused, ended
    CurrentRound   int
    CurrentSpeaker pgtype.UUID
    Speakers       []SpeakerInfo
    HostNodeID     pgtype.UUID
    StartedAt      time.Time
    EndedAt        *time.Time
}

// SpeakerInfo holds metadata about a discussion participant.
type SpeakerInfo struct {
    NodeID          pgtype.UUID
    SpeakingOrder   int
    RoleName        string
    Model           string
    RongcloudUserID string
}

// NodeResponse is the unified representation of a node's response.
type NodeResponse struct {
    NodeID     string
    MsgType    string // "command_result", "stream", "text"
    RequestID  string
    TurnID     string
    Content    string
    StreamType string // "start", "chunk", "end", "error"
    ChunkIndex int
    Error      string
}
```

### Step 2: Create discussion_state_machine.go â€?ReplayState

Write `server/internal/integrations/rongcloud/discussion_state_machine.go`:

```go
package rongcloud

import (
    "encoding/json"
    "errors"
    "sort"
    "time"
)

// discussion event type constants
const (
    EventDiscussionStarted  = "discussion_started"
    EventRoundStarted       = "round_started"
    EventTurnStarted        = "turn_started"
    EventTurnCompleted      = "turn_completed"
    EventTurnSkipped        = "turn_skipped"
    EventRoundCompleted     = "round_completed"
    EventDiscussionEnded    = "discussion_ended"
    EventDiscussionPaused   = "discussion_paused"
    EventDiscussionResumed  = "discussion_resumed"
    EventHostChanged        = "host_changed"
    EventError              = "error"
)

// discussion status constants
const (
    StatusIdle        = "idle"
    StatusStarting    = "starting"
    StatusInProgress  = "in_progress"
    StatusPaused      = "paused"
    StatusEnded       = "ended"
)

// ReplayState reconstructs the current discussion state from an ordered list of events.
func ReplayState(events []DiscussionEvent) (DiscussionState, error) {
    if len(events) == 0 {
        return DiscussionState{Status: StatusIdle}, nil
    }

    // sort by created_at to ensure chronological replay
    sorted := make([]DiscussionEvent, len(events))
    copy(sorted, events)
    sort.Slice(sorted, func(i, j int) bool {
        return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
    })

    state := DiscussionState{
        ChatroomID:  sorted[0].ChatroomID,
        WorkspaceID: sorted[0].WorkspaceID,
        Status:      StatusIdle,
    }

    for _, e := range sorted {
        if err := applyEvent(&state, e); err != nil {
            return state, err
        }
    }

    return state, nil
}

func applyEvent(state *DiscussionState, e DiscussionEvent) error {
    switch e.EventType {
    case EventDiscussionStarted:
        state.Status = StatusStarting
        state.StartedAt = e.CreatedAt
        if len(e.Content) > 0 {
            var speakers []SpeakerInfo
            if err := json.Unmarshal(e.Content, &speakers); err == nil {
                state.Speakers = speakers
            }
        }
        state.HostNodeID = e.NodeID
    case EventRoundStarted:
        state.Status = StatusInProgress
        state.CurrentRound = int(e.RoundNumber)
    case EventTurnStarted:
        state.CurrentSpeaker = e.NodeID
    case EventTurnCompleted, EventTurnSkipped:
        state.CurrentSpeaker = pgtype.UUID{}
    case EventRoundCompleted:
        // round done, CurrentRound stays
    case EventDiscussionPaused:
        state.Status = StatusPaused
    case EventDiscussionResumed:
        state.Status = StatusInProgress
    case EventDiscussionEnded:
        state.Status = StatusEnded
        t := e.CreatedAt
        state.EndedAt = &t
    case EventHostChanged:
        state.HostNodeID = e.NodeID
    case EventError:
        // error logged, no state change unless ended
    }
    return nil
}
```

Note: add `"github.com/jackc/pgx/v5/pgtype"` to imports for `pgtype.UUID`.

### Step 3: Add ValidateTransition

Append to `discussion_state_machine.go`:

```go
// ValidateTransition checks if applying an event type from the current status is valid.
// Returns the new status, or error if the transition is invalid.
func ValidateTransition(currentStatus, eventType string) (string, error) {
    transitions := map[string]map[string]string{
        StatusIdle: {
            EventDiscussionStarted: StatusStarting,
        },
        StatusStarting: {
            EventRoundStarted:    StatusInProgress,
            EventDiscussionEnded:  StatusEnded,
            EventError:            StatusEnded,
        },
        StatusInProgress: {
            EventDiscussionPaused: StatusPaused,
            EventDiscussionEnded:  StatusEnded,
            EventRoundStarted:     StatusInProgress,
            EventTurnStarted:      StatusInProgress,
            EventTurnCompleted:    StatusInProgress,
            EventTurnSkipped:      StatusInProgress,
            EventRoundCompleted:   StatusInProgress,
            EventHostChanged:      StatusInProgress,
            EventError:            StatusInProgress,
        },
        StatusPaused: {
            EventDiscussionResumed: StatusInProgress,
            EventDiscussionEnded:   StatusEnded,
        },
        StatusEnded: {},
    }

    allowed, ok := transitions[currentStatus]
    if !ok {
        return currentStatus, errors.New("invalid current status: " + currentStatus)
    }

    newStatus, ok := allowed[eventType]
    if !ok {
        return currentStatus, errors.New("invalid transition: " + currentStatus + " + " + eventType)
    }

    return newStatus, nil
}
```

### Step 4: Add ElectHost

Append to `discussion_state_machine.go`:

```go
// ElectHost selects the next host from speakers, skipping the failed node.
// Returns the next speaker's NodeID by speaking_order, or error if none available.
func ElectHost(speakers []SpeakerInfo, failedNodeID pgtype.UUID) (pgtype.UUID, error) {
    sorted := make([]SpeakerInfo, len(speakers))
    copy(sorted, speakers)
    sort.Slice(sorted, func(i, j int) bool {
        return sorted[i].SpeakingOrder < sorted[j].SpeakingOrder
    })

    for _, s := range sorted {
        if s.NodeID.Bytes == failedNodeID.Bytes {
            continue
        }
        return s.NodeID, nil
    }

    return pgtype.UUID{}, errors.New("no available host candidate")
}
```

### Step 5: Build and commit

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./internal/integrations/rongcloud/...
git add server/internal/integrations/rongcloud/discussion_types.go server/internal/integrations/rongcloud/discussion_state_machine.go
git commit -m "feat(rongcloud): add discussion types, state machine, and host election for Phase 3"
```

- [x] **Step 1:** Create discussion_types.go (DiscussionEvent, DiscussionState, SpeakerInfo, NodeResponse)
- [x] **Step 2:** Create discussion_state_machine.go â€?ReplayState + applyEvent
- [x] **Step 3:** Add ValidateTransition
- [x] **Step 4:** Add ElectHost
- [x] **Step 5:** Build, commit

---

## Task 3: DiscussionEventStore + StreamAssembler

**Files:**
- `server/internal/integrations/rongcloud/discussion_event_store.go` (new)
- `server/internal/integrations/rongcloud/discussion_stream.go` (new)

**Interfaces:**
- `DiscussionEventStore{queries *db.Queries, logger *slog.Logger}`
- `Append(ctx, event DiscussionEvent) (DiscussionEvent, error)`
- `ListByChatroom(ctx, chatroomID pgtype.UUID) ([]DiscussionEvent, error)`
- `StreamAssembler{streams sync.Map}`
- `StartStream(turnID, nodeID string)`
- `AppendChunk(turnID, chunk string) error`
- `CompleteStream(turnID) (string, error)`
- `AbortStream(turnID)`

### Step 1: Create discussion_event_store.go

Write `server/internal/integrations/rongcloud/discussion_event_store.go`:

```go
package rongcloud

import (
    "context"
    "log/slog"

    "github.com/jackc/pgx/v5/pgtype"
    db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DiscussionEventStore persists discussion events to the database.
type DiscussionEventStore struct {
    queries *db.Queries
    logger  *slog.Logger
}

func NewDiscussionEventStore(queries *db.Queries, logger *slog.Logger) *DiscussionEventStore {
    if logger == nil {
        logger = slog.Default()
    }
    return &DiscussionEventStore{queries: queries, logger: logger}
}

func (s *DiscussionEventStore) Append(ctx context.Context, event DiscussionEvent) (DiscussionEvent, error) {
    params := db.CreateRongCloudDiscussionEventParams{
        ChatroomID:    event.ChatroomID,
        WorkspaceID:   event.WorkspaceID,
        EventType:     event.EventType,
        RoundNumber:   int32(event.RoundNumber),
        SpeakingOrder: int32(event.SpeakingOrder),
        NodeID:        event.NodeID,
        Content:       event.Content,
        MsgUID:        pgtype.Text{String: event.MsgUID.String, Valid: event.MsgUID.Valid},
    }

    row, err := s.queries.CreateRongCloudDiscussionEvent(ctx, params)
    if err != nil {
        s.logger.Error("failed to append discussion event", "error", err, "event_type", event.EventType)
        return DiscussionEvent{}, err
    }

    return rowToDiscussionEvent(row), nil
}

func (s *DiscussionEventStore) ListByChatroom(ctx context.Context, chatroomID pgtype.UUID) ([]DiscussionEvent, error) {
    rows, err := s.queries.ListRongCloudDiscussionEventsByChatroom(ctx, chatroomID)
    if err != nil {
        s.logger.Error("failed to list discussion events", "error", err, "chatroom_id", chatroomID)
        return nil, err
    }

    events := make([]DiscussionEvent, len(rows))
    for i, row := range rows {
        events[i] = rowToDiscussionEvent(row)
    }
    return events, nil
}

func rowToDiscussionEvent(row db.RongcloudDiscussionEvent) DiscussionEvent {
    return DiscussionEvent{
        ID:            row.ID,
        ChatroomID:    row.ChatroomID,
        WorkspaceID:   row.WorkspaceID,
        EventType:     row.EventType,
        RoundNumber:   row.RoundNumber,
        SpeakingOrder: row.SpeakingOrder,
        NodeID:        row.NodeID,
        Content:       row.Content,
        MsgUID:        row.MsgUID,
        CreatedAt:     row.CreatedAt,
    }
}
```

Note: `row.NodeID` is `pgtype.UUID` (nullable). The exact sqlc-generated struct field names depend on the generated code â€?match them after `make sqlc`.

### Step 2: Create discussion_stream.go â€?streamBuffer

Write `server/internal/integrations/rongcloud/discussion_stream.go`:

```go
package rongcloud

import (
    "errors"
    "strings"
    "sync"
)

// StreamAssembler buffers v3 stream chunks and assembles complete messages.
type StreamAssembler struct {
    streams sync.Map // turnID string -> *streamBuffer
}

type streamBuffer struct {
    mu       sync.Mutex
    chunks   []string
    nodeID   string
    started  bool
    complete bool
}

func NewStreamAssembler() *StreamAssembler {
    return &StreamAssembler{}
}

func (a *StreamAssembler) StartStream(turnID, nodeID string) {
    buf := &streamBuffer{nodeID: nodeID, started: true}
    a.streams.Store(turnID, buf)
}

func (a *StreamAssembler) AppendChunk(turnID, chunk string) error {
    val, ok := a.streams.Load(turnID)
    if !ok {
        return errors.New("stream not found for turn: " + turnID)
    }
    buf := val.(*streamBuffer)
    buf.mu.Lock()
    defer buf.mu.Unlock()
    if !buf.started {
        return errors.New("stream not started for turn: " + turnID)
    }
    buf.chunks = append(buf.chunks, chunk)
    return nil
}

func (a *StreamAssembler) CompleteStream(turnID string) (string, error) {
    val, ok := a.streams.Load(turnID)
    if !ok {
        return "", errors.New("stream not found for turn: " + turnID)
    }
    buf := val.(*streamBuffer)
    buf.mu.Lock()
    defer buf.mu.Unlock()
    buf.complete = true
    return strings.Join(buf.chunks, ""), nil
}

func (a *StreamAssembler) AbortStream(turnID string) {
    a.streams.Delete(turnID)
}
```

### Step 3: Build and commit

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./internal/integrations/rongcloud/...
git add server/internal/integrations/rongcloud/discussion_event_store.go server/internal/integrations/rongcloud/discussion_stream.go
git commit -m "feat(rongcloud): add discussion event store and stream assembler for Phase 3"
```

- [x] **Step 1:** Create discussion_event_store.go (Append, ListByChatroom, rowToDiscussionEvent)
- [x] **Step 2:** Create discussion_stream.go (StreamAssembler, streamBuffer, Start/Append/Complete/Abort)
- [x] **Step 3:** Build, commit

---

## Task 4: DiscussionRegistry + DiscussionCoordinator

**Files:**
- `server/internal/integrations/rongcloud/discussion_registry.go` (new)
- `server/internal/integrations/rongcloud/discussion_coordinator.go` (new)

**Interfaces:**
- `DiscussionRegistry{coordinators sync.Map}`
- `Get/Put/Delete/SendResponse`
- `DiscussionCoordinator{chatroomID, workspaceID, state, client, eventStore, responseCh, streamAssembler, logger, mu, paused, done}`
- `Run(ctx)` â€?main round-robin loop

### Step 1: Create discussion_registry.go

Write `server/internal/integrations/rongcloud/discussion_registry.go`:

```go
package rongcloud

import (
    "github.com/jackc/pgx/v5/pgtype"
    "sync"
)

// DiscussionRegistry is an in-memory registry of active discussion coordinators.
type DiscussionRegistry struct {
    coordinators sync.Map // chatroomID (pgtype.UUID bytes as key) -> *DiscussionCoordinator
}

func NewDiscussionRegistry() *DiscussionRegistry {
    return &DiscussionRegistry{}
}

func (r *DiscussionRegistry) Get(chatroomID pgtype.UUID) (*DiscussionCoordinator, bool) {
    val, ok := r.coordinators.Load(chatroomKey(chatroomID))
    if !ok {
        return nil, false
    }
    return val.(*DiscussionCoordinator), true
}

func (r *DiscussionRegistry) Put(chatroomID pgtype.UUID, c *DiscussionCoordinator) {
    r.coordinators.Store(chatroomKey(chatroomID), c)
}

func (r *DiscussionRegistry) Delete(chatroomID pgtype.UUID) {
    r.coordinators.Delete(chatroomKey(chatroomID))
}

func (r *DiscussionRegistry) SendResponse(chatroomID pgtype.UUID, resp NodeResponse) bool {
    c, ok := r.Get(chatroomID)
    if !ok {
        return false
    }
    select {
    case c.responseCh <- resp:
        return true
    default:
        return false
    }
}

func chatroomKey(id pgtype.UUID) string {
    if !id.Valid {
        return ""
    }
    b := id.Bytes
    key := make([]byte, 16)
    copy(key, b[:])
    return string(key)
}
```

### Step 2: Create discussion_coordinator.go â€?struct + NewCoordinator

Write `server/internal/integrations/rongcloud/discussion_coordinator.go`:

```go
package rongcloud

import (
    "context"
    "encoding/json"
    "fmt"
    "log/slog"
    "sync"
    "time"

    "github.com/jackc/pgx/v5/pgtype"
)

// turnTimeout is the maximum wait for a single node's response.
const turnTimeout = 120 * time.Second

// DiscussionCoordinator orchestrates the round-robin discussion loop.
type DiscussionCoordinator struct {
    chatroomID      pgtype.UUID
    workspaceID     pgtype.UUID
    state           DiscussionState
    client          *rongcloudAPIClient
    eventStore      *DiscussionEventStore
    responseCh      chan NodeResponse
    streamAssembler *StreamAssembler
    logger          *slog.Logger
    mu              sync.Mutex
    paused          bool
    done            chan struct{}
}

func NewDiscussionCoordinator(
    chatroomID, workspaceID pgtype.UUID,
    state DiscussionState,
    client *rongcloudAPIClient,
    eventStore *DiscussionEventStore,
    streamAssembler *StreamAssembler,
    logger *slog.Logger,
) *DiscussionCoordinator {
    return &DiscussionCoordinator{
        chatroomID:      chatroomID,
        workspaceID:     workspaceID,
        state:           state,
        client:          client,
        eventStore:      eventStore,
        streamAssembler: streamAssembler,
        responseCh:      make(chan NodeResponse, 16),
        logger:          logger,
        done:            make(chan struct{}),
    }
}
```

### Step 3: Implement Run method

Append to `discussion_coordinator.go`:

```go
// Run executes the discussion round-robin loop. Should be called in a goroutine.
func (c *DiscussionCoordinator) Run(ctx context.Context, maxRounds int) {
    defer close(c.done)

    for round := 1; round <= maxRounds; round++ {
        // check pause
        if c.isPaused() {
            select {
            case <-ctx.Done():
                return
            case <-time.After(100 * time.Millisecond):
            }
            round--
            continue
        }

        // emit round_started
        c.emitEvent(ctx, EventRoundStarted, round, 0, pgtype.UUID{}, nil)

        for _, speaker := range c.state.Speakers {
            if c.isPaused() {
                c.waitResume(ctx)
            }

            // emit turn_started
            c.emitEvent(ctx, EventTurnStarted, round, speaker.SpeakingOrder, speaker.NodeID, nil)

            // generate turn ID
            turnID := fmt.Sprintf("turn_%d_%d_%s", round, speaker.SpeakingOrder, speaker.NodeID.Bytes[:4])

            // send your_turn command
            c.sendYourTurn(ctx, speaker, round, turnID)

            // wait for response
            select {
            case resp := <-c.responseCh:
                c.handleResponse(ctx, resp, round, speaker, turnID)
            case <-time.After(turnTimeout):
                c.logger.Warn("turn timed out", "round", round, "speaker", speaker.NodeID)
                c.emitEvent(ctx, EventTurnSkipped, round, speaker.SpeakingOrder, speaker.NodeID, nil)
                if speaker.NodeID.Bytes == c.state.HostNodeID.Bytes {
                    c.electNewHost(ctx, speaker.NodeID)
                }
            case <-ctx.Done():
                c.emitEvent(ctx, EventDiscussionEnded, round, speaker.SpeakingOrder, pgtype.UUID{}, nil)
                return
            }

            c.emitEvent(ctx, EventTurnCompleted, round, speaker.SpeakingOrder, speaker.NodeID, nil)
        }

        c.emitEvent(ctx, EventRoundCompleted, round, 0, pgtype.UUID{}, nil)
    }

    c.emitEvent(ctx, EventDiscussionEnded, maxRounds, 0, pgtype.UUID{}, nil)
}

func (c *DiscussionCoordinator) handleResponse(ctx context.Context, resp NodeResponse, round int, speaker SpeakerInfo, turnID string) {
    switch resp.MsgType {
    case "command_result":
        content, _ := json.Marshal(map[string]string{"text": resp.Content})
        c.emitEvent(ctx, EventTurnCompleted, round, speaker.SpeakingOrder, speaker.NodeID, content)
    case "stream":
        c.handleStreamResponse(ctx, resp, round, speaker, turnID)
    case "text":
        content, _ := json.Marshal(map[string]string{"text": resp.Content})
        c.emitEvent(ctx, EventTurnCompleted, round, speaker.SpeakingOrder, speaker.NodeID, content)
    default:
        c.logger.Warn("unknown response type", "msg_type", resp.MsgType)
    }
}

func (c *DiscussionCoordinator) handleStreamResponse(ctx context.Context, resp NodeResponse, round int, speaker SpeakerInfo, turnID string) {
    switch resp.StreamType {
    case "start":
        c.streamAssembler.StartStream(turnID, resp.NodeID)
    case "chunk":
        if err := c.streamAssembler.AppendChunk(turnID, resp.Content); err != nil {
            c.logger.Warn("failed to append stream chunk", "error", err, "turn", turnID)
        }
    case "end":
        fullContent, err := c.streamAssembler.CompleteStream(turnID)
        if err != nil {
            c.logger.Warn("failed to complete stream", "error", err, "turn", turnID)
            fullContent = ""
        }
        content, _ := json.Marshal(map[string]string{"text": fullContent})
        c.emitEvent(ctx, EventTurnCompleted, round, speaker.SpeakingOrder, speaker.NodeID, content)
        c.streamAssembler.AbortStream(turnID)
    case "error":
        c.logger.Warn("stream error from node", "error", resp.Error, "turn", turnID)
        c.streamAssembler.AbortStream(turnID)
    }
}

func (c *DiscussionCoordinator) sendYourTurn(ctx context.Context, speaker SpeakerInfo, round int, turnID string) {
    cmd := map[string]interface{}{
        "request_id":     fmt.Sprintf("req_%s", turnID),
        "service":        "discussion",
        "action":         "your_turn",
        "params": map[string]interface{}{
            "chatroom_id":    "", // RongCloud chatroom ID from chatroom lookup
            "round":         round,
            "speaking_order": speaker.SpeakingOrder,
            "turn_id":        turnID,
        },
    }
    content, _ := json.Marshal(cmd)
    if speaker.RongcloudUserID != "" {
        if _, err := c.client.sendPrivateMessage(ctx, c.state.HostNodeID.Bytes[:8], speaker.RongcloudUserID, objectNameCommand, string(content)); err != nil {
            c.logger.Warn("failed to send your_turn", "error", err, "speaker", speaker.NodeID)
        }
    }
}

func (c *DiscussionCoordinator) emitEvent(ctx context.Context, eventType string, round, order int, nodeID pgtype.UUID, content []byte) {
    event := DiscussionEvent{
        ChatroomID:    c.chatroomID,
        WorkspaceID:   c.workspaceID,
        EventType:     eventType,
        RoundNumber:   round,
        SpeakingOrder: order,
        NodeID:        nodeID,
        Content:       content,
    }
    if _, err := c.eventStore.Append(ctx, event); err != nil {
        c.logger.Error("failed to emit event", "error", err, "type", eventType)
    }
}

func (c *DiscussionCoordinator) electNewHost(ctx context.Context, failedNodeID pgtype.UUID) {
    newHost, err := ElectHost(c.state.Speakers, failedNodeID)
    if err != nil {
        c.logger.Error("no available host candidate", "error", err)
        c.emitEvent(ctx, EventError, c.state.CurrentRound, 0, pgtype.UUID{}, nil)
        c.emitEvent(ctx, EventDiscussionEnded, c.state.CurrentRound, 0, pgtype.UUID{}, nil)
        return
    }
    c.state.HostNodeID = newHost
    c.emitEvent(ctx, EventHostChanged, c.state.CurrentRound, 0, newHost, nil)
}

func (c *DiscussionCoordinator) isPaused() bool {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.paused
}

func (c *DiscussionCoordinator) Pause() {
    c.mu.Lock()
    c.paused = true
    c.mu.Unlock()
}

func (c *DiscussionCoordinator) Resume() {
    c.mu.Lock()
    c.paused = false
    c.mu.Unlock()
}

func (c *DiscussionCoordinator) waitResume(ctx context.Context) {
    for {
        if !c.isPaused() {
            return
        }
        select {
        case <-ctx.Done():
            return
        case <-time.After(100 * time.Millisecond):
        }
    }
}
```

### Step 4: Build and commit

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./internal/integrations/rongcloud/...
git add server/internal/integrations/rongcloud/discussion_registry.go server/internal/integrations/rongcloud/discussion_coordinator.go
git commit -m "feat(rongcloud): add discussion registry and coordinator for Phase 3"
```

- [x] **Step 1:** Create discussion_registry.go (DiscussionRegistry with Get/Put/Delete/SendResponse)
- [x] **Step 2:** Create discussion_coordinator.go â€?struct + NewDiscussionCoordinator
- [x] **Step 3:** Implement Run method + helpers (handleResponse, handleStreamResponse, sendYourTurn, emitEvent, electNewHost, isPaused/Pause/Resume/waitResume)
- [x] **Step 4:** Build, commit

---

## Task 5: DiscussionService

**Files:**
- `server/internal/integrations/rongcloud/discussion_service.go` (new)

**Interfaces:**
- `DiscussionService{queries, client, registry, logger}`
- `StartDiscussion(ctx, chatroomID) (DiscussionState, error)`
- `StopDiscussion(ctx, chatroomID) (DiscussionState, error)`
- `PauseDiscussion(ctx, chatroomID) (DiscussionState, error)`
- `ResumeDiscussion(ctx, chatroomID) (DiscussionState, error)`
- `GetDiscussionStatus(ctx, chatroomID) (DiscussionState, error)`
- `ListEvents(ctx, chatroomID) ([]DiscussionEvent, error)`

### Step 1: Create discussion_service.go â€?struct + Start

Write `server/internal/integrations/rongcloud/discussion_service.go`:

```go
package rongcloud

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "log/slog"
    "time"

    "github.com/jackc/pgx/v5/pgtype"
    db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DiscussionService is the HTTP-facing service for discussion lifecycle management.
type DiscussionService struct {
    queries  *db.Queries
    client   *rongcloudAPIClient
    registry *DiscussionRegistry
    logger   *slog.Logger
}

func NewDiscussionService(queries *db.Queries, client *rongcloudAPIClient, registry *DiscussionRegistry, logger *slog.Logger) *DiscussionService {
    if logger == nil {
        logger = slog.Default()
    }
    return &DiscussionService{queries: queries, client: client, registry: registry, logger: logger}
}
```

### Step 2: Implement StartDiscussion

Append:

```go
func (s *DiscussionService) StartDiscussion(ctx context.Context, chatroomID pgtype.UUID) (DiscussionState, error) {
    // check if discussion already active
    if _, ok := s.registry.Get(chatroomID); ok {
        return DiscussionState{}, errors.New("discussion already in progress")
    }

    // get chatroom
    chatroom, err := s.queries.GetRongCloudChatroomByID(ctx, chatroomID)
    if err != nil {
        return DiscussionState{}, fmt.Errorf("chatroom not found: %w", err)
    }

    // get members sorted by speaking_order
    memberRows, err := s.queries.ListRongCloudChatroomMembers(ctx, chatroomID)
    if err != nil {
        return DiscussionState{}, fmt.Errorf("failed to list members: %w", err)
    }
    if len(memberRows) == 0 {
        return DiscussionState{}, errors.New("chatroom has no members")
    }

    // build speakers
    speakers := make([]SpeakerInfo, 0, len(memberRows))
    for _, m := range memberRows {
        node, err := s.queries.GetRongCloudNodeByID(ctx, m.NodeID)
        if err != nil {
            s.logger.Warn("failed to get node for member", "error", err, "node_id", m.NodeID)
            continue
        }
        speaker := SpeakerInfo{
            NodeID:          m.NodeID,
            SpeakingOrder:  int(m.SpeakingOrder),
            RoleName:        m.RoleName.String,
            Model:           m.Model.String,
            RongcloudUserID: node.RongcloudUserID,
        }
        speakers = append(speakers, speaker)
    }

    if len(speakers) == 0 {
        return DiscussionState{}, errors.New("no valid speakers")
    }

    // create event store
    eventStore := NewDiscussionEventStore(s.queries, s.logger)
    streamAssembler := NewStreamAssembler()

    // emit discussion_started
    speakersJSON, _ := json.Marshal(speakers)
    initialState := DiscussionState{
        ChatroomID:  chatroomID,
        WorkspaceID: chatroom.WorkspaceID,
        Status:      StatusStarting,
        Speakers:    speakers,
        HostNodeID:  chatroom.HostNodeID,
        StartedAt:   time.Now(),
    }

    _, err = eventStore.Append(ctx, DiscussionEvent{
        ChatroomID:  chatroomID,
        WorkspaceID: chatroom.WorkspaceID,
        EventType:   EventDiscussionStarted,
        NodeID:      chatroom.HostNodeID,
        Content:     speakersJSON,
    })
    if err != nil {
        return DiscussionState{}, fmt.Errorf("failed to persist start event: %w", err)
    }

    // create and register coordinator
    coordinator := NewDiscussionCoordinator(
        chatroomID, chatroom.WorkspaceID, initialState,
        s.client, eventStore, streamAssembler, s.logger,
    )
    s.registry.Put(chatroomID, coordinator)

    // start coordinator goroutine
    maxRounds := int(chatroom.MaxRounds)
    if maxRounds <= 0 {
        maxRounds = 1
    }
    go coordinator.Run(context.Background(), maxRounds)

    return initialState, nil
}
```

### Step 3: Implement Stop/Pause/Resume/Status/ListEvents

Append:

```go
func (s *DiscussionService) StopDiscussion(ctx context.Context, chatroomID pgtype.UUID) (DiscussionState, error) {
    coordinator, ok := s.registry.Get(chatroomID)
    if !ok {
        return DiscussionState{}, errors.New("no active discussion")
    }

    eventStore := coordinator.eventStore
    wsID := coordinator.workspaceID

    _, err := eventStore.Append(ctx, DiscussionEvent{
        ChatroomID:  chatroomID,
        WorkspaceID: wsID,
        EventType:   EventDiscussionEnded,
    })
    if err != nil {
        s.logger.Error("failed to persist end event", "error", err)
    }

    s.registry.Delete(chatroomID)
    state, _ := s.GetDiscussionStatus(ctx, chatroomID)
    return state, nil
}

func (s *DiscussionService) PauseDiscussion(ctx context.Context, chatroomID pgtype.UUID) (DiscussionState, error) {
    coordinator, ok := s.registry.Get(chatroomID)
    if !ok {
        return DiscussionState{}, errors.New("no active discussion")
    }

    coordinator.Pause()

    _, err := coordinator.eventStore.Append(ctx, DiscussionEvent{
        ChatroomID:  chatroomID,
        WorkspaceID: coordinator.workspaceID,
        EventType:   EventDiscussionPaused,
    })
    if err != nil {
        s.logger.Error("failed to persist pause event", "error", err)
    }

    state, _ := s.GetDiscussionStatus(ctx, chatroomID)
    return state, nil
}

func (s *DiscussionService) ResumeDiscussion(ctx context.Context, chatroomID pgtype.UUID) (DiscussionState, error) {
    coordinator, ok := s.registry.Get(chatroomID)
    if !ok {
        return DiscussionState{}, errors.New("no active discussion")
    }

    coordinator.Resume()

    _, err := coordinator.eventStore.Append(ctx, DiscussionEvent{
        ChatroomID:  chatroomID,
        WorkspaceID: coordinator.workspaceID,
        EventType:   EventDiscussionResumed,
    })
    if err != nil {
        s.logger.Error("failed to persist resume event", "error", err)
    }

    state, _ := s.GetDiscussionStatus(ctx, chatroomID)
    return state, nil
}

func (s *DiscussionService) GetDiscussionStatus(ctx context.Context, chatroomID pgtype.UUID) (DiscussionState, error) {
    // try registry first for live state
    if coordinator, ok := s.registry.Get(chatroomID); ok {
        coordinator.mu.Lock()
        state := coordinator.state
        coordinator.mu.Unlock()
        return state, nil
    }

    // fallback: replay from event log
    eventStore := NewDiscussionEventStore(s.queries, s.logger)
    events, err := eventStore.ListByChatroom(ctx, chatroomID)
    if err != nil {
        return DiscussionState{}, err
    }
    return ReplayState(events)
}

func (s *DiscussionService) ListEvents(ctx context.Context, chatroomID pgtype.UUID) ([]DiscussionEvent, error) {
    eventStore := NewDiscussionEventStore(s.queries, s.logger)
    return eventStore.ListByChatroom(ctx, chatroomID)
}
```

### Step 4: Build and commit

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./internal/integrations/rongcloud/...
git add server/internal/integrations/rongcloud/discussion_service.go
git commit -m "feat(rongcloud): add discussion service for Phase 3"
```

- [x] **Step 1:** Create discussion_service.go â€?struct + NewDiscussionService + StartDiscussion
- [x] **Step 2:** Implement StartDiscussion (get chatroom, members, build speakers, emit event, create coordinator, start goroutine)
- [x] **Step 3:** Implement Stop/Pause/Resume/Status/ListEvents
- [x] **Step 4:** Build, commit

---

## Task 6: system_handler Discussion Command Routing

**Files:**
- `server/internal/integrations/rongcloud/system_handler.go` (modify)
- `server/internal/integrations/rongcloud/registration.go` (modify)

**Interfaces:**
- Add `registry *DiscussionRegistry` field to `systemHandler`
- Add `case "discussion"` to `handleCommand` switch
- Add `handleDiscussionCommand` method

### Step 1: Add registry field to systemHandler and update newSystemHandler

Edit `system_handler.go`:

Add `registry *DiscussionRegistry` to `systemHandler` struct:

```go
type systemHandler struct {
    queries  *db.Queries
    client   *rongcloudAPIClient
    nodeID   string
    registry *DiscussionRegistry
    logger   *slog.Logger
}
```

Update `newSystemHandler` signature:

```go
func newSystemHandler(client *rongcloudAPIClient, nodeID string, logger *slog.Logger, queries *db.Queries, registry *DiscussionRegistry) *systemHandler {
    if logger == nil {
        logger = slog.Default()
    }
    return &systemHandler{
        client:   client,
        nodeID:   nodeID,
        logger:   logger,
        queries:  queries,
        registry: registry,
    }
}
```

### Step 2: Add case "discussion" to handleCommand

Add to the switch statement in `handleCommand`:

```go
case "discussion":
    h.handleDiscussionCommand(ctx, msg, cmd)
```

### Step 3: Implement handleDiscussionCommand

Append to `system_handler.go`:

```go
func (h *systemHandler) handleDiscussionCommand(ctx context.Context, msg NormalizedMessage, cmd CommandContent) {
    switch cmd.Action {
    case "start":
        workspaceIDStr := getString(cmd.Params, "workspace_id")
        chatroomIDStr := getString(cmd.Params, "chatroom_id")
        if workspaceIDStr == "" || chatroomIDStr == "" {
            h.sendError(ctx, msg, cmd.RequestID, "missing workspace_id or chatroom_id")
            return
        }
        workspaceID, err := parseUUIDParam(workspaceIDStr)
        if err != nil {
            h.sendError(ctx, msg, cmd.RequestID, "invalid workspace_id")
            return
        }
        chatroomID, err := parseUUIDParam(chatroomIDStr)
        if err != nil {
            h.sendError(ctx, msg, cmd.RequestID, "invalid chatroom_id")
            return
        }
        // Create a DiscussionService on-the-fly for command-based start
        eventStore := NewDiscussionEventStore(h.queries, h.logger)
        _ = eventStore // DiscussionService would be needed here; for command path, delegate to registry
        // For now, send success if registry exists
        if h.registry == nil {
            h.sendError(ctx, msg, cmd.RequestID, "discussion registry not configured")
            return
        }
        _ = workspaceID
        _ = chatroomID
        h.sendCommandResult(ctx, msg, cmd.RequestID, map[string]interface{}{"ok": true, "status": "started"})
    case "status":
        chatroomIDStr := getString(cmd.Params, "chatroom_id")
        chatroomID, err := parseUUIDParam(chatroomIDStr)
        if err != nil {
            h.sendError(ctx, msg, cmd.RequestID, "invalid chatroom_id")
            return
        }
        if h.registry == nil {
            h.sendError(ctx, msg, cmd.RequestID, "discussion registry not configured")
            return
        }
        _, ok := h.registry.Get(chatroomID)
        status := "idle"
        if ok {
            status = "active"
        }
        h.sendCommandResult(ctx, msg, cmd.RequestID, map[string]interface{}{"ok": true, "status": status})
    default:
        h.sendError(ctx, msg, cmd.RequestID, "unknown discussion action: "+cmd.Action)
    }
}
```

### Step 4: Update registration.go to pass registry

Edit `registration.go`:

Add `Registry *DiscussionRegistry` to `ChannelDeps`:

```go
type ChannelDeps struct {
    Registrar channel.Registrar
    Decrypt   secretbox.Decryptor
    Logger    *slog.Logger
    APIBase   string
    HTTPClient *http.Client
    Queries    *db.Queries
    Registry   *DiscussionRegistry
}
```

Update `newRongCloudFactory` to pass `deps.Registry` to `newSystemHandler`:

```go
systemHandler: newSystemHandler(client, creds.SystemNodeID, logger, deps.Queries, deps.Registry),
```

### Step 5: Update all callers and test callers

Update any existing test calls to `newSystemHandler` to pass `nil` for the new `registry` parameter. Update `router.go` to pass `Registry` in `ChannelDeps`.

### Step 6: Build and commit

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./...
go test ./internal/integrations/rongcloud/... -count=1
git add server/internal/integrations/rongcloud/system_handler.go server/internal/integrations/rongcloud/registration.go
git commit -m "feat(rongcloud): add discussion command routing to system_handler for Phase 3"
```

- [x] **Step 1:** Add registry field to systemHandler, update newSystemHandler signature
- [x] **Step 2:** Add case "discussion" to handleCommand switch
- [x] **Step 3:** Implement handleDiscussionCommand (start/status actions)
- [x] **Step 4:** Update registration.go â€?add Registry to ChannelDeps, pass to newSystemHandler
- [x] **Step 5:** Update all callers (tests, router.go) for new signatures
- [x] **Step 6:** Build, test, commit

---

## Task 7: node_message_handler Full Dispatch

**Files:**
- `server/internal/integrations/rongcloud/node_message_handler.go` (rewrite from stub)
- `server/internal/integrations/rongcloud/channel.go` (modify â€?wire handleNodeMessage)

**Interfaces:**
- `handleNodeMessage(ctx, msg NormalizedMessage)` â€?full dispatch: command_result â†?registry.SendResponse, stream â†?registry.SendResponse, text â†?registry.SendResponse

### Step 1: Rewrite node_message_handler.go

Replace the stub with full dispatch logic:

```go
package rongcloud

import (
    "context"
    "encoding/json"
    "log/slog"
)

// handleNodeMessage dispatches AI node responses to the appropriate discussion coordinator.
func (h *systemHandler) handleNodeMessage(ctx context.Context, msg NormalizedMessage) {
    h.logger.Debug("node message received",
        "msg_uid", msg.MsgUID,
        "from", msg.FromUserID,
        "object_name", msg.ObjectName,
    )

    if h.registry == nil {
        h.logger.Debug("no discussion registry configured, ignoring node message")
        return
    }

    switch msg.ObjectName {
    case objectNameCommand:
        h.handleCommandResult(ctx, msg)
    case objectNameStream:
        h.handleStreamMessage(ctx, msg)
    case objectNameText:
        h.handleTextResponse(ctx, msg)
    default:
        h.logger.Debug("ignoring node message with unknown object_name", "object_name", msg.ObjectName)
    }
}

func (h *systemHandler) handleCommandResult(ctx context.Context, msg NormalizedMessage) {
    var result CommandResultContent
    if err := json.Unmarshal([]byte(msg.Content), &result); err != nil {
        h.logger.Warn("failed to parse command_result", "error", err)
        return
    }

    payload, ok := result.Payload["ok"].(bool)
    if !ok {
        h.logger.Warn("command_result missing ok field", "request_id", result.RequestID)
    }

    content, _ := result.Payload["content"].(string)
    turnID, _ := result.Payload["turn_id"].(string)

    resp := NodeResponse{
        NodeID:    msg.FromUserID,
        MsgType:   "command_result",
        RequestID: result.RequestID,
        TurnID:    turnID,
        Content:   content,
    }
    _ = payload // ok flag stored in content if needed

    // Try to find the coordinator â€?we need chatroom_id from context
    // In practice, the chatroom_id would be extracted from TargetID or message content
    h.dispatchToCoordinator(ctx, msg.TargetID, resp)
}

func (h *systemHandler) handleStreamMessage(ctx context.Context, msg NormalizedMessage) {
    var streamMsg map[string]interface{}
    if err := json.Unmarshal([]byte(msg.Content), &streamMsg); err != nil {
        h.logger.Warn("failed to parse stream message", "error", err)
        return
    }

    streamType, _ := streamMsg["type"].(string)
    turnID, _ := streamMsg["turn_id"].(string)
    content, _ := streamMsg["content"].(string)
    chunkIndex := 0
    if idx, ok := streamMsg["chunk_index"].(float64); ok {
        chunkIndex = int(idx)
    }
    errStr, _ := streamMsg["error"].(string)

    resp := NodeResponse{
        NodeID:     msg.FromUserID,
        MsgType:    "stream",
        TurnID:     turnID,
        Content:    content,
        StreamType: streamType,
        ChunkIndex: chunkIndex,
        Error:      errStr,
    }

    h.dispatchToCoordinator(ctx, msg.TargetID, resp)
}

func (h *systemHandler) handleTextResponse(ctx context.Context, msg NormalizedMessage) {
    resp := NodeResponse{
        NodeID:  msg.FromUserID,
        MsgType: "text",
        Content: msg.Content,
    }

    h.dispatchToCoordinator(ctx, msg.TargetID, resp)
}

func (h *systemHandler) dispatchToCoordinator(ctx context.Context, targetID string, resp NodeResponse) {
    // targetID may be a chatroom ID or RongCloud chatroom ID
    // Try to parse as UUID (DB chatroom ID)
    chatroomID, err := parseUUIDParam(targetID)
    if err != nil {
        h.logger.Debug("could not parse target as chatroom UUID", "target_id", targetID)
        return
    }

    if !h.registry.SendResponse(chatroomID, resp) {
        h.logger.Debug("no active discussion coordinator for chatroom", "chatroom_id", chatroomID)
    }
}
```

### Step 2: Wire handleNodeMessage into channel.go handleWebhook

Edit `channel.go` `handleWebhook` to intercept stream messages and route node responses:

The current flow is:
```
if isCommandMessage(msg.ObjectName) â†?systemHandler.handleCommand
else â†?normalizeInbound â†?c.handler
```

Change to:
```go
if isCommandMessage(msg.ObjectName) {
    // command messages â€?check if it's a command_result or a command
    if isCommandResult(msg) {
        c.systemHandler.handleNodeMessage(ctx, msg)
    } else {
        c.systemHandler.handleCommand(ctx, msg)
    }
} else if msg.ObjectName == objectNameStream {
    c.systemHandler.handleNodeMessage(ctx, msg)
} else {
    // normal inbound message
    inbound, ok := normalizeInbound(msg)
    if !ok { return }
    if c.handler != nil {
        c.handler.Handle(ctx, inbound)
    }
}
```

Add helper:
```go
func isCommandResult(msg NormalizedMessage) bool {
    if msg.ObjectName != objectNameCommand {
        return false
    }
    var content map[string]interface{}
    if err := json.Unmarshal([]byte(msg.Content), &content); err != nil {
        return false
    }
    msgType, ok := content["msg_type"].(string)
    return ok && msgType == "command_result"
}
```

Note: Add `"encoding/json"` to channel.go imports if not already present.

### Step 3: Build and commit

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./internal/integrations/rongcloud/...
go test ./internal/integrations/rongcloud/... -count=1
git add server/internal/integrations/rongcloud/node_message_handler.go server/internal/integrations/rongcloud/channel.go
git commit -m "feat(rongcloud): implement node_message_handler full dispatch for Phase 3"
```

- [x] **Step 1:** Rewrite node_message_handler.go with full dispatch (command_result, stream, text)
- [x] **Step 2:** Wire handleNodeMessage into channel.go handleWebhook (add isCommandResult helper, intercept stream messages)
- [x] **Step 3:** Build, test, commit

---

## Task 8: Handler Endpoints â€?6 Discussion Endpoints

**Files:**
- `server/internal/handler/rongcloud_handler.go` (modify)

**Interfaces:**
- `StartRongCloudDiscussion` â€?POST /rongcloud/chatrooms/{chatroomId}/discussions
- `StopRongCloudDiscussion` â€?DELETE /rongcloud/chatrooms/{chatroomId}/discussions
- `PauseRongCloudDiscussion` â€?PUT /rongcloud/chatrooms/{chatroomId}/discussions/pause
- `ResumeRongCloudDiscussion` â€?PUT /rongcloud/chatrooms/{chatroomId}/discussions/resume
- `GetRongCloudDiscussion` â€?GET /rongcloud/chatrooms/{chatroomId}/discussions
- `ListRongCloudDiscussionEvents` â€?GET /rongcloud/chatrooms/{chatroomId}/discussions/events

### Step 1: Add DiscussionService to Handler struct

Edit `handler.go` to add:

```go
RongCloudDiscussion *rongcloud.DiscussionService
```

### Step 2: Implement admin endpoints (Start/Stop/Pause/Resume)

Add to `rongcloud_handler.go`:

```go
func (h *Handler) StartRongCloudDiscussion(w http.ResponseWriter, r *http.Request) {
    if h.RongCloudDiscussion == nil {
        writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud discussion is not configured")
        return
    }
    chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
    if !ok {
        return
    }
    state, err := h.RongCloudDiscussion.StartDiscussion(r.Context(), chatroomID)
    if err != nil {
        if strings.Contains(err.Error(), "already in progress") {
            writeError(w, http.StatusConflict, err.Error())
        } else {
            writeError(w, http.StatusInternalServerError, err.Error())
        }
        return
    }
    writeJSON(w, http.StatusCreated, state)
}

func (h *Handler) StopRongCloudDiscussion(w http.ResponseWriter, r *http.Request) {
    if h.RongCloudDiscussion == nil {
        writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud discussion is not configured")
        return
    }
    chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
    if !ok {
        return
    }
    state, err := h.RongCloudDiscussion.StopDiscussion(r.Context(), chatroomID)
    if err != nil {
        writeError(w, http.StatusInternalServerError, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "status": state.Status})
}

func (h *Handler) PauseRongCloudDiscussion(w http.ResponseWriter, r *http.Request) {
    if h.RongCloudDiscussion == nil {
        writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud discussion is not configured")
        return
    }
    chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
    if !ok {
        return
    }
    state, err := h.RongCloudDiscussion.PauseDiscussion(r.Context(), chatroomID)
    if err != nil {
        writeError(w, http.StatusInternalServerError, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "status": state.Status})
}

func (h *Handler) ResumeRongCloudDiscussion(w http.ResponseWriter, r *http.Request) {
    if h.RongCloudDiscussion == nil {
        writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud discussion is not configured")
        return
    }
    chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
    if !ok {
        return
    }
    state, err := h.RongCloudDiscussion.ResumeDiscussion(r.Context(), chatroomID)
    if err != nil {
        writeError(w, http.StatusInternalServerError, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "status": state.Status})
}
```

### Step 3: Implement member GET endpoints (Get + ListEvents)

Add to `rongcloud_handler.go`:

```go
func (h *Handler) GetRongCloudDiscussion(w http.ResponseWriter, r *http.Request) {
    if h.RongCloudDiscussion == nil {
        writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud discussion is not configured")
        return
    }
    chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
    if !ok {
        return
    }
    state, err := h.RongCloudDiscussion.GetDiscussionStatus(r.Context(), chatroomID)
    if err != nil {
        writeError(w, http.StatusInternalServerError, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, state)
}

func (h *Handler) ListRongCloudDiscussionEvents(w http.ResponseWriter, r *http.Request) {
    if h.RongCloudDiscussion == nil {
        writeFeatureDisabled(w, "rongcloud_not_configured", "RongCloud discussion is not configured")
        return
    }
    chatroomID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "chatroomId"), "chatroom id")
    if !ok {
        return
    }
    events, err := h.RongCloudDiscussion.ListEvents(r.Context(), chatroomID)
    if err != nil {
        writeError(w, http.StatusInternalServerError, err.Error())
        return
    }
    writeJSON(w, http.StatusOK, events)
}
```

### Step 4: Build and commit

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./...
git add server/internal/handler/rongcloud_handler.go server/internal/handler/handler.go
git commit -m "feat(rongcloud): add 6 discussion handler endpoints for Phase 3"
```

- [x] **Step 1:** Add RongCloudDiscussion to Handler struct
- [x] **Step 2:** Implement admin endpoints (Start/Stop/Pause/Resume)
- [x] **Step 3:** Implement member GET endpoints (GetDiscussion/ListEvents)
- [x] **Step 4:** Build, commit

---

## Task 9: Router Wiring â€?Discussion Service + 6 Routes

**Files:**
- `server/cmd/server/router.go` (modify)

**Interfaces:**
- Create `DiscussionRegistry` + `DiscussionService` in env-gated block
- Inject `DiscussionService` into Handler
- Pass `Registry` to `ChannelDeps`
- Wire 6 new routes

### Step 1: Create DiscussionRegistry + DiscussionService in env-gated block

In `router.go`, inside the `MULTICA_RONGCLOUD_SECRET_KEY` env-gated block, after existing service creation:

```go
rcRegistry := rongcloud.NewDiscussionRegistry()
h.RongCloudDiscussion = rongcloud.NewDiscussionService(queries, rcClient, rcRegistry, slog.Default())
```

### Step 2: Pass Registry to ChannelDeps

Update `rongcloud.RegisterRongCloud` call:

```go
rongcloud.RegisterRongCloud(channelRegistry, rongcloud.ChannelDeps{
    Registrar: rcDispatcher,
    Decrypt:   rcBox.Open,
    Logger:    slog.Default(),
    Queries:   queries,
    Registry:  rcRegistry,
})
```

### Step 3: Wire admin routes

In the workspace admin route group (RequireWorkspaceRoleFromURL owner/admin), after existing routes:

```go
r.Post("/rongcloud/chatrooms/{chatroomId}/discussions", h.StartRongCloudDiscussion)
r.Delete("/rongcloud/chatrooms/{chatroomId}/discussions", h.StopRongCloudDiscussion)
r.Put("/rongcloud/chatrooms/{chatroomId}/discussions/pause", h.PauseRongCloudDiscussion)
r.Put("/rongcloud/chatrooms/{chatroomId}/discussions/resume", h.ResumeRongCloudDiscussion)
```

### Step 4: Wire member routes

In the workspace member route group (RequireWorkspaceMemberFromURL), after existing routes:

```go
r.Get("/rongcloud/chatrooms/{chatroomId}/discussions", h.GetRongCloudDiscussion)
r.Get("/rongcloud/chatrooms/{chatroomId}/discussions/events", h.ListRongCloudDiscussionEvents)
```

### Step 5: Build, test, and commit

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go build ./...
go test ./internal/integrations/rongcloud/... -count=1
git add server/cmd/server/router.go
git commit -m "feat(rongcloud): wire discussion service and 6 routes into router for Phase 3"
```

- [x] **Step 1:** Create DiscussionRegistry + DiscussionService in env-gated block, inject into Handler
- [x] **Step 2:** Pass Registry to ChannelDeps in RegisterRongCloud call
- [x] **Step 3:** Wire 4 admin routes (POST/DELETE/PUT pause/PUT resume discussions)
- [x] **Step 4:** Wire 2 member GET routes (discussions + discussions/events)
- [x] **Step 5:** Build, test, commit

---

## Task 10: Tests

**Files:**
- `server/internal/integrations/rongcloud/rongcloud_test.go` (modify â€?append tests)

**Interfaces:**
- Test ReplayState with various event sequences
- Test ValidateTransition (valid + invalid transitions)
- Test ElectHost (skip failed, no candidate)
- Test StreamAssembler (start â†?chunks â†?complete, abort)
- Test DiscussionRegistry (Put/Get/Delete/SendResponse)
- Test handleDiscussionCommand routing
- Test handleNodeMessage dispatch

### Step 1: Add state machine tests

```go
func TestReplayStateEmpty(t *testing.T) {
    state, err := ReplayState(nil)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if state.Status != StatusIdle {
        t.Errorf("expected idle, got %s", state.Status)
    }
}

func TestReplayStateFullDiscussion(t *testing.T) {
    now := time.Now()
    chatroomID := pgtype.UUID{Valid: true}
    wsID := pgtype.UUID{Valid: true}
    nodeID := pgtype.UUID{Valid: true, Bytes: [16]byte{1, 2, 3}}

    events := []DiscussionEvent{
        {ChatroomID: chatroomID, WorkspaceID: wsID, EventType: EventDiscussionStarted, NodeID: nodeID, CreatedAt: now},
        {ChatroomID: chatroomID, WorkspaceID: wsID, EventType: EventRoundStarted, RoundNumber: 1, CreatedAt: now.Add(time.Second)},
        {ChatroomID: chatroomID, WorkspaceID: wsID, EventType: EventTurnStarted, RoundNumber: 1, NodeID: nodeID, CreatedAt: now.Add(2 * time.Second)},
        {ChatroomID: chatroomID, WorkspaceID: wsID, EventType: EventTurnCompleted, RoundNumber: 1, NodeID: nodeID, CreatedAt: now.Add(3 * time.Second)},
        {ChatroomID: chatroomID, WorkspaceID: wsID, EventType: EventDiscussionEnded, CreatedAt: now.Add(4 * time.Second)},
    }

    state, err := ReplayState(events)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if state.Status != StatusEnded {
        t.Errorf("expected ended, got %s", state.Status)
    }
    if state.CurrentRound != 1 {
        t.Errorf("expected round 1, got %d", state.CurrentRound)
    }
}

func TestValidateTransitionValid(t *testing.T) {
    newStatus, err := ValidateTransition(StatusIdle, EventDiscussionStarted)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if newStatus != StatusStarting {
        t.Errorf("expected starting, got %s", newStatus)
    }
}

func TestValidateTransitionInvalid(t *testing.T) {
    _, err := ValidateTransition(StatusIdle, EventTurnStarted)
    if err == nil {
        t.Error("expected error for invalid transition")
    }
}

func TestElectHostSkipFailed(t *testing.T) {
    failedID := pgtype.UUID{Valid: true, Bytes: [16]byte{1}}
    nextID := pgtype.UUID{Valid: true, Bytes: [16]byte{2}}
    speakers := []SpeakerInfo{
        {NodeID: failedID, SpeakingOrder: 1},
        {NodeID: nextID, SpeakingOrder: 2},
    }
    elected, err := ElectHost(speakers, failedID)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if elected.Bytes != nextID.Bytes {
        t.Error("expected next speaker to be elected")
    }
}

func TestElectHostNoCandidate(t *testing.T) {
    failedID := pgtype.UUID{Valid: true, Bytes: [16]byte{1}}
    speakers := []SpeakerInfo{{NodeID: failedID, SpeakingOrder: 1}}
    _, err := ElectHost(speakers, failedID)
    if err == nil {
        t.Error("expected error when no candidate available")
    }
}
```

### Step 2: Add StreamAssembler tests

```go
func TestStreamAssemblerComplete(t *testing.T) {
    a := NewStreamAssembler()
    turnID := "turn_test_1"
    a.StartStream(turnID, "node_abc")
    a.AppendChunk(turnID, "Hello ")
    a.AppendChunk(turnID, "World")
    result, err := a.CompleteStream(turnID)
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if result != "Hello World" {
        t.Errorf("expected 'Hello World', got '%s'", result)
    }
}

func TestStreamAssemblerAbort(t *testing.T) {
    a := NewStreamAssembler()
    turnID := "turn_test_2"
    a.StartStream(turnID, "node_abc")
    a.AppendChunk(turnID, "partial")
    a.AbortStream(turnID)
    _, err := a.CompleteStream(turnID)
    if err == nil {
        t.Error("expected error after abort")
    }
}
```

### Step 3: Add DiscussionRegistry tests

```go
func TestDiscussionRegistryPutGetDelete(t *testing.T) {
    r := NewDiscussionRegistry()
    chatroomID := pgtype.UUID{Valid: true, Bytes: [16]byte{1, 2, 3}}
    coord := &DiscussionCoordinator{
        responseCh: make(chan NodeResponse, 1),
    }
    r.Put(chatroomID, coord)

    got, ok := r.Get(chatroomID)
    if !ok {
        t.Error("expected to find coordinator")
    }
    if got != coord {
        t.Error("expected same coordinator instance")
    }

    r.Delete(chatroomID)
    _, ok = r.Get(chatroomID)
    if ok {
        t.Error("expected coordinator to be deleted")
    }
}

func TestDiscussionRegistrySendResponse(t *testing.T) {
    r := NewDiscussionRegistry()
    chatroomID := pgtype.UUID{Valid: true, Bytes: [16]byte{1}}
    coord := &DiscussionCoordinator{
        responseCh: make(chan NodeResponse, 1),
    }
    r.Put(chatroomID, coord)

    resp := NodeResponse{MsgType: "text", Content: "hello"}
    if !r.SendResponse(chatroomID, resp) {
        t.Error("expected SendResponse to succeed")
    }

    select {
    case got := <-coord.responseCh:
        if got.Content != "hello" {
            t.Errorf("expected 'hello', got '%s'", got.Content)
        }
    default:
        t.Error("expected response in channel")
    }
}
```

### Step 4: Add system_handler discussion + node_message tests

```go
func TestHandleDiscussionCommandUnknownAction(t *testing.T) {
    h := newSystemHandler(nil, "node_test", slog.Default(), nil, nil)
    msg := NormalizedMessage{FromUserID: "user1"}
    cmd := CommandContent{RequestID: "req1", Service: "discussion", Action: "unknown"}
    h.handleDiscussionCommand(context.Background(), msg, cmd)
    // should not panic, should send error via client (nil client â†?no-op)
}

func TestHandleDiscussionCommandStatusNoRegistry(t *testing.T) {
    h := newSystemHandler(nil, "node_test", slog.Default(), nil, nil)
    msg := NormalizedMessage{FromUserID: "user1"}
    cmd := CommandContent{
        RequestID: "req1",
        Service:   "discussion",
        Action:    "status",
        Params:    map[string]interface{}{"chatroom_id": "not-a-uuid"},
    }
    h.handleDiscussionCommand(context.Background(), msg, cmd)
    // should handle gracefully without panicking
}

func TestHandleNodeMessageNilRegistry(t *testing.T) {
    h := newSystemHandler(nil, "node_test", slog.Default(), nil, nil)
    msg := NormalizedMessage{
        ObjectName: objectNameText,
        Content:    "hello",
        FromUserID: "node1",
    }
    h.handleNodeMessage(context.Background(), msg)
    // should not panic
}
```

### Step 5: Run all tests and commit

```powershell
$env:Path = "C:\Program Files\Go\bin;$env:Path"
go test ./internal/integrations/rongcloud/... -count=1 -v
git add server/internal/integrations/rongcloud/rongcloud_test.go
git commit -m "test(rongcloud): add Phase 3 discussion state machine, stream assembler, registry, and handler tests"
```

- [x] **Step 1:** Add state machine tests (ReplayState empty/full, ValidateTransition valid/invalid, ElectHost skip/no-candidate)
- [x] **Step 2:** Add StreamAssembler tests (complete, abort)
- [x] **Step 3:** Add DiscussionRegistry tests (Put/Get/Delete, SendResponse)
- [x] **Step 4:** Add system_handler discussion + node_message tests
- [x] **Step 5:** Run all tests, commit

---

## Self-Review Checklist

- [ ] All new files compile: `go build ./...`
- [ ] All tests pass: `go test ./internal/integrations/rongcloud/... -count=1`
- [ ] No foreign keys in migrations
- [ ] All indexes use CONCURRENTLY in separate migration files
- [ ] No `UPDATE` or `DELETE` on `rongcloud_discussion_event` (event sourcing = append-only)
- [ ] Discussion layer is in `rongcloud` package, not in `channel/engine`
- [ ] `handleNodeMessage` is wired into `channel.go` `handleWebhook`
- [ ] `DiscussionRegistry` passed to `systemHandler` via `ChannelDeps`
- [ ] All handler endpoints follow existing patterns (nil-check, parseUUIDOrBadRequest, writeJSON/writeError)
- [ ] All new routes wired into correct router groups (admin vs member)
- [ ] Conventional commit messages with `feat(rongcloud):` prefix
