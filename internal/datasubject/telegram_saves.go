package datasubject

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Telegram's named saves (slice 5d-2, design §15.18.1). "/save <name>" writes
// a chat's active lane to <dir of session_path>/saves/<chat_id>/<name>.json,
// outside channel_sessions and outside the fence. The erasure reaches them
// from vornikctl, which reads the same config, and deletes them: Telegram
// records no speaker per message, so a shared Telegram session is deleted
// rather than masked, and its saves with it (#346 Q5, §15.19).
//
// see LLD § https://docs.vornik.io §15.18.1

// TelegramSaves locates the named saves. Root is the directory holding
// telegram.session_path; empty means the bot keeps no files.
type TelegramSaves struct {
	Root string
}

// eraseTelegramSaves deletes the chat's named saves. Called for every
// telegram session action that got past the fence and identifier checks,
// whatever happened to the row itself.
func (e *Executor) eraseTelegramSaves(a Action, chatID string, res *ErasureResult) {
	if e.TelegramSaves == nil || e.TelegramSaves.Root == "" {
		return
	}
	if _, err := strconv.ParseInt(chatID, 10, 64); err != nil {
		return // not a chat id: no save directory can exist for it
	}
	root := e.TelegramSaves.Root
	if _, err := os.Stat(root); err != nil {
		e.deferRedaction(a, res, fmt.Sprintf("the Telegram saves directory %s is not reachable from this "+
			"host (%v); run the erasure where the daemon runs", filepath.Join(root, "saves", chatID), err))
		return
	}
	dir := filepath.Join(root, "saves", chatID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("list the Telegram saves in %s: %v", dir, err)})
		return
	}
	names := make([]string, 0, len(entries))
	for _, en := range entries {
		if !en.IsDir() && strings.HasSuffix(en.Name(), ".json") {
			names = append(names, en.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		e.eraseTelegramSave(a, filepath.Join(dir, name), res)
	}
}

func (e *Executor) eraseTelegramSave(a Action, path string, res *ErasureResult) {
	rowID := a.RowID + " (saved copy " + strings.TrimSuffix(filepath.Base(path), ".json") + ")"
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: rowID,
			Err: fmt.Sprintf("remove %s: %v", path, err)})
		return
	}
	res.Deleted = append(res.Deleted, DeletedRecord{Table: a.Table, RowID: rowID})
}

// telegramChatID returns the chat id of a telegram session action, from the
// row id or, for a legacy bare id, from the loaded row.
func telegramChatID(ctx context.Context, store ChannelSessionStore, rowID string) (string, bool) {
	if kind, sid, err := ParseChannelSessionRowID(rowID); err == nil {
		return sid, kind == "telegram"
	}
	s, found, err := store.LoadSession(ctx, rowID)
	if err != nil || !found {
		return "", false
	}
	return s.SessionID, s.Kind == "telegram"
}
