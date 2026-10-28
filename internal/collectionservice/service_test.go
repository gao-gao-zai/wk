package collectionservice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/collection"
	"workbuddy2api/internal/metricsstore"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/statestore"
	"workbuddy2api/internal/upstream"
)

func fixture(t *testing.T, handler http.HandlerFunc) (*Service, *auth.Auth) {
	t.Helper()
	remote := httptest.NewServer(handler)
	t.Cleanup(remote.Close)
	dir := t.TempDir()
	p := pool.New("")
	a := &auth.Auth{UID: "a", AccessToken: "fake-access", RefreshToken: "fake-refresh", Domain: "www.workbuddy.cn", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	p.Add(a)
	up := upstream.New()
	up.BillingBaseCN = remote.URL
	up.ChatBaseCN = remote.URL
	db, e := statestore.OpenSQLite(filepath.Join(dir, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	metrics, e := metricsstore.Open(filepath.Join(dir, "metrics.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { metrics.Close() })
	s, e := New(db, p, up, metrics, &sync.Mutex{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Manager.Close)
	cfg := s.Manager.Settings()
	cfg.RequestsPerSecond = 20
	cfg.Automatic = false
	_ = s.Manager.Configure(cfg)
	return s, a
}
func TestUsageCompletePaginationAndWatermark(t *testing.T) {
	var pages atomic.Int32
	var starts []string
	var mu sync.Mutex
	s, a := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Page  int    `json:"pageNum"`
			Start string `json:"startTime"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pages.Add(1)
		mu.Lock()
		starts = append(starts, body.Start)
		mu.Unlock()
		rows := []map[string]any{}
		count := 200
		if body.Page == 2 {
			count = 5
		}
		for n := 0; n < count; n++ {
			rows = append(rows, map[string]any{"requestId": fmt.Sprintf("r-%d-%d", body.Page, n), "credit": 2.5})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"data": rows, "total": 205}})
	})
	value, e := s.run(context.Background(), collection.Target{UID: a.UID, Resource: "usage"}, collection.Snapshot{})
	if e != nil {
		t.Fatal(e)
	}
	var result usageResult
	_ = json.Unmarshal(value, &result)
	if pages.Load() != 2 || result.Records != 205 || result.Watermark == 0 {
		t.Fatalf("%s pages=%d", value, pages.Load())
	}
	_, e = s.run(context.Background(), collection.Target{UID: a.UID, Resource: "usage"}, collection.Snapshot{Data: value})
	if e != nil {
		t.Fatal(e)
	}
	if starts[2] <= starts[0] {
		t.Fatal("watermark did not advance window")
	}
}
func TestDeletedAccountResponseCannotApply(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s, a := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacityRemain":99}]}}}}`)
	})
	done := make(chan error)
	go func() {
		_, e := s.run(context.Background(), collection.Target{UID: a.UID, Resource: "balance"}, collection.Snapshot{})
		done <- e
	}()
	<-started
	s.Pool.Remove(a.UID)
	replacement := &auth.Auth{UID: a.UID, AccessToken: "different", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	s.Pool.Add(replacement)
	close(release)
	if e := <-done; e == nil {
		t.Fatal("accepted obsolete response")
	}
	st, _ := s.Pool.Status(a.UID)
	if st.CreditUpdatedAt != 0 {
		t.Fatal("replacement was overwritten")
	}
}
func TestCollectionFailuresDoNotDisableAndSecretsAreRedacted(t *testing.T) {
	s, a := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"code":11140,"msg":"secret-fake-refresh request illegal"}`)
	})
	_, e := s.run(context.Background(), collection.Target{UID: a.UID, Resource: "balance"}, collection.Snapshot{})
	if e == nil {
		t.Fatal("expected rejection")
	}
	if s.Pool.IsDisabled(a.UID) {
		t.Fatal("read failure disabled account")
	}
	if e.Error() == "" || strings.Contains(e.Error(), "secret-fake-refresh") {
		t.Fatal("unsafe error")
	}
}

func TestCollection401RefreshIsCoalesced(t *testing.T) {
	var refreshed atomic.Int32
	s, a := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/plugin/auth/token/refresh" {
			refreshed.Add(1)
			time.Sleep(30 * time.Millisecond)
			fmt.Fprint(w, `{"code":0,"data":{"accessToken":"fresh-fake","refreshToken":"fresh-refresh","expiresIn":3600}}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer fake-access" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"msg":"expired"}`)
			return
		}
		if r.URL.Path == "/v2/activity/growth/tasks" {
			fmt.Fprint(w, `{"code":0,"data":{"tasks":[]}}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CapacityRemain":99}]}}}}`)
	})
	errors := make(chan error, 2)
	for _, resource := range []string{"balance", "tasks"} {
		go func(r string) {
			_, e := s.run(context.Background(), collection.Target{UID: a.UID, Resource: r}, collection.Snapshot{})
			errors <- e
		}(resource)
	}
	for i := 0; i < 2; i++ {
		if e := <-errors; e != nil {
			t.Fatal(e)
		}
	}
	if refreshed.Load() != 1 || a.Snapshot().AccessToken != "fresh-fake" {
		t.Fatalf("refreshes=%d", refreshed.Load())
	}
}

func TestCollectionCredentialDeletionDoesNotRecreateFile(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s, a := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"fresh-fake","expiresIn":3600}}`)
	})
	a.FilePath = filepath.Join(t.TempDir(), "account.json")
	if e := a.SaveAtomic(); e != nil {
		t.Fatal(e)
	}
	a.ApplyRefresh("fake-access", "", "", time.Now().Add(-time.Hour).Unix())
	done := make(chan error)
	go func() {
		_, e := s.run(context.Background(), collection.Target{UID: a.UID, Resource: "credential"}, collection.Snapshot{})
		done <- e
	}()
	<-started
	s.AccountLock.Lock()
	s.Pool.Remove(a.UID)
	_ = os.Remove(a.FilePath)
	s.AccountLock.Unlock()
	close(release)
	if e := <-done; e == nil {
		t.Fatal("accepted deleted credential refresh")
	}
	if _, e := os.Stat(a.FilePath); !os.IsNotExist(e) {
		t.Fatal("deleted credential file recreated")
	}
}
