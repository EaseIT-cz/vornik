package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/approval"
)

// GitHub #806 (agent-administered design part 22 §22.2.16d, part 06 §6.5
// amendment 2026-11-17): the console inbox clears its informational cards.
// A failed apply gets Dismiss, a withheld-tools notice Acknowledge; both
// write through the approver-device service the phone uses, so clearing one
// surface clears the other. Nothing is applied here.

// NoticeCard is one withheld-tools notice for the console.
type NoticeCard struct {
	Key  string
	Text string
}

// ApproverNoticeActions are the console's writes for #806. ErrNotChangeable
// from either means the card is gone or never was one (409).
type ApproverNoticeActions struct {
	Notices     func(ctx context.Context) []NoticeCard
	Acknowledge func(ctx context.Context, key, actor string) error
	Dismiss     func(ctx context.Context, id, actor string) error
}

// ErrNotChangeable means the card no longer applies (already gone, not a failed
// apply, or not a live notice).
var ErrNotChangeable = errors.New("ui: the card no longer applies")

// WithApproverNoticeActions wires the #806 writes; nil leaves the cards
// read-only and hides the notices.
func WithApproverNoticeActions(a *ApproverNoticeActions) ServerOption {
	return func(s *Server) { s.approverNotices = a }
}

// loadApproverNotices lists the notices for the same viewer that sees the
// failed applies: operator-wide, never a project-scoped one.
func (s *Server) loadApproverNotices(r *http.Request) []NoticeCard {
	if s.approverNotices == nil || s.approverNotices.Notices == nil || scopeQueryIDs(r) != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	return s.approverNotices.Notices(ctx)
}

// approverNoticeGate admits a same-origin POST by an admin-class caller or
// the managed instance's owner. A project-scoped viewer gets 404: the cards
// are hidden from it on read.
func (s *Server) approverNoticeGate(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if s.approverNotices == nil {
		http.NotFound(w, r)
		return false
	}
	if err := approval.CheckRequest(r); err != nil {
		approval.WriteError(w, r, err)
		return false
	}
	switch api.OperatorSurfaceAccessFor(r, s.isAdminKeyForSurface, api.SurfaceApproverNotices) {
	case api.OperatorSurfaceAllowed:
		return true
	case api.OperatorSurfaceRefused:
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		http.NotFound(w, r)
	}
	return false
}

// ApproverNoticeRouter serves POST /inbox/apply-failed/<id>/dismiss and
// POST /inbox/notices/<key>/ack.
func (s *Server) ApproverNoticeRouter(w http.ResponseWriter, r *http.Request) {
	if !s.approverNoticeGate(w, r) {
		return
	}
	actor := "operator:" + s.operatorIDForRequest(r)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var err error
	switch {
	case strings.HasPrefix(r.URL.Path, "/inbox/apply-failed/") && strings.HasSuffix(r.URL.Path, "/dismiss"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/inbox/apply-failed/"), "/dismiss")
		if id == "" || strings.Contains(id, "/") || s.approverNotices.Dismiss == nil {
			http.NotFound(w, r)
			return
		}
		err = s.approverNotices.Dismiss(ctx, id, actor)
	case strings.HasPrefix(r.URL.Path, "/inbox/notices/") && strings.HasSuffix(r.URL.Path, "/ack"):
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/inbox/notices/"), "/ack")
		if key == "" || strings.Contains(key, "/") || s.approverNotices.Acknowledge == nil {
			http.NotFound(w, r)
			return
		}
		err = s.approverNotices.Acknowledge(ctx, key, actor)
	default:
		http.NotFound(w, r)
		return
	}
	switch {
	case errors.Is(err, ErrNotChangeable):
		http.Error(w, "this card no longer applies", http.StatusConflict)
		return
	case err != nil:
		s.logger.Warn().Err(err).Msg("inbox: approver notice action failed")
		http.Error(w, "could not save that; try again", http.StatusInternalServerError)
		return
	}
	s.redirectInbox(w, r, "")
}
