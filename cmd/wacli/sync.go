package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	appPkg "github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
)

func newSyncCmd(flags *rootFlags) *cobra.Command {
	var once bool
	var follow bool
	var forDuration time.Duration
	var warmSessions bool
	var warmGroup string
	var warmInterval time.Duration
	var idleExit time.Duration
	var maxReconnect time.Duration
	var staleThreshold time.Duration
	var downloadMedia bool
	var refreshContacts bool
	var refreshGroups bool
	var refreshChannels bool
	var webhookURL string
	var webhookSecret string
	var webhookAllowPrivate bool
	var storage syncStorageLimitFlags

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync messages (requires prior auth; never shows QR)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := flags.requireWritable(); err != nil {
				return err
			}
			storage.maxMessagesSet = cmd.Flags().Changed("max-messages")
			maxMessages, maxDBSize, err := resolveSyncStorageLimits(storage)
			if err != nil {
				return err
			}
			if webhookSecret != "" && webhookURL == "" {
				return fmt.Errorf("--webhook-secret requires --webhook")
			}
			if staleThreshold != 0 && staleThreshold < time.Second {
				return fmt.Errorf("--stale-threshold must be at least 1s, got %s", staleThreshold)
			}
			if maxStaleThreshold := appPkg.MaxStaleThreshold(); staleThreshold >= maxStaleThreshold {
				return fmt.Errorf("--stale-threshold must be less than %s because whatsmeow auto-reconnects after that much keepalive failure, got %s", maxStaleThreshold, staleThreshold)
			}
			ctx, stop := signalContextWithEvents(out.NewEventWriter(os.Stderr, flags.events))
			defer stop()

			a, lk, err := newApp(ctx, flags, true, false)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			if err := a.EnsureAuthed(); err != nil {
				return err
			}

			mode := appPkg.SyncModeFollow
			if once {
				mode = appPkg.SyncModeOnce
			} else if follow {
				mode = appPkg.SyncModeFollow
			} else {
				mode = appPkg.SyncModeOnce
			}

			// --for bounds a follow run to a fixed window then exits cleanly, so wacli can be a
			// guaranteed live recipient through the early-vote window without manual Ctrl+C.
			if forDuration > 0 {
				if mode != appPkg.SyncModeFollow {
					return fmt.Errorf("--for only applies in follow mode (drop --once)")
				}
				var cancelFor context.CancelFunc
				ctx, cancelFor = context.WithTimeout(ctx, forDuration)
				defer cancelFor()
			}

			var stopSendDelegate func()
			defer func() {
				if stopSendDelegate != nil {
					stopSendDelegate()
				}
			}()
			var afterConnect func(context.Context) error
			if mode == appPkg.SyncModeFollow {
				afterConnect = func(ctx context.Context) error {
					stop, err := startSendDelegateServer(ctx, a)
					if err != nil {
						return err
					}
					stopSendDelegate = stop
					return nil
				}
			}

			res, err := a.Sync(ctx, appPkg.SyncOptions{
				Mode:                mode,
				AllowQR:             false,
				AfterConnect:        afterConnect,
				DownloadMedia:       downloadMedia,
				RefreshContacts:     refreshContacts,
				RefreshGroups:       refreshGroups,
				RefreshChannels:     refreshChannels,
				WarmSessions:        warmSessions,
				WarmGroup:           warmGroup,
				WarmInterval:        warmInterval,
				IdleExit:            idleExit,
				MaxReconnect:        maxReconnect,
				StaleThreshold:      staleThreshold,
				MaxMessages:         maxMessages,
				MaxDBSizeBytes:      maxDBSize,
				WarnNoLimits:        true,
				WebhookURL:          webhookURL,
				WebhookSecret:       webhookSecret,
				WebhookAllowPrivate: webhookAllowPrivate,
			})
			if err != nil {
				// --for expiring is a clean, expected stop, not a failure. The window context can
				// fire during a setup phase (connect/migrate/AfterConnect) before the follow loop's
				// own ctx.Done handler turns it into a nil return, so normalise it here.
				if forDuration > 0 && errors.Is(err, context.DeadlineExceeded) {
					err = nil
				} else {
					return err
				}
			}

			if flags.asJSON {
				return out.WriteJSON(os.Stdout, map[string]any{
					"synced":          true,
					"messages_stored": res.MessagesStored,
				})
			}
			fmt.Fprintf(os.Stdout, "Messages stored: %d\n", res.MessagesStored)
			return nil
		},
	}

	cmd.Flags().BoolVar(&once, "once", false, "sync until idle and exit")
	cmd.Flags().BoolVar(&follow, "follow", true, "keep syncing until Ctrl+C")
	cmd.Flags().DurationVar(&forDuration, "for", 0, "in follow mode, stay connected for this long then exit cleanly (e.g. 5m; 0 = until Ctrl+C)")
	cmd.Flags().BoolVar(&warmSessions, "warm-sessions", false, "on connect, refresh group members' device lists via usync so recent realm migrations are recognised sooner")
	cmd.Flags().StringVar(&warmGroup, "warm-group", "", "restrict --warm-sessions to this group JID (default: all joined groups)")
	cmd.Flags().DurationVar(&warmInterval, "warm-interval", 0, "in follow mode, re-warm sessions every interval (e.g. 5m; min 1m; 0 = warm once on connect)")
	cmd.Flags().DurationVar(&idleExit, "idle-exit", 30*time.Second, "exit after being idle (once mode)")
	cmd.Flags().DurationVar(&maxReconnect, "max-reconnect", 5*time.Minute, "give up reconnecting after this duration (0 = unlimited)")
	cmd.Flags().DurationVar(&staleThreshold, "stale-threshold", 0, "force reconnect when keepalive failures last this long in follow mode (1s-<2m20s, 0 = disabled)")
	cmd.Flags().BoolVar(&downloadMedia, "download-media", false, "download media in the background during sync")
	cmd.Flags().BoolVar(&refreshContacts, "refresh-contacts", false, "refresh contacts from session store into local DB")
	cmd.Flags().BoolVar(&refreshGroups, "refresh-groups", false, "refresh joined groups (live) into local DB")
	cmd.Flags().BoolVar(&refreshChannels, "refresh-channels", false, "refresh subscribed channels (live) into local DB")
	cmd.Flags().StringVar(&webhookURL, "webhook", "", "URL to POST live message JSON")
	cmd.Flags().StringVar(&webhookSecret, "webhook-secret", "", "HMAC-SHA256 secret for X-Wacli-Signature header")
	cmd.Flags().BoolVar(&webhookAllowPrivate, "webhook-allow-private", false, "allow webhook URLs that resolve to localhost or private networks")
	cmd.Flags().Int64Var(&storage.maxMessages, "max-messages", 0, "maximum total messages to keep in the local DB before sync stops (0 = unlimited, or WACLI_SYNC_MAX_MESSAGES)")
	cmd.Flags().StringVar(&storage.maxDBSize, "max-db-size", "", "maximum wacli.db disk usage before sync stops, e.g. 500MB or 2GB (default: WACLI_SYNC_MAX_DB_SIZE or unlimited)")
	return cmd
}
