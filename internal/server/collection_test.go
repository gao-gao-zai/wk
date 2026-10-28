package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/collection"
	"workbuddy2api/internal/groups"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/statestore"
)

func TestCollectionAdminBoundaryLocalReadsAndDurability(t *testing.T) {
	dir := t.TempDir()
	store, e := statestore.OpenSQLite(filepath.Join(dir, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "a", Domain: "www.workbuddy.cn"})
	var calls atomic.Int32
	m, e := collection.New(collection.Options{Store: store, UIDs: func() []string { return []string{"a"} }, Run: func(context.Context, collection.Target, collection.Snapshot) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`[]`), nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	gs, e := groups.New(filepath.Join(dir, "groups"))
	if e != nil {
		t.Fatal(e)
	}
	key, e := gs.CreateKey("test", "default")
	if e != nil {
		t.Fatal(e)
	}
	h := NewHandler(Config{APIKey: "fake-admin", Pool: p, Collection: m, CollectionRequired: true, Groups: gs})
	request := func(method, path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/admin/collection", "/admin/collection/snapshots?uids=a", "/admin/collection/jobs/x"} {
		if w := request("GET", path, key.Key, ""); w.Code != 401 {
			t.Fatalf("group key reached %s: %d", path, w.Code)
		}
	}
	if w := request("POST", "/admin/collection/jobs", key.Key, `{"resources":["balance"]}`); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/status", "/v1/models", "/admin/collection", "/admin/collection/snapshots?uids=a", "/admin/account/a/tasks", "/admin/growth/ledger"} {
		w := request("GET", path, "fake-admin", "")
		if w.Code == 500 {
			t.Fatalf("read %s: %s", path, w.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("page reads triggered upstream collection")
	}
	w := request("POST", "/admin/collection/jobs", "fake-admin", `{"uids":["a"],"resources":["models"]}`)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Body.String())
	}
	var result struct {
		ID string `json:"job_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		j, _ := m.Job(result.ID)
		if j.Done == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if w = request("GET", "/admin/collection/jobs/"+result.ID+"?page_size=100", "fake-admin", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	m.Close()
	m2, e := collection.New(collection.Options{Store: store, Run: func(context.Context, collection.Target, collection.Snapshot) (json.RawMessage, error) {
		t.Error("completed job replayed")
		return nil, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer m2.Close()
	job, ok := m2.Job(result.ID)
	if !ok || job.Success != 1 || len(job.Items) != 1 {
		t.Fatalf("not durable: %+v", job)
	}
	if snap := m2.Snapshot("a", "models"); snap.LastSuccess == 0 || string(snap.Data) != "[]" {
		t.Fatalf("not durable: %+v", snap)
	}
}

func TestCollectionModelSnapshotsRespectGroupScope(t *testing.T) {
	store, e := statestore.OpenSQLite(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b", Domain: "www.workbuddy.ai"})
	m, e := collection.New(collection.Options{Store: store, Run: func(_ context.Context, t collection.Target, _ collection.Snapshot) (json.RawMessage, error) {
		return json.RawMessage(`[{"ID":"model-` + t.UID + `","ContextWindow":1024}]`), nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	j, e := m.Submit(collection.Request{UIDs: []string{"a", "b"}, Resources: []string{"models"}})
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		j, _ = m.Job(j.ID)
		if j.Done == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	gs, e := groups.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = gs.Create("limited"); e != nil {
		t.Fatal(e)
	}
	_ = gs.SetAccountGroups("a", []string{"limited"})
	_ = gs.SetAccountGroups("b", []string{"default"})
	key, e := gs.CreateKey("limited", "limited")
	if e != nil {
		t.Fatal(e)
	}
	h := NewHandler(Config{APIKey: "admin", Pool: p, Groups: gs, Collection: m})
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 200 || len(body.Data) != 1 || body.Data[0].ID != "model-a" {
		t.Fatalf("scope leaked: %s", w.Body.String())
	}
}
