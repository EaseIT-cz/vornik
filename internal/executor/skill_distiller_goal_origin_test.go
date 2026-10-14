package executor

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #347 (learning-loop design, amendment 2026-10-13). The distiller
// judged a goal's origin by inference from creation_source, the API key
// (an attribution field) and the chat turn. A third-party surface that
// gains key attribution (the GitHub channel or a webhook attributed to the
// key that configured it) would have passed. The recorded tasks.goal_origin
// is now checked first, and a task whose goal changed after creation (an
// amended brief, a conversation) is refused.

// keyedUserRoot is the shape the inference alone accepts: a keyed USER root
// with no chat turn.
func keyedUserRoot(origin string) *persistence.Task {
	t := ordinaryTask()
	t.CreationSource = persistence.TaskCreationSourceUser
	t.GoalOrigin = origin
	return t
}

func TestDistiller_ThirdPartyGoalOriginIsNotDistilled(t *testing.T) {
	for _, origin := range []string{
		persistence.TaskGoalOriginWebhook, persistence.TaskGoalOriginGitHub,
		persistence.TaskGoalOriginGitHubCI, persistence.TaskGoalOriginChat,
		persistence.TaskGoalOriginA2A, persistence.TaskGoalOriginBroker,
		persistence.TaskGoalOriginReminder,
		// Derived origins, judged when the row is a root (OrphanChildren).
		persistence.TaskGoalOriginCheckpoint, persistence.TaskGoalOriginRoute,
		persistence.TaskGoalOriginDelegation, persistence.TaskGoalOriginSpawnProject,
		persistence.TaskGoalOriginCallProject, persistence.TaskGoalOriginFork,
		persistence.TaskGoalOriginCounterfactual,
	} {
		t.Run(origin, func(t *testing.T) {
			f := newDistillFixture(t)
			w := f.wireTaint()
			w.step(t, "t1", "file_read")
			f.assertRefused(t, f.runOrdinary(t, w, keyedUserRoot(origin)), "goal_third_party")
		})
	}
}

func TestDistiller_UnrecordedGoalOriginIsNotDistilled(t *testing.T) {
	for name, origin := range map[string]string{"null": "", "unknown": "evil", "case": "REST"} {
		t.Run(name, func(t *testing.T) {
			f := newDistillFixture(t)
			w := f.wireTaint()
			w.step(t, "t1", "file_read")
			f.assertRefused(t, f.runOrdinary(t, w, keyedUserRoot(origin)), "goal_origin_unrecorded")
		})
	}
}

// The marker narrows; it never widens: a first-party origin still has to
// pass the inference, in that order.
func TestDistiller_FirstPartyGoalOriginStillNeedsTheInference(t *testing.T) {
	t.Run("rest keyed distils", func(t *testing.T) {
		f := newDistillFixture(t)
		w := f.wireTaint()
		w.step(t, "t1", "file_read")
		if n := f.runOrdinary(t, w, keyedUserRoot(persistence.TaskGoalOriginREST)); n != 1 {
			t.Fatalf("a keyed REST goal with no chat turn must still distil; got %d drafts", n)
		}
	})
	turn := "turn-1"
	for name, mutate := range map[string]func(*persistence.Task){
		"rest with a chat turn": func(t *persistence.Task) { t.ChatTurnID = &turn },
		"rest unkeyed":          func(t *persistence.Task) { t.CreatedByAPIKeyID = nil },
		"doctor unkeyed": func(t *persistence.Task) {
			t.GoalOrigin, t.CreatedByAPIKeyID = persistence.TaskGoalOriginDoctor, nil
		},
		// Operator decision 2026-10-13 (learning-loop open question 6): a
		// doctor goal stays refused even if a key were ever attributed to it.
		"doctor keyed": func(t *persistence.Task) { t.GoalOrigin = persistence.TaskGoalOriginDoctor },
		// The ui exemption drops only the key requirement, not the chat-turn
		// one.
		"ui with a chat turn": func(t *persistence.Task) {
			t.GoalOrigin, t.CreatedByAPIKeyID, t.ChatTurnID = persistence.TaskGoalOriginUI, nil, &turn
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDistillFixture(t)
			w := f.wireTaint()
			w.step(t, "t1", "file_read")
			task := keyedUserRoot(persistence.TaskGoalOriginREST)
			mutate(task)
			f.assertRefused(t, f.runOrdinary(t, w, task), "goal_origin_unproven")
		})
	}
}

// Operator decision 2026-10-13 on learning-loop open question 6 (GitHub
// #347): a task created from the web UI form, typed by a signed-in operator,
// is first-party and distillable without an API key. Before the decision it
// refused goal_origin_unproven (the inference's key requirement).
func TestDistiller_UIGoalWithoutAKeyIsDistilled(t *testing.T) {
	f := newDistillFixture(t)
	w := f.wireTaint()
	w.step(t, "t1", "file_read")
	task := keyedUserRoot(persistence.TaskGoalOriginUI)
	task.CreatedByAPIKeyID = nil
	if n := f.runOrdinary(t, w, task); n != 1 {
		t.Fatalf("an unkeyed ui-form goal with no chat turn must distil; got %d drafts", n)
	}
	// Keyed or not: the decision drops the key requirement, it does not
	// invert it (review-20261013-daf4 F3).
	f2 := newDistillFixture(t)
	w2 := f2.wireTaint()
	w2.step(t, "t1", "file_read")
	if n := f2.runOrdinary(t, w2, keyedUserRoot(persistence.TaskGoalOriginUI)); n != 1 {
		t.Fatalf("a keyed ui-form goal with no chat turn must distil; got %d drafts", n)
	}
}

func TestDistiller_TaskWithMessagesIsNotDistilled(t *testing.T) {
	now := time.Now()
	for name, mutate := range map[string]func(*persistence.Task){
		"conversation":   func(t *persistence.Task) { t.MessageCount = 1 },
		"amended brief":  func(t *persistence.Task) { t.BriefAmendedAt = &now },
		"child messages": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newDistillFixture(t)
			w := f.wireTaint()
			w.step(t, "t1", "file_read")
			task := ordinaryTask()
			if mutate != nil {
				mutate(task)
			} else {
				f.child("kid-1")
				f.tasks.tasks["kid-1"].MessageCount = 2
				w.step(t, "kid-1", "file_read")
			}
			f.assertRefused(t, f.runOrdinary(t, w, task), "task_messages")
		})
	}
}
