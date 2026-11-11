package runner

import "testing"

var candidates = []string{"cook", "vocab", "__scheduler__"}

func TestParseRouteJSON(t *testing.T) {
	r := ParseRoute(`{"projects":["cook"],"attachmentProject":null}`, candidates)
	if len(r.Projects) != 1 || r.Projects[0] != "cook" || r.AttachmentProject != "" {
		t.Fatalf("unexpected route: %+v", r)
	}
}

func TestParseRouteStripsCodeFencesAndProse(t *testing.T) {
	r := ParseRoute("```json\n{\"projects\":[\"cook\",\"vocab\"],\"attachmentProject\":\"cook\"}\n```", candidates)
	if len(r.Projects) != 2 || r.AttachmentProject != "cook" {
		t.Fatalf("unexpected route: %+v", r)
	}
}

func TestParseRouteDeduplicates(t *testing.T) {
	r := ParseRoute(`{"projects":["cook","cook"]}`, candidates)
	if len(r.Projects) != 1 {
		t.Fatalf("expected dedup, got %+v", r)
	}
}

func TestParseRouteRejectsUnknownProject(t *testing.T) {
	if r := ParseRoute(`{"projects":["nope"]}`, candidates); len(r.Projects) != 0 {
		t.Fatalf("unknown project must be rejected: %+v", r)
	}
}

func TestParseRouteSchedulerIsExclusive(t *testing.T) {
	if r := ParseRoute(`{"projects":["__scheduler__","cook"]}`, candidates); len(r.Projects) != 0 {
		t.Fatalf("scheduler cannot be combined: %+v", r)
	}
	r := ParseRoute(`{"projects":["__scheduler__"]}`, candidates)
	if len(r.Projects) != 1 || r.Projects[0] != "__scheduler__" {
		t.Fatalf("expected scheduler route: %+v", r)
	}
}

func TestParseRouteAttachmentMustBeSelected(t *testing.T) {
	if r := ParseRoute(`{"projects":["cook"],"attachmentProject":"vocab"}`, candidates); len(r.Projects) != 0 {
		t.Fatalf("attachment outside the selection must reset the route: %+v", r)
	}
	if r := ParseRoute(`{"projects":["cook"],"attachmentProject":"__scheduler__"}`, candidates); len(r.Projects) != 0 {
		t.Fatalf("scheduler attachment must reset the route: %+v", r)
	}
}

func TestParseRouteResetContext(t *testing.T) {
	if r := ParseRoute("RESET_CONTEXT", candidates); !r.ResetContext {
		t.Fatalf("expected reset: %+v", r)
	}
}

func TestParseRouteUnknown(t *testing.T) {
	if r := ParseRoute("UNKNOWN", candidates); len(r.Projects) != 0 || r.ResetContext {
		t.Fatalf("expected empty route: %+v", r)
	}
}

func TestParseRouteBareName(t *testing.T) {
	if r := ParseRoute("cook", candidates); len(r.Projects) != 1 || r.Projects[0] != "cook" {
		t.Fatalf("single bare name should route: %+v", r)
	}
	if r := ParseRoute("cook and vocab", candidates); len(r.Projects) != 0 {
		t.Fatalf("ambiguous bare names must not route: %+v", r)
	}
}

func TestParseRouteSchedulerAlias(t *testing.T) {
	if r := ParseRoute("每天早上 8 点提醒我", candidates); len(r.Projects) != 1 || r.Projects[0] != "__scheduler__" {
		t.Fatalf("expected scheduler alias route: %+v", r)
	}
}

func TestShouldRecordConversationTurn(t *testing.T) {
	if !ShouldRecordConversationTurn([]string{"read"}, 0) {
		t.Fatal("a read-only task is still recorded")
	}
	if ShouldRecordConversationTurn([]string{"clear_conversation_context"}, 0) {
		t.Fatal("a pure context clear must not be recorded")
	}
	if !ShouldRecordConversationTurn([]string{"clear_conversation_context", "write"}, 0) {
		t.Fatal("clear + data tool must be recorded")
	}
	if !ShouldRecordConversationTurn([]string{"clear_conversation_context"}, 1) {
		t.Fatal("clear + saved image must be recorded")
	}
}

func TestShouldClearConversationForRoute(t *testing.T) {
	if !ShouldClearConversationForRoute(true, true, "im") {
		t.Fatal("reset on an IM session must clear")
	}
	if ShouldClearConversationForRoute(true, false, "im") {
		t.Fatal("non-IM sessions never clear")
	}
	if ShouldClearConversationForRoute(false, true, "im") {
		t.Fatal("no reset means no clear")
	}
}
