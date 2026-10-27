package haozhumah5

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fixture(t *testing.T, fn http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("Cookie") != "" {
			t.Error("incorrect authentication")
		}
		fn(w, r)
	}))
	t.Cleanup(srv.Close)
	old := BaseURL
	BaseURL = srv.URL
	t.Cleanup(func() { BaseURL = old })
	return New("fixture-token")
}
func TestWebProjectsAndPagination(t *testing.T) {
	calls := 0
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/projects/search":
			if r.URL.Query().Get("keyword") != "腾讯" || r.URL.Query().Get("size") != "50" {
				t.Error(r.URL)
			}
			fmt.Fprint(w, `{"code":0,"data":{"list":[{"id":52283,"sid":"abcdef0123456789","name":"无数字前缀"}]}}`)
		case "/bindings":
			calls++
			if r.URL.Query().Get("keyword") != "腾讯" {
				t.Error("missing filter")
			}
			if r.URL.Query().Get("page") == "1" {
				fmt.Fprint(w, `{"code":0,"data":{"list":[{"code":"52283-A","price":"0.55","available":4,"status":"paused"}],"total":2}}`)
			} else {
				fmt.Fprint(w, `{"code":0,"data":{"list":[{"code":"52283-B","price":1.1,"available":"8","lastUsedAt":"2026-10-28"}],"total":2}}`)
			}
		default:
			t.Error(r.URL.Path)
		}
	})
	p, err := c.Projects(context.Background(), " 腾讯 ")
	if err != nil || len(p) != 1 || p[0].ProjectID != "52283" {
		t.Fatalf("%+v %v", p, err)
	}
	mine, err := c.MyUIDs(context.Background(), "腾讯")
	if err != nil || len(mine) != 2 || mine[0].State != "已暂停" || mine[1].LastUsedAt != "2026-10-28" || calls != 2 {
		t.Fatalf("%+v %v calls=%d", mine, err, calls)
	}
}
func TestWebIncompletePagesAreErrors(t *testing.T) {
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"code":0,"data":{"list":[],"total":2}}`) })
	if _, err := c.MyUIDs(context.Background(), ""); err == nil {
		t.Fatal("partial list accepted")
	}
}
func TestWebJoinPartialFailureAndDeleteVerification(t *testing.T) {
	deleted := false
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"code":0,"data":{"list":[],"total":0}}`)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing JSON")
		}
		var body struct {
			Codes  []string `json:"codes"`
			Action string   `json:"action"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Codes) != 1 || body.Codes[0] != "52283-A" {
			t.Error("bad body")
		}
		if r.URL.Path == "/bindings" {
			fmt.Fprint(w, `{"code":0,"data":{"results":[{"code":"52283-A","ok":false,"message":"fixture-token"}]}}`)
		} else if r.URL.Path == "/bindings/batch" {
			deleted = body.Action == "delete"
			fmt.Fprint(w, `{"code":0,"data":{"affected":1}}`)
		} else {
			t.Error(r.URL)
		}
	})
	err := c.AddUID(context.Background(), "52283-A")
	if err == nil || strings.Contains(err.Error(), "fixture-token") {
		t.Fatal("partial failure or secret handling")
	}
	if err := c.RemoveUIDs(context.Background(), []string{"52283-A", "52283-A"}); err != nil || !deleted {
		t.Fatalf("delete: %v", err)
	}
}
func TestWeb401AndSessionReplacement(t *testing.T) {
	calls := 0
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(401)
		fmt.Fprint(w, "fixture-token")
	})
	if err := c.Ping(context.Background()); !errors.Is(err, ErrSessionExpired) || !c.Info().Expired {
		t.Fatal(err)
	}
	if _, err := c.Projects(context.Background(), "腾讯"); !errors.Is(err, ErrSessionExpired) || calls != 1 {
		t.Fatal("expired token reused")
	}
	c.Session("")
	if c.Info().Has {
		t.Fatal("clear failed")
	}
	if err := c.Ping(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Fatal(err)
	}
}
func TestWebRejectsInvalidPriceAndMissingStockIsUnknown(t *testing.T) {
	for _, price := range []scalar{"", "NaN", "Infinity", "bad", "-1"} {
		if _, err := (webCode{Price: price}).item(); err == nil {
			t.Fatalf("accepted %q", price)
		}
	}
	it, err := (webCode{Price: "0.55"}).item()
	if err != nil || it.Stock != -1 {
		t.Fatal(it, err)
	}
}
func TestWebDoesNotForwardTokenOnRedirect(t *testing.T) {
	forwarded := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
	defer other.Close()
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) })
	if err := c.Ping(context.Background()); err == nil || forwarded {
		t.Fatal("redirect followed")
	}
}

func TestWebMarketIncludesOwnedButExcludesPaused(t *testing.T) {
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/public-codes") {
			fmt.Fprint(w, `{"code":0,"data":[{"code":"52283-A","price":"1","available":2},{"code":"52283-P","price":"1","available":9}]}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"list":[{"code":"52283-B","price":"0.55","available":4,"prefixes":["170"]},{"code":"52283-P","price":"1","available":9,"status":"paused"}],"total":2}}`)
	})
	items, err := c.UIDs(context.Background(), "abcdef0123456789")
	if err != nil || len(items) != 2 || items[0].UID != "52283-B" || len(items[0].Segments) != 1 {
		t.Fatalf("market merge: %+v %v", items, err)
	}
}

func TestWebOwnedOnlyMarket(t *testing.T) {
	c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/public-codes") {
			fmt.Fprint(w, `{"code":0,"data":[]}`)
		} else {
			fmt.Fprint(w, `{"code":0,"data":{"list":[{"code":"52283-B","price":"1","available":3}],"total":1}}`)
		}
	})
	c.RememberProject("abcdef0123456789", "52283")
	items, err := c.UIDs(context.Background(), "abcdef0123456789")
	if err != nil || len(items) != 1 {
		t.Fatalf("owned market: %+v %v", items, err)
	}
}
