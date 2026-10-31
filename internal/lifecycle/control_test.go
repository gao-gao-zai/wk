package lifecycle

import (
	"net/http/httptest"
	"testing"
)

func TestControlRequiresTwoStageDrainAndCommit(t *testing.T) {
	Default.Resume()
	defer Default.Resume()
	var foreground Gate
	finish, _ := foreground.Enter()
	commits, stops := 0, 0
	h := Control("release-a", &foreground, func() bool { return true }, func() error { commits++; return nil }, func() { stops++ })
	request := func(path string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != want {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	request("/pause?release=wrong", 409)
	request("/pause?release=release-a", 200)
	request("/quiesce?release=release-a", 409)
	finish()
	request("/quiesce?release=release-a", 200)
	background := Default.Track()
	request("/stop?release=release-a", 409)
	background()
	request("/stop?release=release-a", 200)
	request("/resume?release=release-a", 409)
	if commits != 1 || stops != 1 {
		t.Fatal("commit/stop count", commits, stops)
	}
}
