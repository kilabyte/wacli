# whatsmeow poll-vote PN↔LID decryption fix

## Symptom

`wacli poll show` silently dropped a large fraction of WhatsApp **group** poll votes. The votes
arrived (they are stored as `text='Poll vote'` rows in `wacli.db.messages`), but whatsmeow failed to
**decrypt** some of them into the `poll_votes` table, so those voters were omitted entirely. The
failure was sporadic **per (poll, voter)**: the same member decrypted on one poll and failed on
another.

## Root cause

Poll votes are encrypted with an AEAD key derived (HKDF-SHA256) from:

- the poll's stored 32-byte message secret, and
- a "use-case secret" = `pollMsgID + origMsgSenderStr + modificationSenderStr + "Poll Vote"`,

where `origMsgSenderStr` is the poll **author**'s JID and `modificationSenderStr` is the **voter**'s
JID, both rendered with `.ToNonAD().String()`. The AEAD additional data is
`pollMsgID + "\x00" + modificationSenderStr` (voter again). Decryption only succeeds if **both**
JID strings match exactly what the encrypting client used.

While WhatsApp migrates user identities from phone-number JIDs (`@s.whatsapp.net`, "PN") to LID JIDs
(`@lid`), the realm of an identity in the vote event we receive can differ from the realm the
voter's client used to derive the key. whatsmeow's `decryptMsgSecret`:

- (pinned snapshot `6dd3d24c1ca6`, 2026-05-05) had a "double-try" that was a **no-op** — both the
  first attempt and the retry passed `origSender` as the author (see line 118 of the original
  `msgsecret.go`);
- (upstream `main`) fixed that retry to use `storedOrigSender`, so it now tries both author realms,
  **but still never converts the voter (`modificationSender` / `msg.Info.Sender`) realm.**
  `getOrigSenderFromKey` even carries a TODO acknowledging the LID gap.

So the **voter realm mismatch was never handled** — that is the dropped-vote bug. A whatsmeow
version bump does **not** fix it.

The data needed to fix it is already present in `session.db`: `whatsmeow_message_secrets` and
`whatsmeow_lid_map` (the PN↔LID mapping, reachable via `Device.GetAltJID` /
`cli.Store.LIDs.Get{PN,LID}For{LID,PN}`).

## The fix

`patches/0001-whatsmeow-poll-vote-lid-realm.patch` (applied to the vendored
`third_party/whatsmeow`):

- Keep the canonical first attempt unchanged (fast path for the common case).
- On `message authentication failed`, retry key derivation across the **PN↔LID alternate realms of
  both identities** — the original sender from the message key, the sender stored alongside the
  secret, and the modification sender (the voter) — stopping at the first GCM-authenticating
  combination. Realm conversion uses whatsmeow's existing `Device.GetAltJID` (backed by
  `whatsmeow_lid_map`).
- A new `altRealmJIDs` helper builds the de-duplicated candidate list (non-AD normalised, empties
  dropped, original realm first, nil-store safe).

This subsumes upstream's `storedOrigSender` author retry and additionally covers the voter realm.
It is safe to over-try: AES-GCM tag verification makes a wrong key/AAD **fail** rather than yield
incorrect plaintext, so a wrong candidate cannot produce a false-positive decrypt.

### Why it's proven

`third_party/whatsmeow/msgsecret_realm_test.go` is a deterministic, offline white-box test (fake
`MsgSecretStore` + `LIDStore`, no DB, no network) that:

- encrypts a poll vote using the voter's **PN** identity, then delivers the event addressed by the
  voter's **LID** (and the mirror case) — the exact live failure;
- asserts the **pre-fix** single-attempt derivation **fails** with `message authentication failed`;
- asserts the **patched** `DecryptPollVote` **succeeds** and returns the correct option;
- regression: matched-realm PN and LID votes still decrypt on the fast path;
- author-realm mismatch (the original upstream case) still works;
- negative control: with no LID mapping, decryption still fails (no false positive).

Verified necessary by reverting `msgsecret.go` to the pinned original: the mismatch tests fail;
restoring the patch makes them pass.

## Build

Native (host) for development/tests:

```sh
pnpm test           # full gate incl. third_party realm test
pnpm build          # dist/wacli for the host
```

Deploy binary for the gateway (linux/amd64, glibc / Debian — matches the container):

```sh
scripts/build-linux-amd64.sh        # -> dist/wacli-linux-amd64
```

The whole repo (including `third_party/whatsmeow`) is the build context, so the
`replace go.mau.fi/whatsmeow => ./third_party/whatsmeow` directive resolves with no extra clone.

## Deploy to the gateway (reversible, non-destructive)

Do **not** overwrite the Homebrew binary (a `brew upgrade` would revert it and risk the live pool).
Drop the patched binary into the bind-mounted user bin and call it by full path first:

```sh
# copy dist/wacli-linux-amd64 to the host bind-mount, then:
docker exec -u node naomi-gateway sh -c 'mkdir -p /home/node/.local/bin && \
  cp /path/in/container/wacli-linux-amd64 /home/node/.local/bin/wacli && \
  chmod +x /home/node/.local/bin/wacli'

# verify it uses the SAME store, no re-auth:
docker exec -u node naomi-gateway sh -c '/home/node/.local/bin/wacli poll show ... '
```

Only put `/home/node/.local/bin` ahead of linuxbrew on the wacli callers' PATH once verified. The
brew binary stays as the fallback.

## Live verification (must be done on the gateway — see Constraints)

Past dropped votes are **unrecoverable** (the encrypted payload was discarded; only `'Poll vote'`
placeholders remain), so you cannot verify against old polls. Verify on **new** votes:

1. Run the patched binary's `sync` so it is connected when new votes are cast (a fresh test poll, or
   the next day's pool polls). Never run two WhatsApp connections at once.
2. `poll show` and confirm previously-dropping members now appear with the **correct** picks,
   cross-checked against Dave's "View votes" screenshot / the Back4App `WCPick` oracle.
3. Regression: every vote the unpatched wacli decrypted correctly must still decrypt correctly.

## Provenance

See `third_party/whatsmeow/VENDOR_INFO.md`. Pinned upstream commit `6dd3d24c1ca6`
(= `go.mau.fi/whatsmeow v0.0.0-20260505142014-6dd3d24c1ca6`).

---

## Draft upstream PR (to tulir/whatsmeow)

> **Title:** msgsecret: retry poll/reaction decryption across PN↔LID realms for the modification sender
>
> **Summary:** The message-secret key and AEAD additional data are derived from the JID strings of
> the original-message sender and the modification sender. While WhatsApp migrates to LIDs, the realm
> (`@s.whatsapp.net` vs `@lid`) of either identity in a received event can differ from the realm the
> encrypting client used. `decryptMsgSecret` already retries the original sender (new-event sender vs
> the sender stored with the secret), but never converts the **modification sender** (e.g. the poll
> voter) — so votes/reactions from members addressed in the other realm fail to decrypt and are
> dropped. (Addresses the `getOrigSenderFromKey` TODO for the LID case.)
>
> **Change:** keep the canonical first attempt; on `message authentication failed`, retry key
> derivation across the PN↔LID alternates of both identities (original sender from the key, stored
> sender, and modification sender) via `Device.GetAltJID`, returning on the first GCM-authenticating
> combination. AES-GCM authentication guarantees no false-positive decrypt, so the extra attempts are
> safe and only run on the failure path.
>
> **Tests:** white-box `msgsecret_realm_test.go` with fake `MsgSecretStore`/`LIDStore` proving a
> PN-encrypted vote delivered under the voter's LID (and mirror) decrypts after the change and fails
> before it, plus matched-realm and no-mapping (negative) controls.

> Note for upstreaming: adjust the test file's copyright header, and consider whether to also drop
> the now-redundant `storedOrigSender`-only retry in favour of the generalised loop (this patch keeps
> behaviour a superset of it).

---

## v2 / v3 (`0.11.1-pollvote-lid-v3`)

> The build marker is `0.11.1-pollvote-lid-v3` (latest). The changes below are unchanged since the
> v2 build; the marker was bumped so `wacli --version` unambiguously identifies the newest binary.

### What v1 left open

Live diagnosis on a real slate showed the residual drops were a **delivery/presence gap, not a
decryption gap**. v1's decrypt fix works 100% on votes that *arrive*. The dropped votes (fast voters,
within ~60-90s of a poll posting) never reached the wacli linked device at all (no `Poll vote`
placeholder row, never queued), because wacli posts-then-disconnects and the senders' clients had not
yet routed to wacli's freshly server-migrated LID device.

### v2 changes vs v1

1. **Decrypt hardening (defense in depth, vendored whatsmeow):** the realm retry now also includes
   `msg.Info.SenderAlt` (the voter's alternate-realm JID carried in the event envelope), so the
   voter's other realm is available even when `whatsmeow_lid_map` has no mapping yet. Still a full
   author x voter cross-product, still GCM-gated (no false positives). New test
   `TestDecryptPollVote_VoterAltFromEnvelopeNoLidMap` proves the empty-`lid_map` case.
2. **`sync --follow` fix:** the send-delegate `.send.sock` `chmod` is now best-effort (warn, not
   fatal) so follow runs on bind-mounted filesystems (macOS Docker virtiofs returns `EINVAL`). This
   unblocks staying connected through the voting window.
3. **`sync --follow --for <duration>`:** hold the live connection for a fixed window then exit
   cleanly. Run it spanning poll time so wacli is a guaranteed live recipient during early voting.
4. **`sync --warm-sessions` (opt-in, off by default):** on connect, usync-refresh group members'
   device lists / PN<->LID mappings so recent realm migrations are recognised sooner. `--warm-group
   <jid>` restricts to one group. Sends nothing user-visible. Effect on *sender-side* routing is
   best-effort/unverified; staying connected (`--for`) is the stronger lever.

### Verbose logging: `WACLI_DEBUG_POLLVOTE=1`

Set the env var, then `sync`. Two record types are emitted (NDJSON via the event stream when run with
`--events`, otherwise greppable `WACLI_DEBUG_POLLVOTE {json}` lines on stderr):

- **Receipt (every vote that reaches us, before decrypt):**
  ```json
  {"event":"pollvote_received","poll_msg_id":"…","chat_jid":"…@g.us","vote_msg_id":"…",
   "voter":"…@lid","voter_alt":"…@s.whatsapp.net","addressing":"lid","from_me":false,
   "sender_ts":"2026-06-15T14:03:12Z"}
  ```
- **Outcome (decrypt result):**
  ```json
  {"event":"pollvote_outcome","poll_msg_id":"…","voter":"…@lid","voter_alt":"…@s.whatsapp.net",
   "addressing":"lid","outcome":"ok","voter_canonical":"…@s.whatsapp.net"}
  ```
  On failure: `"outcome":"failed","reason":"…","error":"…"` where `reason` is one of
  `mac_mismatch_all_realms` | `message_secret_not_found` | `not_poll_update` | `other`.

**Reading it next slate:** a ground-truth voter with **no `pollvote_received`** line = delivery gap
(never queued for us). One **with** a receipt but a **failed** outcome = decrypt gap. This is the
signal that separates the two for good.

### Suggested live test

```sh
# span the poll's early-vote window as a live recipient, with logging on:
WACLI_DEBUG_POLLVOTE=1 /home/node/.local/bin/wacli sync --follow --for 10m --warm-sessions 2>pollvote.log
# (start it just before the poll posts)
```
Then `poll show`, cross-check against the group's screenshots, and send `pollvote.log`.
