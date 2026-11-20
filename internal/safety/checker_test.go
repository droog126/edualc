package safety

import (
	"testing"
	"time"

	"mini-cage-relay/internal/exitpolicy"
)

func TestReportAuthorizesFailClosed(t *testing.T) {
	now := time.Now().UTC()
	rep := Report{
		Policy:    PolicyVersion,
		ID:        "abc123",
		CreatedAt: now,
	}
	for _, id := range RequiredCheckIDs {
		rep.Checks = append(rep.Checks, Check{
			ID:        id,
			Title:     id,
			State:     exitpolicy.Passed,
			Critical:  true,
			Evidence:  "ok",
			CheckedAt: now,
		})
	}
	if err := rep.Authorizes(now.Add(time.Minute)); err != nil {
		t.Fatalf("expected report to authorize, got error: %v", err)
	}

	// Expire report -> must reject
	if err := rep.Authorizes(now.Add(ReportLifetime + time.Minute)); err == nil {
		t.Fatalf("expected expired report to fail authorization")
	}

	// Fail one critical check -> must reject
	rep.Checks[3].State = exitpolicy.Failed
	if err := rep.Authorizes(now.Add(time.Minute)); err == nil {
		t.Fatalf("expected failed critical check to reject authorization")
	}
}
