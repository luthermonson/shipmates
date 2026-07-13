package dashboard

import (
	"context"
	"errors"
	"github.com/luthermonson/shipmates/internal/livesession"
	"testing"
)

type fakeTransport struct {
	ids       []string
	releases  []string
	attachErr error
}

func (f *fakeTransport) Attach(context.Context, string, AttachRequest) (livesession.Attach, error) {
	if f.attachErr != nil {
		return livesession.Attach{}, f.attachErr
	}
	id := f.ids[0]
	f.ids = f.ids[1:]
	return livesession.Attach{Snapshot: livesession.Snapshot{SessionID: "session", ThreadID: "thread", State: livesession.Idle}, ControllerID: id}, nil
}
func (f *fakeTransport) Release(_ context.Context, _ string, _ string, id string) error {
	f.releases = append(f.releases, id)
	return nil
}
func (f *fakeTransport) Heartbeat(context.Context, string, string, string) error { return nil }
func (f *fakeTransport) Sync(context.Context, string, string, string, uint64) (livesession.Attach, error) {
	return livesession.Attach{}, nil
}
func (f *fakeTransport) Action(context.Context, string, ActionRequest) (livesession.ControlResult, error) {
	return livesession.ControlResult{}, nil
}
func (f *fakeTransport) ResolveApproval(context.Context, string, ApprovalRequest) (livesession.ApprovalResult, error) {
	return livesession.ApprovalResult{}, nil
}
func TestAttachReconnectUsesNewLeaseAndCleanupIsIdempotent(t *testing.T) {
	f := &fakeTransport{ids: []string{"one", "two"}}
	ctx := context.Background()
	one, err := Connect(ctx, f, "backend", AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err = one.Close(ctx); err != nil {
		t.Fatal(err)
	}
	_ = one.Close(ctx)
	two, err := Connect(ctx, f, "backend", AttachRequest{SessionID: "session", After: 7})
	if err != nil {
		t.Fatal(err)
	}
	_ = two.Close(ctx)
	if len(f.releases) != 2 || f.releases[0] != "one" || f.releases[1] != "two" {
		t.Fatalf("releases=%v", f.releases)
	}
}
func TestAttachFailureHasNoRelease(t *testing.T) {
	f := &fakeTransport{attachErr: errors.New("down")}
	if _, err := Connect(context.Background(), f, "backend", AttachRequest{}); err == nil {
		t.Fatal("expected error")
	}
	if len(f.releases) != 0 {
		t.Fatal(f.releases)
	}
}
