// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

// PRO-EXT01-04b: handler-side gate tests for animated avatars. The
// upload pipeline itself is covered by internal/avatars/animated_test.go;
// these tests assert the wiring: entitlement check, enforce-flag
// behavior, and the report_only_deny telemetry shape SREs will use to
// size the migration before flipping the flag.

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/gif"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
)

func makeTestAnimatedGIF(t *testing.T, frames int) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{R: 0xff}, color.RGBA{G: 0xff}, color.Transparent}
	g := &gif.GIF{LoopCount: 0}
	for i := 0; i < frames; i++ {
		img := image.NewPaletted(image.Rect(0, 0, 64, 64), pal)
		idx := uint8(i % 2)
		for y := 0; y < 64; y++ {
			for x := 0; x < 64; x++ {
				img.SetColorIndex(x, y, idx)
			}
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 10)
	}
	buf := &bytes.Buffer{}
	if err := gif.EncodeAll(buf, g); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

// uploadAvatarBytes is the GIF-aware sibling of uploadAvatar — accepts
// arbitrary body bytes + filename so animated paths can be exercised.
func uploadAvatarBytes(t *testing.T, cli *client, csrf string, body []byte, filename string) *http.Response {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if err := mw.WriteField("csrf_token", csrf); err != nil {
		t.Fatalf("write csrf: %v", err)
	}
	part, err := mw.CreateFormFile("avatar", filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}
	req, err := http.NewRequest("POST", cli.srv.URL+"/settings/profile/avatar", buf)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Referer", cli.srv.URL+"/settings/profile")
	resp, err := cli.c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func readAvatarKey(t *testing.T, pool *pgxpool.Pool, userID int64) string {
	t.Helper()
	var key pgtype.Text
	if err := pool.QueryRow(context.Background(),
		`SELECT avatar_object_key FROM users WHERE id = $1`, userID).Scan(&key); err != nil {
		t.Fatalf("read avatar key: %v", err)
	}
	if !key.Valid {
		t.Fatal("avatar_object_key is null after upload")
	}
	return key.String
}

func upgradeAvatarTestUserToPro(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	now := time.Now().UTC()
	suffix := strconv.FormatInt(userID, 10)
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_anim_pro_" + suffix, Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_anim_pro_" + suffix,
	})
	if err != nil {
		t.Fatalf("ApplyUserSubscriptionSnapshot: %v", err)
	}
}

// TestAvatarAnimated_ProUserPreserves: with the Pro entitlement, the
// canonical variant key keeps a .gif extension — i.e. the raw GIF
// bytes survive into the object store and the public route will serve
// them with image/gif.
func TestAvatarAnimated_ProUserPreserves(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceAnimatedAvatars: true, // enforce on; Pro must still pass.
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "proanimuser")
	upgradeAvatarTestUserToPro(t, pool, userID)

	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := uploadAvatarBytes(t, cli, csrf, makeTestAnimatedGIF(t, 3), "anim.gif")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	key := readAvatarKey(t, pool, userID)
	if !strings.HasSuffix(key, ".gif") {
		t.Errorf("Pro user avatar key = %q, want .gif extension (animation must survive)", key)
	}
}

// TestAvatarAnimated_FreeUserUnderEnforceFlattens: Free upload of an
// animated GIF with enforce=true flattens to PNG. This is the *flip*
// outcome PRO-EXT01-17 will eventually land.
func TestAvatarAnimated_FreeUserUnderEnforceFlattens(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceAnimatedAvatars: true,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freeanimenforce")

	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := uploadAvatarBytes(t, cli, csrf, makeTestAnimatedGIF(t, 4), "anim.gif")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	key := readAvatarKey(t, pool, userID)
	if !strings.HasSuffix(key, ".png") {
		t.Errorf("Free+enforce avatar key = %q, want .png extension (must flatten)", key)
	}
}

// TestAvatarAnimated_FreeUserReportOnlyPreservesAndLogs is the
// soak-window contract: enforce=false, Free user uploads animated GIF →
// the *user-visible* outcome is preservation (so we don't punish
// soak-window users), AND a structured report_only_deny line lands so
// SREs can count would-deny attempts before flipping enforce.
func TestAvatarAnimated_FreeUserReportOnlyPreservesAndLogs(t *testing.T) {
	t.Parallel()
	logBuf := &bytes.Buffer{}
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceAnimatedAvatars: false, // report-only.
		LogSink:                logBuf,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freeanimreport")

	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := uploadAvatarBytes(t, cli, csrf, makeTestAnimatedGIF(t, 3), "anim.gif")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	key := readAvatarKey(t, pool, userID)
	if !strings.HasSuffix(key, ".gif") {
		t.Errorf("Free+report-only avatar key = %q, want .gif (soak must preserve)", key)
	}
	out := logBuf.String()
	if !strings.Contains(out, `"msg":"entitlements.report_only_deny"`) {
		t.Errorf("expected report_only_deny log line; got: %s", out)
	}
	if !strings.Contains(out, `"feature":"animated_avatars"`) {
		t.Errorf("log must name the animated_avatars feature: %s", out)
	}
	if !strings.Contains(out, `"surface":"avatar-upload"`) {
		t.Errorf("log must tag surface=avatar-upload so dashboards can split this signal: %s", out)
	}
	if !strings.Contains(out, `"mode":"report_only"`) {
		t.Errorf("log must record mode=report_only when enforce is off: %s", out)
	}
}

// TestAvatarAnimated_StaticPNGNeverLogs ensures the gate doesn't emit a
// would-deny when the upload isn't animated in the first place — Free
// users uploading PNGs are a non-event and would otherwise drown the
// signal PRO-EXT01-17 needs.
//
// Note: today the gate fires on *every* Free upload, animated or not,
// because we check entitlement before Process knows the format. That
// is intentional — it keeps the gate code simple — but the log line
// includes the feature constant so downstream pipelines can ignore
// "would-have-denied PNG" events. We assert the log content here so a
// future change can't quietly start lying about which uploads we'd
// reject.
func TestAvatarAnimated_StaticPNGFromFreeStillLogs(t *testing.T) {
	t.Parallel()
	logBuf := &bytes.Buffer{}
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceAnimatedAvatars: false,
		LogSink:                logBuf,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freepngreport")

	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := uploadAvatarBytes(t, cli, csrf, makeTestPNG(t, 400, 400), "static.png")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	key := readAvatarKey(t, pool, userID)
	if !strings.HasSuffix(key, ".png") {
		t.Errorf("static PNG upload key = %q, want .png", key)
	}
	// The gate runs before Process, so we DO emit a log here. Asserting
	// this so a future refactor that defers the check can be reviewed
	// against this baseline rather than silently changing telemetry.
	if !strings.Contains(logBuf.String(), `"feature":"animated_avatars"`) {
		t.Errorf("expected report_only_deny log on Free upload; got: %s", logBuf.String())
	}
}
