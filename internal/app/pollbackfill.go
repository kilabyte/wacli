package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// PollBackfillOptions configures `poll backfill`.
type PollBackfillOptions struct {
	PollMsgID      string
	Count          int           // messages per on-demand request
	MaxRequests    int           // how many on-demand requests to walk back through
	WaitPerRequest time.Duration // per-request response timeout
}

// PollBackfillResult reports what a backfill recovered.
type PollBackfillResult struct {
	PollMsgID       string
	ChatJID         string
	Question        string
	VotesBefore     int
	VotesAfter      int
	Recovered       int
	RecoveredVoters []string
	RequestsSent    int
	ResponsesSeen   int
	ReachedPoll     bool
}

type onDemandPollResp struct {
	messages   int
	hasOldest  bool
	oldestTS   time.Time
	oldestInfo types.MessageInfo
	endType    waHistorySync.Conversation_EndOfHistoryTransferType
}

// BackfillPoll attempts to recover poll votes that never arrived live by pulling the chat's history
// from the primary device (on-demand history sync) and re-running the poll-vote pipeline over it.
//
// On-demand history fetches the N messages BEFORE a reference message (older), so we anchor at the
// chat's newest local message and walk backward until the returned window reaches the poll's
// creation time (where its votes live) or we run out of requests/history. Each response is processed
// through handleHistorySync, which decrypts and stores any poll votes in it (filling gaps).
//
// Feasibility caveat: this can only recover votes the PRIMARY device still has and includes in the
// on-demand response; that must be confirmed against live data (see patches/README.md).
func (a *App) BackfillPoll(ctx context.Context, opts PollBackfillOptions) (PollBackfillResult, error) {
	pollMsgID := strings.TrimSpace(opts.PollMsgID)
	if pollMsgID == "" {
		return PollBackfillResult{}, fmt.Errorf("--id is required")
	}
	if opts.Count <= 0 {
		opts.Count = DefaultBackfillCount
	}
	if opts.Count > MaxBackfillCount {
		opts.Count = MaxBackfillCount
	}
	if opts.MaxRequests <= 0 {
		opts.MaxRequests = 10
	}
	if opts.MaxRequests > MaxBackfillRequests {
		opts.MaxRequests = MaxBackfillRequests
	}
	if opts.WaitPerRequest <= 0 {
		opts.WaitPerRequest = 60 * time.Second
	}

	if err := a.EnsureAuthed(); err != nil {
		return PollBackfillResult{}, err
	}

	poll, err := a.db.FindPollByMsgID(pollMsgID)
	if err != nil {
		return PollBackfillResult{}, fmt.Errorf("poll %s not found locally; run `wacli sync` so the poll creation is stored first: %w", pollMsgID, err)
	}
	chatStr := poll.ChatJID
	chat, err := types.ParseJID(chatStr)
	if err != nil {
		return PollBackfillResult{}, fmt.Errorf("parse poll chat JID %q: %w", chatStr, err)
	}

	votesBefore, _ := a.db.ListPollVotes(chatStr, pollMsgID)
	beforeSet := make(map[string]struct{}, len(votesBefore))
	for _, v := range votesBefore {
		beforeSet[v.VoterJID] = struct{}{}
	}

	if err := a.OpenWA(); err != nil {
		return PollBackfillResult{}, err
	}
	a.wa.SetManualHistorySyncDownload(true)
	defer a.wa.SetManualHistorySyncDownload(false)

	var mu sync.Mutex
	var waitCh chan onDemandPollResp
	var manualStored, manualLast atomic.Int64
	manualLast.Store(nowUTC().UnixNano())

	deliver := func(resp onDemandPollResp) {
		mu.Lock()
		ch := waitCh
		mu.Unlock()
		if ch == nil {
			return
		}
		select {
		case ch <- resp:
		default:
		}
	}

	extract := func(hs *events.HistorySync) (onDemandPollResp, bool) {
		if hs == nil || hs.Data == nil || hs.Data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
			return onDemandPollResp{}, false
		}
		for _, conv := range hs.Data.GetConversations() {
			if strings.TrimSpace(conv.GetID()) != chatStr {
				continue
			}
			resp := onDemandPollResp{messages: len(conv.GetMessages()), endType: conv.GetEndOfHistoryTransferType()}
			for _, m := range conv.GetMessages() {
				wm := m.GetMessage()
				if wm == nil || wm.GetKey() == nil {
					continue
				}
				ts := time.Unix(int64(wm.GetMessageTimestamp()), 0).UTC()
				if !resp.hasOldest || ts.Before(resp.oldestTS) {
					resp.hasOldest = true
					resp.oldestTS = ts
					resp.oldestInfo = types.MessageInfo{
						MessageSource: types.MessageSource{Chat: chat, IsFromMe: wm.GetKey().GetFromMe()},
						ID:            types.MessageID(wm.GetKey().GetID()),
						Timestamp:     ts,
					}
				}
			}
			return resp, true
		}
		return onDemandPollResp{}, false
	}

	handlerID := a.wa.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.HistorySync:
			if resp, ok := extract(v); ok {
				deliver(resp)
			}
		case *events.Message:
			notif := historySyncNotificationFromMessage(v)
			if notif == nil || notif.GetSyncType() != waE2E.HistorySyncType_ON_DEMAND {
				return
			}
			data, err := a.wa.DownloadHistorySync(ctx, notif)
			if err != nil {
				a.emitWarning("on_demand_history_download_failed",
					fmt.Sprintf("warning: failed to download on-demand history sync: %v", err),
					map[string]any{"error": err.Error()})
				return
			}
			if data.GetSyncType() != waHistorySync.HistorySync_ON_DEMAND {
				return
			}
			hs := &events.HistorySync{Data: data}
			// Processes + stores any poll votes in the window (the actual backfill).
			a.handleHistorySync(ctx, SyncOptions{}, hs, &manualStored, &manualLast, func(string, string) {})
			if resp, ok := extract(hs); ok {
				deliver(resp)
			}
		}
	})
	defer a.wa.RemoveEventHandler(handlerID)

	result := PollBackfillResult{PollMsgID: pollMsgID, ChatJID: chatStr, Question: poll.Question, VotesBefore: len(votesBefore)}

	_, err = a.Sync(ctx, SyncOptions{
		Mode:     SyncModeOnce,
		AllowQR:  false,
		IdleExit: 5 * time.Second,
		AfterConnect: func(ctx context.Context) error {
			latest, err := a.db.GetLatestMessageInfo(chatStr)
			if err != nil {
				return fmt.Errorf("no messages for %s in local DB; run `wacli sync` first", chatStr)
			}
			ref := types.MessageInfo{
				MessageSource: types.MessageSource{Chat: chat, IsFromMe: latest.FromMe},
				ID:            types.MessageID(latest.MsgID),
				Timestamp:     latest.Timestamp,
			}
			for i := 0; i < opts.MaxRequests; i++ {
				ch := make(chan onDemandPollResp, 4)
				mu.Lock()
				waitCh = ch
				mu.Unlock()

				result.RequestsSent++
				a.emitOrPrint("pollbackfill_requesting",
					map[string]any{"poll_msg_id": pollMsgID, "chat_jid": chatStr, "count": opts.Count, "request": result.RequestsSent},
					"Requesting history for poll %s (request %d, %d msgs)...\n", pollMsgID, result.RequestsSent, opts.Count)
				if _, err := a.wa.RequestHistorySyncOnDemand(ctx, ref, opts.Count); err != nil {
					return err
				}

				var resp onDemandPollResp
				select {
				case <-ctx.Done():
					return ctx.Err()
				case resp = <-ch:
					result.ResponsesSeen++
				case <-time.After(opts.WaitPerRequest):
					return fmt.Errorf("timed out waiting for on-demand history sync response")
				}
				mu.Lock()
				if waitCh == ch {
					waitCh = nil
				}
				mu.Unlock()

				a.emitOrPrint("pollbackfill_response",
					map[string]any{"poll_msg_id": pollMsgID, "messages": resp.messages, "responses_seen": result.ResponsesSeen},
					"On-demand response: %d messages.\n", resp.messages)

				if resp.hasOldest && !resp.oldestTS.After(poll.CreatedAt) {
					result.ReachedPoll = true // walked back far enough to cover the poll's vote window
					break
				}
				if resp.endType == waHistorySync.Conversation_COMPLETE_AND_NO_MORE_MESSAGE_REMAIN_ON_PRIMARY {
					break
				}
				if !resp.hasOldest || resp.messages <= 0 || resp.oldestInfo.ID == ref.ID {
					break // no progress
				}
				ref = resp.oldestInfo // walk further back
			}
			return nil
		},
	})
	if err != nil {
		return result, err
	}

	votesAfter, _ := a.db.ListPollVotes(chatStr, pollMsgID)
	result.VotesAfter = len(votesAfter)
	for _, v := range votesAfter {
		if _, had := beforeSet[v.VoterJID]; !had {
			result.RecoveredVoters = append(result.RecoveredVoters, v.VoterJID)
		}
	}
	result.Recovered = len(result.RecoveredVoters)
	return result, nil
}
