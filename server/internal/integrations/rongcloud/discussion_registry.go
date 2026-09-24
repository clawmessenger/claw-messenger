package rongcloud

import (
	"encoding/hex"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
)

type DiscussionRegistry struct {
	coordinators sync.Map
}

func NewDiscussionRegistry() *DiscussionRegistry {
	return &DiscussionRegistry{}
}

func (r *DiscussionRegistry) Get(chatroomID pgtype.UUID) (*DiscussionCoordinator, bool) {
	v, ok := r.coordinators.Load(chatroomKey(chatroomID))
	if !ok {
		return nil, false
	}
	c, ok := v.(*DiscussionCoordinator)
	return c, ok
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
	var buf [36]byte
	hex.Encode(buf[:], id.Bytes[:])
	return string(buf[:])
}
