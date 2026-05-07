// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func makeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 30, G: 60, B: 90, A: 255})
		}
	}
	buf := &bytes.Buffer{}
	if err := png.Encode(buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// uploadAvatar posts a multipart form to /settings/profile/avatar carrying
// the PNG body under "avatar". Returns the http response.
func uploadAvatar(t *testing.T, cli *client, csrf string, png []byte) *http.Response {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("csrf_token", csrf); err != nil {
		t.Fatalf("write csrf: %v", err)
	}
	part, err := mw.CreateFormFile("avatar", "test.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(png); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}
	req, err := http.NewRequest("POST", cli.srv.URL+"/settings/profile/avatar", body)
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

func TestAvatarUpload_Roundtrip(t *testing.T) {
	t.Parallel()
	cli := loginProfileUser(t)
	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := uploadAvatar(t, cli, csrf, makeTestPNG(t, 600, 600))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Location"); got != "/settings/profile" {
		t.Fatalf("Location=%q", got)
	}

	// After upload, the profile page should now show the Remove button
	// (HasAvatar=true).
	resp = cli.get(t, "/settings/profile")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/settings/profile/avatar/remove") {
		t.Fatalf("expected remove form post-upload, got: %s", body)
	}
}

func TestAvatarUpload_RejectsNonImage(t *testing.T) {
	t.Parallel()
	cli := loginProfileUser(t)
	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := uploadAvatar(t, cli, csrf, []byte("this is not an image"))
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "decode") && !strings.Contains(string(body), "format") {
		t.Fatalf("expected decode/format error, got: %s", body)
	}
}

func TestAvatarRemove_ClearsKey(t *testing.T) {
	t.Parallel()
	cli := loginProfileUser(t)
	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := uploadAvatar(t, cli, csrf, makeTestPNG(t, 400, 400))
	_ = resp.Body.Close()

	csrf = cli.extractCSRF(t, "/settings/profile")
	resp = cli.post(t, "/settings/profile/avatar/remove", url.Values{
		"csrf_token": {csrf},
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	// After remove, the profile page should NOT show the remove form.
	resp = cli.get(t, "/settings/profile")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "/settings/profile/avatar/remove") {
		t.Fatalf("expected no remove form after clearing avatar, got: %s", body)
	}
}
