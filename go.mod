module github.com/openclaw/wacli

go 1.26.0

require (
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/mdp/qrterminal/v3 v3.2.1
	github.com/spf13/cobra v1.10.2
	go.mau.fi/whatsmeow v0.0.0-20260917111002-2e338d0ee73d
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
)

// Use the vendored, patched copy of whatsmeow (pinned commit 2e338d0ee73d plus the
// PN<->LID poll-vote decryption fix). See third_party/whatsmeow/VENDOR_INFO.md and
// patches/0001-whatsmeow-poll-vote-lid-realm.patch. Remove this once the fix lands upstream.
// Refreshed 2026-09-18 (v8): the May pin used a protocol version WhatsApp now rejects.
replace go.mau.fi/whatsmeow => ./third_party/whatsmeow

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/beeper/argo-go v1.1.2 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/elliotchance/orderedmap/v3 v3.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/kr/pretty v0.1.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/petermattis/goid v0.0.0-20260820044319-269ab09b5261 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/vektah/gqlparser/v2 v2.5.33 // indirect
	go.mau.fi/libsignal v0.2.2 // indirect
	go.mau.fi/util v0.10.1 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	gopkg.in/check.v1 v1.0.0-20190902080502-41f04d3bba15 // indirect
	rsc.io/qr v0.2.0 // indirect
)
