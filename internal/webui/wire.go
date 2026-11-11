package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/crossoverJie/botler-agent/internal/channels/wechat"
	"github.com/crossoverJie/botler-agent/internal/config"
	"github.com/crossoverJie/botler-agent/internal/scheduler"
)

var wireOnce sync.Once

// WireRuntime installs the production scheduler and WeChat backends. It is idempotent and called
// automatically by Start; tests that want hermetic behavior install their own backends before
// hitting the handler.
func WireRuntime() {
	wireOnce.Do(func() {
		SetSchedulesBackend(SchedulesBackend{
			Load:   loadSchedulesAny,
			Save:   saveSchedulesAny,
			Reload: scheduler.Reload,
			NextFire: func(spec map[string]any, afterMs int64) (int64, bool, error) {
				return previewNextFire(spec, afterMs)
			},
			Overview:     func() any { return mustOverview() },
			HistoryIndex: func() any { return mustHistoryIndex() },
			Runs: func(id string, limit, offset int) any {
				page, err := scheduler.ScheduleRuns(id, limit, offset)
				if err != nil {
					return map[string]any{"logs": []any{}, "total": 0}
				}
				return page
			},
			Stats: func(id string) any {
				stats, err := scheduler.ScheduleStats(id)
				if err != nil {
					return map[string]any{"scheduleId": id, "totalRuns": 0}
				}
				return stats
			},
		})
		SetWeChatBackend(WeChatBackend{
			Status:       wechatStatusFromDisk,
			StartPairing: startPairing,
			PairStatus:   func() any { return wechat.PairStatus() },
			CancelPairing: func() {
				wechat.CancelPairing()
			},
		})
	})
}

// loadSchedulesAny renders schedules.json into JSON-generic values so the WebUI can echo the file
// back without knowing the scheduler types.
func loadSchedulesAny() ([]any, error) {
	entries, err := scheduler.LoadSchedules()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []any{}
	}
	return out, nil
}

// saveSchedulesAny validates + normalizes + atomically writes the entries the UI submitted.
func saveSchedulesAny(items []any) error {
	list := make([]scheduler.Schedule, 0, len(items))
	for i, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			return fmt.Errorf("schedule[%d]: %w", i, err)
		}
		var entry scheduler.Schedule
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("schedule[%d]: %w", i, err)
		}
		list = append(list, entry)
	}
	if _, err := scheduler.SaveSchedules(list); err != nil {
		return err
	}
	return nil
}

// previewNextFire computes the next fire of an unsaved candidate entry. hasNext=false means the
// entry can never fire (a spent `once`), which the UI renders as "no upcoming fire".
func previewNextFire(spec map[string]any, afterMs int64) (int64, bool, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return 0, false, err
	}
	var entry scheduler.Schedule
	if err := json.Unmarshal(raw, &entry); err != nil {
		return 0, false, err
	}
	if entry.Timezone == "" {
		entry.Timezone = "Asia/Shanghai"
	}
	next, err := scheduler.NextFireEpoch(entry, afterMs)
	if err != nil {
		return 0, false, err
	}
	if next == scheduler.NoFire {
		return 0, false, nil
	}
	return next, true, nil
}

func mustOverview() any {
	list, err := scheduler.Overview()
	if err != nil {
		return []any{}
	}
	return list
}

func mustHistoryIndex() any {
	list, err := scheduler.HistoryIndex()
	if err != nil {
		return []any{}
	}
	return list
}

// wechatStatusFromDisk assembles the read-only WeChat panel from the persisted account, the
// process-local reminder loop state and the owner's context entry.
func wechatStatusFromDisk() any {
	resolved := wechat.ResolveAccount()
	account := WeChatAccount{Configured: resolved.Configured, Owner: resolved.UserID}
	if data := wechat.LoadAccount(); data != nil && data.SavedAt != "" {
		if t, err := time.Parse(time.RFC3339, data.SavedAt); err == nil {
			ms := t.UnixMilli()
			account.LoginAtMs = &ms
		} else if t, err := time.Parse(time.RFC3339Nano, data.SavedAt); err == nil {
			ms := t.UnixMilli()
			account.LoginAtMs = &ms
		}
	}
	state := wechat.ReminderStatus()
	reminder := WeChatReminder{
		Started:    state.Started,
		TickMs:     state.TickMs,
		LastTickAt: state.LastTickAt,
		NextTickAt: state.NextTickAt,
	}
	var ownerContext *WeChatContext
	if account.Owner != "" {
		if entry, ok := wechat.GetContext(account.Owner); ok {
			ownerContext = &WeChatContext{LastMsgAt: entry.LastMsgAt, LastRemindedAt: entry.LastRemindedAt}
		}
	}
	return BuildWeChatStatus(nowMillis(), account, config.C().WeChatReminderHours, reminder, ownerContext)
}

// startPairing starts a pairing session and renders the QR data URL for the UI.
func startPairing(ctx context.Context) PairStartResult {
	start, err := wechat.StartPairing(ctx)
	if err != nil {
		return PairStartResult{State: "error", Error: err.Error()}
	}
	if start.State == wechat.PairError {
		msg := "pairing failed"
		if session := wechat.GetPairSession(); session != nil && session.Error != "" {
			msg = session.Error
		}
		return PairStartResult{State: "error", Error: msg}
	}
	image, err := wechat.RenderPairQR(start.Content)
	if err != nil {
		return PairStartResult{State: "error", Error: err.Error()}
	}
	return PairStartResult{State: string(start.State), QRImage: image}
}
