package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"workbuddy2api/internal/haozhuma"
)

func TestSMSProfilesSwitchPreservesBothCredentials(t *testing.T) {
	var ecCalls atomic.Int32
	ec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ecCalls.Add(1)
		if r.URL.Query().Get("token") != "ec-profile-fixture" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, "20.00")
	}))
	defer ec.Close()
	h, _, path := newWebuiTestHandler(t, `{"sms":{"haozhuma":{"user":"hao-profile-fixture","pass":"hao-pass-fixture","sid":"52283"}}}`)
	post := func(patch map[string]any, want int) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"sms": map[string]any{"haozhuma": patch}})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/config", strings.NewReader(string(raw))))
		if w.Code != want {
			t.Fatalf("save: %d %s", w.Code, w.Body.String())
		}
	}
	read := func() haozhuma.Settings {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		return smsSettingsFromDoc(doc).Normalized()
	}
	checkStored := func() {
		t.Helper()
		s := read()
		if s.Credentials.Haozhuma.User != "hao-profile-fixture" || s.Credentials.Haozhuma.Pass != "hao-pass-fixture" || s.Credentials.Ecode.Token != "ec-profile-fixture" {
			t.Fatal("switch discarded or mixed platform credentials")
		}
	}
	post(map[string]any{"provider": "ecode", "mode": "auto", "token": "ec-profile-fixture", "user": "", "pass": "", "ecode_base": ec.URL, "ecode_keyword": "腾讯科技", "ecode_province": "浙江", "ecode_card_type": "实卡"}, 200)
	checkStored()
	verify := httptest.NewRecorder()
	h.ServeHTTP(verify, httptest.NewRequest("POST", "/admin/account/sms/haozhuma/verify", strings.NewReader(`{"token":"rejected-fixture"}`)))
	if verify.Code != 400 {
		t.Fatal("draft verification reused saved token instead of validating draft")
	}
	// Repeated switches carry no credentials in the request.
	for _, provider := range []string{"haozhuma", "ecode", "haozhuma", "ecode"} {
		post(map[string]any{"provider": provider, "mode": "auto"}, 200)
		checkStored()
		s := read()
		if s.Sid != "52283" || s.EcodeProvince != "浙江" || s.EcodeCardType != "实卡" || s.EcodeBase != ec.URL {
			t.Fatal("platform parameters lost during switch")
		}
		if provider == "ecode" && (s.Token != "ec-profile-fixture" || s.User != "" || s.Pass != "" || h.cfg.AutoEnroll.HasHaozhuma()) {
			t.Fatal("Ecode must not use stored Haozhuma credentials or client")
		}
	}
	// Reloading persisted configuration (startup) keeps the selected platform independent.
	restarted, err := haozhuma.Build(context.Background(), read(), "http://127.0.0.1:1", nil, false)
	if err != nil || restarted.Provider != "ecode" {
		t.Fatalf("restart: %v", err)
	}
	// GET reports only presence, never either stored secret or the credentials object.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/admin/config", nil))
	for _, secret := range []string{"hao-profile-fixture", "hao-pass-fixture", "ec-profile-fixture", `"credentials"`} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("config GET exposed stored credentials")
		}
	}
	if !strings.Contains(w.Body.String(), `"saved_platforms":{"ecode":true,"haozhuma":true}`) {
		t.Fatal("saved platform status missing")
	}
	// Failed replacement is atomic across both profiles and the live client.
	before, _ := os.ReadFile(path)
	client := h.cfg.AutoEnroll.CurrentSMSClient()
	post(map[string]any{"token": "rejected-fixture"}, 400)
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || client != h.cfg.AutoEnroll.CurrentSMSClient() {
		t.Fatal("rejected token changed disk or runtime")
	}
	// Disabling fallback preserves its credentials; re-enabling needs no secret input.
	post(map[string]any{"provider": "haozhuma", "mode": "auto", "fallback_enabled": true}, 200)
	if !h.cfg.AutoEnroll.FallbackMounted() {
		t.Fatal("saved Ecode fallback not mounted")
	}
	post(map[string]any{"fallback_enabled": false, "mode": "auto"}, 200)
	if h.cfg.AutoEnroll.FallbackMounted() {
		t.Fatal("fallback still mounted")
	}
	checkStored()
	post(map[string]any{"fallback_enabled": true}, 200)
	if !h.cfg.AutoEnroll.FallbackMounted() || ecCalls.Load() == 0 {
		t.Fatal("saved fallback not reusable")
	}
}

func TestSMSProfilesFallbackToggleRejectsInvalidRequests(t *testing.T) {
	h, _, path := newWebuiTestHandler(t, `{"sms":{"haozhuma":{"token":"hao-fixture","sid":"52283","ecode_keyword":"腾讯科技"}}}`)
	before, _ := os.ReadFile(path)
	client := h.cfg.AutoEnroll.CurrentSMSClient()
	for _, body := range []string{
		`{"sms":{"haozhuma":{"fallback_enabled":true}}}`,
		`{"sms":{"haozhuma":{"fallback_enabled":false,"fallback":"unverified-fixture"}}}`,
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/config", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatal("invalid fallback toggle accepted")
		}
		after, _ := os.ReadFile(path)
		if string(after) != string(before) || client != h.cfg.AutoEnroll.CurrentSMSClient() {
			t.Fatal("invalid request changed configuration")
		}
	}
}

func TestSMSProfilesLegacyFallbackMigrationAndEcodeIsolation(t *testing.T) {
	var haoCalls atomic.Int32
	hao := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { haoCalls.Add(1); w.WriteHeader(503) }))
	defer hao.Close()
	ec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "legacy-ec-fixture" {
			t.Error("wrong migrated token")
		}
		fmt.Fprint(w, "8.00")
	}))
	defer ec.Close()
	seed, _ := json.Marshal(map[string]any{"sms": map[string]any{"haozhuma": map[string]any{
		"provider": "haozhuma", "token": "legacy-hao-fixture", "sid": "52283", "fallback": "legacy-ec-fixture", "fallback_base": ec.URL,
		"fallback_card_type": "实卡", "fallback_province": "广东", "ecode_keyword": "腾讯科技",
	}}})
	h, _, path := newWebuiTestHandler(t, string(seed))
	oldBase := haozhumaBase
	haozhumaBase = hao.URL
	t.Cleanup(func() { haozhumaBase = oldBase })
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/admin/config", strings.NewReader(`{"sms":{"haozhuma":{"provider":"ecode","mode":"auto"}}}`)))
	if w.Code != 200 || haoCalls.Load() != 0 {
		t.Fatalf("Ecode switch contacted unavailable Haozhuma: %d %s", w.Code, w.Body.String())
	}
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	s := smsSettingsFromDoc(doc).Normalized()
	if s.Credentials.Haozhuma.Token != "legacy-hao-fixture" || s.Token != "legacy-ec-fixture" || s.EcodeBase != ec.URL || s.EcodeCardType != "实卡" || s.EcodeProvince != "广东" {
		t.Fatal("legacy fallback parameters or either token lost")
	}
}
