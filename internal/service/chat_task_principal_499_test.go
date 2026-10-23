package service

// GitHub #499 (operator decision A19; design 22, "Amendment 2026-10-21
// (#482)", part 8a): a chat identity linked to an admin account is an
// identifiable operator, so a task it starts from its own private chat
// carries `user:<account id>`; unlinked and group turns carry none, and
// memory verification treats those tasks as delegated. Driven through the
// production dispatcher (Container.initDispatcher) and the Telegram receiver
// wireTelegramReceiver attaches, over real SQLite, with a scripted model that
// calls create_task: the level a chat user reaches.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/telegram"
)

// createTaskModel answers the first request with a create_task call and the
// next with a final message.
func createTaskModel(t *testing.T) string {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"project_id": "alpha", "type": "research", "prompt": "look into it", "await_completion": false})
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chat.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		n++
		msg := map[string]any{"role": "assistant", "content": "Started."}
		finish := "stop"
		if n == 1 {
			msg = map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{
				{"id": "c499", "type": "function", "function": map[string]any{"name": "create_task", "arguments": string(args)}},
			}}
			finish = "tool_calls"
		}
		body, _ := json.Marshal(map[string]any{"id": "r", "object": "chat.completion", "model": "m",
			"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTelegram499_ChatTaskPrincipal(t *testing.T) {
	const adminID, userID, groupID = 1001, 1002, -5001
	for _, tc := range []struct {
		name    string
		chatID  int64
		speaker int64
		want    string
	}{
		{"admin-linked speaker in a private chat", adminID, adminID, "user:u-admin"},
		{"admin-linked speaker in a group", groupID, adminID, ""},
		{"linked user-role speaker", userID, userID, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bot, err := telegram.NewBot(telegram.BotConfig{
				Token: "t",
				AllowedUsers: map[int64]telegram.UserAccess{
					adminID: {Allowed: true, Projects: []string{"*"}},
					userID:  {Allowed: true, Projects: []string{"*"}},
				},
			}, nil, telegram.WithHTTPClient(&http.Client{Transport: okTransport{}}))
			if err != nil {
				t.Fatalf("NewBot: %v", err)
			}
			bot.SetIdentityShim(chatauth.New(resolver480{principals: map[string]*authz.Principal{
				"telegram:1001": admin480, "telegram:1002": user480,
			}}, nil, zerolog.Nop()))
			db := sqlitetest.Memory(t)
			tasks := sqlite.NewTaskRepository(db.DB)
			c := &Container{
				Logger:      zerolog.Nop(),
				ChatClient:  chat.NewClient(createTaskModel(t), "k", "m"),
				Config:      &config.Config{},
				TelegramBot: bot,
				repos:       &storage.Repositories{Tasks: tasks, Executions: sqlite.NewExecutionRepository(db.DB)},
			}
			c.initDispatcher()
			recv := bot.Receiver()
			if recv == nil {
				t.Fatal("no receiver")
			}
			msg := telegram.MessageToChannelMessage(&telegram.Message{ID: 1, ChatID: tc.chatID, UserID: tc.speaker,
				Text: "please research this", ProjectOverride: "alpha"})
			if err := recv.Receive(context.Background(), msg); err != nil {
				t.Fatalf("Receive: %v", err)
			}
			alpha := "alpha"
			got, err := tasks.List(context.Background(), persistence.TaskFilter{ProjectID: &alpha})
			if err != nil || len(got) != 1 {
				t.Fatalf("tasks created: %d (%v)", len(got), err)
			}
			actor := ""
			if got[0].CreatedByActor != nil {
				actor = *got[0].CreatedByActor
			}
			if actor != tc.want {
				t.Fatalf("created_by_actor = %q, want %q", actor, tc.want)
			}
		})
	}
}
