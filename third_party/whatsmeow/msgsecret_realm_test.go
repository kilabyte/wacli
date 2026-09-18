// Copyright (c) 2026 wacli contributors
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"context"
	"crypto/rand"
	"testing"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/gcmutil"
)

// These tests prove the PN<->LID realm fix for message-secret (poll vote) decryption without
// needing a live WhatsApp connection or a real database. The store dependencies that
// decryptMsgSecret touches (MsgSecrets and LIDs) are interfaces on store.Device, so we back them
// with tiny in-memory fakes and exercise the real decrypt path through DecryptPollVote.

// --- fakes ---------------------------------------------------------------------------------------

type fakeMsgSecrets struct {
	secret       []byte
	storedSender types.JID
}

func (f fakeMsgSecrets) PutMessageSecrets(context.Context, []store.MessageSecretInsert) error {
	return nil
}
func (f fakeMsgSecrets) PutMessageSecret(context.Context, types.JID, types.JID, types.MessageID, []byte) error {
	return nil
}
func (f fakeMsgSecrets) GetMessageSecret(context.Context, types.JID, types.JID, types.MessageID) ([]byte, types.JID, error) {
	return f.secret, f.storedSender, nil
}

// fakeLIDs maps a single PN<->LID pair, mirroring whatsmeow_lid_map.
type fakeLIDs struct {
	pnUser  string
	lidUser string
}

func (f fakeLIDs) PutManyLIDMappings(context.Context, []store.LIDMapping) error { return nil }
func (f fakeLIDs) PutLIDMapping(context.Context, types.JID, types.JID) error    { return nil }
func (f fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	if lid.Server == types.HiddenUserServer && lid.User == f.lidUser {
		return types.JID{User: f.pnUser, Server: types.DefaultUserServer}, nil
	}
	return types.JID{}, nil
}
func (f fakeLIDs) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	if pn.Server == types.DefaultUserServer && pn.User == f.pnUser {
		return types.JID{User: f.lidUser, Server: types.HiddenUserServer}, nil
	}
	return types.JID{}, nil
}
func (f fakeLIDs) GetManyLIDsForPNs(context.Context, []types.JID) (map[types.JID]types.JID, error) {
	return nil, nil
}

// --- helpers -------------------------------------------------------------------------------------

const (
	groupChat     = "16047202980-1398731215@g.us"
	pollID        = types.MessageID("3EB041F36EC957B84DC71E")
	voterPNUser   = "17788778980"    // Thomas Ciaccia (phone-number realm)
	voterLIDUser  = "52978572611695" // Thomas Ciaccia (LID realm)
	authorPNUser  = "16048058923"    // poll author (Dave) in PN realm
	authorLIDUser = "99887766554433" // poll author in LID realm
)

func jidPN(user string) types.JID  { return types.JID{User: user, Server: types.DefaultUserServer} }
func jidLID(user string) types.JID { return types.JID{User: user, Server: types.HiddenUserServer} }

// newClient builds a bare Client whose Store has the configured secret and LID mapping wired.
func newClient(secret []byte, storedSender types.JID, lids fakeLIDs) *Client {
	return &Client{Store: &store.Device{
		MsgSecrets: fakeMsgSecrets{secret: secret, storedSender: storedSender},
		LIDs:       lids,
	}}
}

// encryptVoteAs encrypts a poll vote exactly as a sender's client would, using the given voter and
// author JID strings to derive the key. This reproduces what arrives over the wire.
func encryptVoteAs(t *testing.T, voter, author types.JID, secret []byte, options []string) *waE2E.PollEncValue {
	t.Helper()
	plaintext, err := proto.Marshal(&waE2E.PollVoteMessage{SelectedOptions: HashPollOptions(options)})
	if err != nil {
		t.Fatalf("marshal vote: %v", err)
	}
	key, aad := generateMsgSecretKey(EncSecretPollVote, voter, pollID, author, secret)
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("iv: %v", err)
	}
	ciphertext, err := gcmutil.Encrypt(key, iv, plaintext, aad)
	if err != nil {
		t.Fatalf("encrypt vote: %v", err)
	}
	return &waE2E.PollEncValue{EncPayload: ciphertext, EncIV: iv}
}

// voteEvent builds the *events.Message we'd receive: eventSender is how WhatsApp addressed the
// voter to us; keyParticipant is the poll author as embedded in the poll-creation message key.
func voteEvent(eventSender, keyParticipant types.JID, enc *waE2E.PollEncValue) *events.Message {
	chat, _ := types.ParseJID(groupChat)
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: eventSender, IsGroup: true},
			ID:            pollID,
		},
		Message: &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{
			Vote: enc,
			PollCreationMessageKey: &waCommon.MessageKey{
				RemoteJID:   proto.String(groupChat),
				FromMe:      proto.Bool(false),
				ID:          proto.String(string(pollID)),
				Participant: proto.String(keyParticipant.String()),
			},
		}},
	}
}

// canonicalDecryptFails reports whether the unpatched, single-attempt derivation (key derived from
// the identities exactly as they appear in the event) fails to decrypt. This is the pre-fix path.
func canonicalDecryptFails(eventSender, keyParticipant types.JID, secret []byte, enc *waE2E.PollEncValue) bool {
	key, aad := generateMsgSecretKey(EncSecretPollVote, eventSender, pollID, keyParticipant, secret)
	_, err := gcmutil.Decrypt(key, enc.GetEncIV(), enc.GetEncPayload(), aad)
	return err != nil
}

func assertVote(t *testing.T, cli *Client, evt *events.Message, want []string) {
	t.Helper()
	got, err := cli.DecryptPollVote(context.Background(), evt)
	if err != nil {
		t.Fatalf("DecryptPollVote: %v", err)
	}
	wantHashes := HashPollOptions(want)
	if len(got.GetSelectedOptions()) != len(wantHashes) {
		t.Fatalf("got %d options, want %d", len(got.GetSelectedOptions()), len(wantHashes))
	}
	for i, h := range wantHashes {
		if string(got.GetSelectedOptions()[i]) != string(h) {
			t.Fatalf("option %d hash mismatch", i)
		}
	}
}

// --- tests ---------------------------------------------------------------------------------------

// TestDecryptPollVote_VoterLIDMismatch is the headline bug: the voter encrypted using their PN, but
// WhatsApp delivers the vote event addressed by their LID. The pre-fix canonical derivation fails;
// the fix recovers it by retrying the voter's alternate realm.
func TestDecryptPollVote_VoterLIDMismatch(t *testing.T) {
	secret := make([]byte, 32)
	rand.Read(secret)
	author := jidPN(authorPNUser)
	options := []string{"Ivory Coast"}

	// Voter's client derived the key with its PN identity.
	enc := encryptVoteAs(t, jidPN(voterPNUser), author, secret, options)
	// But the event arrives addressed by the voter's LID.
	evt := voteEvent(jidLID(voterLIDUser), author, enc)

	if !canonicalDecryptFails(jidLID(voterLIDUser), author, secret, enc) {
		t.Fatal("expected the pre-fix canonical derivation to FAIL for the LID-addressed voter")
	}

	cli := newClient(secret, author, fakeLIDs{pnUser: voterPNUser, lidUser: voterLIDUser})
	assertVote(t, cli, evt, options)
}

// TestDecryptPollVote_VoterPNMismatch is the mirror case: voter encrypted using LID, event arrives
// addressed by PN.
func TestDecryptPollVote_VoterPNMismatch(t *testing.T) {
	secret := make([]byte, 32)
	rand.Read(secret)
	author := jidPN(authorPNUser)
	options := []string{"Draw"}

	enc := encryptVoteAs(t, jidLID(voterLIDUser), author, secret, options)
	evt := voteEvent(jidPN(voterPNUser), author, enc)

	if !canonicalDecryptFails(jidPN(voterPNUser), author, secret, enc) {
		t.Fatal("expected the pre-fix canonical derivation to FAIL for the PN-addressed voter")
	}

	cli := newClient(secret, author, fakeLIDs{pnUser: voterPNUser, lidUser: voterLIDUser})
	assertVote(t, cli, evt, options)
}

// TestDecryptPollVote_AuthorRealmMismatch covers the original upstream scenario: the poll author in
// the message key is in a different realm than the one stored alongside the secret. The fix must
// still handle it (it subsumes the previous storedOrigSender retry).
func TestDecryptPollVote_AuthorRealmMismatch(t *testing.T) {
	secret := make([]byte, 32)
	rand.Read(secret)
	options := []string{"Ecuador"}

	// Voter (PN, matching realm) encrypted using the author's PN identity (== stored sender).
	enc := encryptVoteAs(t, jidPN(voterPNUser), jidPN(authorPNUser), secret, options)
	// But the poll-creation key embeds the author's LID, and that's what getOrigSenderFromKey returns.
	evt := voteEvent(jidPN(voterPNUser), jidLID(authorLIDUser), enc)

	// Stored sender is the PN realm; author<->LID resolvable via the map.
	cli := newClient(secret, jidPN(authorPNUser), fakeLIDs{pnUser: authorPNUser, lidUser: authorLIDUser})
	assertVote(t, cli, evt, options)
}

// TestDecryptPollVote_NoRegressionMatchedRealm proves the common, already-working cases (event
// realm == encryption realm) still decrypt on the fast path, for both PN and LID voters.
func TestDecryptPollVote_NoRegressionMatchedRealm(t *testing.T) {
	author := jidPN(authorPNUser)
	lids := fakeLIDs{pnUser: voterPNUser, lidUser: voterLIDUser}

	t.Run("PN voter", func(t *testing.T) {
		secret := make([]byte, 32)
		rand.Read(secret)
		options := []string{"Ivory Coast"}
		enc := encryptVoteAs(t, jidPN(voterPNUser), author, secret, options)
		evt := voteEvent(jidPN(voterPNUser), author, enc)
		if canonicalDecryptFails(jidPN(voterPNUser), author, secret, enc) {
			t.Fatal("matched-realm PN vote should decrypt on the fast path")
		}
		assertVote(t, newClient(secret, author, lids), evt, options)
	})

	t.Run("LID voter", func(t *testing.T) {
		secret := make([]byte, 32)
		rand.Read(secret)
		options := []string{"Draw"}
		enc := encryptVoteAs(t, jidLID(voterLIDUser), author, secret, options)
		evt := voteEvent(jidLID(voterLIDUser), author, enc)
		if canonicalDecryptFails(jidLID(voterLIDUser), author, secret, enc) {
			t.Fatal("matched-realm LID vote should decrypt on the fast path")
		}
		assertVote(t, newClient(secret, author, lids), evt, options)
	})
}

// TestDecryptPollVote_FromMeSelfVoteRealmMismatch covers the getOrigSenderFromKey fromMe path
// (a vote on a poll we created). getOrigSenderFromKey returns msg.Info.Sender as the author, so a
// realm flip on our own identity affects BOTH the author and the voter string. The retry must still
// recover it by trying our own PN<->LID alternate on both axes.
func TestDecryptPollVote_FromMeSelfVoteRealmMismatch(t *testing.T) {
	secret := make([]byte, 32)
	rand.Read(secret)
	options := []string{"Ivory Coast"}

	// We created the poll and voted on it using our PN identity (author == voter == own PN).
	enc := encryptVoteAs(t, jidPN(voterPNUser), jidPN(voterPNUser), secret, options)
	// The event is fromMe (poll-creation key FromMe=true) but addressed by our LID.
	chat, _ := types.ParseJID(groupChat)
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: jidLID(voterLIDUser), IsGroup: true, IsFromMe: true},
			ID:            pollID,
		},
		Message: &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{
			Vote: enc,
			PollCreationMessageKey: &waCommon.MessageKey{
				RemoteJID: proto.String(groupChat),
				FromMe:    proto.Bool(true),
				ID:        proto.String(string(pollID)),
			},
		}},
	}

	// Stored under our PN realm; own PN<->LID resolvable.
	cli := newClient(secret, jidPN(voterPNUser), fakeLIDs{pnUser: voterPNUser, lidUser: voterLIDUser})
	assertVote(t, cli, evt, options)
}

// TestDecryptPollVote_VoterAltFromEnvelopeNoLidMap is the v2 case: the lid_map has NO entry for the
// voter (the residual June-15 failure mode), but the event envelope carries the voter's alternate
// realm in SenderAlt. The retry must use SenderAlt directly so the vote still decrypts.
func TestDecryptPollVote_VoterAltFromEnvelopeNoLidMap(t *testing.T) {
	secret := make([]byte, 32)
	rand.Read(secret)
	author := jidPN(authorPNUser)
	options := []string{"Ivory Coast"}

	// Voter encrypted with their PN; event arrives addressed by LID with SenderAlt = the PN.
	enc := encryptVoteAs(t, jidPN(voterPNUser), author, secret, options)
	chat, _ := types.ParseJID(groupChat)
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:           chat,
				Sender:         jidLID(voterLIDUser),
				SenderAlt:      jidPN(voterPNUser), // <-- the realm we need, straight from the envelope
				AddressingMode: types.AddressingModeLID,
				IsGroup:        true,
			},
			ID: pollID,
		},
		Message: &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{
			Vote: enc,
			PollCreationMessageKey: &waCommon.MessageKey{
				RemoteJID:   proto.String(groupChat),
				FromMe:      proto.Bool(false),
				ID:          proto.String(string(pollID)),
				Participant: proto.String(author.String()),
			},
		}},
	}

	// EMPTY lid_map: GetAltJID can't help; only SenderAlt can.
	assertVote(t, newClient(secret, author, fakeLIDs{}), evt, options)
}

// TestDecryptPollVote_UnknownVoterStaysFailed proves we don't silently mis-decrypt: if there's no
// LID mapping for the voter, no SenderAlt, and the realm genuinely mismatches, decryption still
// fails (no false positive from the GCM tag).
func TestDecryptPollVote_UnknownVoterStaysFailed(t *testing.T) {
	secret := make([]byte, 32)
	rand.Read(secret)
	author := jidPN(authorPNUser)
	enc := encryptVoteAs(t, jidPN(voterPNUser), author, secret, []string{"Ivory Coast"})
	evt := voteEvent(jidLID(voterLIDUser), author, enc) // no SenderAlt set

	// Empty LID map: no PN<->LID resolution available.
	cli := newClient(secret, author, fakeLIDs{})
	if _, err := cli.DecryptPollVote(context.Background(), evt); err == nil {
		t.Fatal("expected decryption to fail when the voter realm can't be resolved")
	}
}
