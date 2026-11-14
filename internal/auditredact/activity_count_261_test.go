package auditredact

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #261: the console holds these wrappers, not the repositories, so the
// adoption panel's counts reach the database only if they forward CountSince;
// over a repository that cannot count they say so instead of answering 0.

type countingToolRepo261 struct {
	persistence.ToolAuditRepository
}

func (countingToolRepo261) CountSince(context.Context, string, time.Time) (int64, error) {
	return 7, nil
}

type countingChatRepo261 struct {
	persistence.ChatAuditRepository
}

func (countingChatRepo261) CountSince(context.Context, string, time.Time) (int64, error) {
	return 9, nil
}

func TestActivityCount261_WrappersForward(t *testing.T) {
	logger := zerolog.Nop()
	tool := NewBypassed(countingToolRepo261{}, Reason("test"), &logger)
	if n, err := tool.CountSince(context.Background(), "p", time.Now()); err != nil || n != 7 {
		t.Fatalf("tool wrapper CountSince = %d, %v; want 7", n, err)
	}
	chat := NewChatAudit(countingChatRepo261{}, nil, nil, &logger)
	if n, err := chat.CountSince(context.Background(), "p", time.Now()); err != nil || n != 9 {
		t.Fatalf("chat wrapper CountSince = %d, %v; want 9", n, err)
	}
}

type plainToolRepo261 struct {
	persistence.ToolAuditRepository
}
type plainChatRepo261 struct {
	persistence.ChatAuditRepository
}

func TestActivityCount261_NotCountedOverARepoThatCannot(t *testing.T) {
	logger := zerolog.Nop()
	if _, err := NewBypassed(plainToolRepo261{}, Reason("test"), &logger).CountSince(context.Background(), "", time.Now()); !errors.Is(err, persistence.ErrActivityNotCounted) {
		t.Fatalf("tool wrapper over a non-counting repo: err = %v, want ErrActivityNotCounted", err)
	}
	if _, err := NewChatAudit(plainChatRepo261{}, nil, nil, &logger).CountSince(context.Background(), "", time.Now()); !errors.Is(err, persistence.ErrActivityNotCounted) {
		t.Fatalf("chat wrapper over a non-counting repo: err = %v, want ErrActivityNotCounted", err)
	}
}
