# Vendored, patched copy of whatsmeow

- Upstream: https://github.com/tulir/whatsmeow
- Pinned commit: 2e338d0ee73d7044252a5224e628de607d7f9cdd ("proto: update to v1047769893", 2026-09-17)
  (matches the module pseudo-version go.mau.fi/whatsmeow v0.0.0-20260917111002-2e338d0ee73d)
- Requires Go >= 1.26.0 (upstream floor; the build image is golang:1.26).
- Local patch: ../../patches/0001-whatsmeow-poll-vote-lid-realm.patch
- Reason: fixes poll-vote (message-secret) decryption dropping votes from voters
  addressed in a different realm (PN @s.whatsapp.net vs LID @lid) than the one used
  to encrypt the vote. See patches/README.md and CHANGES below.

## Refresh history
- 2026-09-18 (v8): refreshed from the May pin 6dd3d24c1ca6 to 2e338d0ee73d.
  WhatsApp had begun rejecting the protocol version carried by the May pin, so the
  v7 daemon could no longer hold a connection ("30 consecutive reconnects failed to
  hold 1m0s"). The new pin's own commit is a protobuf/protocol bump, which is the
  fix. Verified after the refresh: `sync --once` reports `Connected.` and exits 0,
  and `sync --follow --for 75s` holds the full window with zero reconnects.
- 2026-05-05 (v1..v7): original pin 6dd3d24c1ca6.

## Upstream status of the patch
Still NOT fixed upstream as of 2e338d0ee73d. Upstream `decryptMsgSecret` retries only
`origSender` vs `storedOrigSender` (the poll author's two realms) and never varies the
modification sender's realm (the voter's), so votes from a voter addressed in the other
realm are still dropped. Re-check this on every refresh before re-porting.

## Changes vs the pinned commit
- msgsecret.go: decryptMsgSecret now retries key derivation across the PN<->LID
  alternate realms of BOTH the original message sender and the modification sender
  (e.g. the poll voter), via the new altRealmJIDs helper using Store.LIDs/GetAltJID.
  The fast path returns early on success; the cross-product retry runs only on
  "message authentication failed".
- msgsecret_realm_test.go: deterministic proof tests (no DB / no network).

## Port friction seen on the 2026-09-18 refresh (expect these again)
Two wacli call sites broke on upstream API changes and were ported:
- internal/wa/client.go: SetStatusMessage now takes types.SetStatusInput{Text: *string}
  instead of a bare string.
- internal/wa/media.go: DownloadMediaWithPathToFile dropped its fileLength parameter and
  gained a trailing allowNoHash bool.

To re-create this tree from scratch:
    git clone https://github.com/tulir/whatsmeow third_party/whatsmeow
    cd third_party/whatsmeow && git checkout 2e338d0ee73d
    git apply ../../patches/0001-whatsmeow-poll-vote-lid-realm.patch
    rm -rf .git
