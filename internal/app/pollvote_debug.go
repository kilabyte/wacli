package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// debugPollVote enables verbose poll-vote receipt/decrypt logging when WACLI_DEBUG_POLLVOTE=1.
// It is read once at startup. The goal of this logging is to definitively separate two failure
// modes for a missing poll vote:
//   - "never arrived" (a delivery/presence gap): there is NO pollvote_received line for the voter.
//   - "arrived but failed" (a decrypt gap): there IS a pollvote_received line followed by a
//     pollvote_outcome line with outcome=failed.
var debugPollVote = os.Getenv("WACLI_DEBUG_POLLVOTE") == "1"

// logPollVoteReceipt records a poll vote the moment it reaches this device, before decryption.
func (a *App) logPollVoteReceipt(chatJID, pollMsgID string, evt *events.Message) {
	if !debugPollVote || evt == nil {
		return
	}
	a.pollVoteDebugLine(map[string]any{
		"event":       "pollvote_received",
		"poll_msg_id": pollMsgID,
		"chat_jid":    chatJID,
		"vote_msg_id": evt.Info.ID,
		"voter":       evt.Info.Sender.String(),
		"voter_alt":   altJIDString(evt.Info.SenderAlt),
		"addressing":  string(evt.Info.AddressingMode),
		"from_me":     evt.Info.IsFromMe,
		"sender_ts":   evt.Info.Timestamp.UTC().Format(time.RFC3339),
	})
}

// logPollVoteOutcome records the result of decrypting a received poll vote.
func (a *App) logPollVoteOutcome(chatJID, pollMsgID, voterJID string, evt *events.Message, decErr error) {
	if !debugPollVote || evt == nil {
		return
	}
	rec := map[string]any{
		"event":       "pollvote_outcome",
		"poll_msg_id": pollMsgID,
		"chat_jid":    chatJID,
		"voter":       evt.Info.Sender.String(),
		"voter_alt":   altJIDString(evt.Info.SenderAlt),
		"addressing":  string(evt.Info.AddressingMode),
	}
	if decErr == nil {
		rec["outcome"] = "ok"
		rec["voter_canonical"] = voterJID
	} else {
		rec["outcome"] = "failed"
		rec["reason"] = classifyDecryptFailure(decErr)
		rec["error"] = decErr.Error()
	}
	a.pollVoteDebugLine(rec)
}

// classifyDecryptFailure buckets a decrypt error into a stable, greppable reason code.
func classifyDecryptFailure(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "message authentication failed"):
		return "mac_mismatch_all_realms"
	case strings.Contains(msg, "original message secret key not found"):
		return "message_secret_not_found"
	case strings.Contains(msg, "isn't a poll update message"):
		return "not_poll_update"
	default:
		return "other"
	}
}

func altJIDString(j types.JID) string {
	if j.IsEmpty() {
		return ""
	}
	return j.String()
}

// pollVoteDebugLine emits one structured record. When NDJSON events are enabled it rides the normal
// event stream; otherwise it writes a greppable line to stderr so it's visible during plain syncs.
func (a *App) pollVoteDebugLine(rec map[string]any) {
	if a != nil && a.eventsEnabled() {
		a.emitEvent("pollvote_debug", rec)
		return
	}
	if b, err := json.Marshal(rec); err == nil {
		fmt.Fprintln(os.Stderr, "WACLI_DEBUG_POLLVOTE "+string(b))
	}
}
