# Vendored, patched copy of whatsmeow

- Upstream: https://github.com/tulir/whatsmeow
- Pinned commit: 6dd3d24c1ca68f5194de26c73b2961b06ffbcab2
  (matches the module pseudo-version go.mau.fi/whatsmeow v0.0.0-20260505142014-6dd3d24c1ca6)
- Local patch: ../../patches/0001-whatsmeow-poll-vote-lid-realm.patch
- Reason: fixes poll-vote (message-secret) decryption dropping votes from voters
  addressed in a different realm (PN @s.whatsapp.net vs LID @lid) than the one used
  to encrypt the vote. See patches/0001-... and CHANGES below.

## Changes vs the pinned commit
- msgsecret.go: decryptMsgSecret now retries key derivation across the PN<->LID
  alternate realms of BOTH the original message sender and the modification sender
  (e.g. the poll voter), via the new altRealmJIDs helper using Device.GetAltJID.
- msgsecret_realm_test.go: deterministic proof tests (no DB / no network).

To re-create this tree from scratch:
    git clone https://github.com/tulir/whatsmeow third_party/whatsmeow
    cd third_party/whatsmeow && git checkout 6dd3d24c1ca6
    git apply ../../patches/0001-whatsmeow-poll-vote-lid-realm.patch
    rm -rf .git
