package upstream

import (
	"encoding/json"
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
)

// Match the newer client research contract at yreidev/workbuddy2api-panel
// 39049fcc: isFirstPage is a JSON boolean on buddyapp_enter_click. Validate the
// actual request body, because a Go map's string "1" otherwise serializes fine.
func TestDesktopBuddyAppReportUsesBooleanFirstPage(t *testing.T) {
	const buddyID = "fixture-buddy"
	const buddyName = "fixture-app"
	wantCodes := []string{"buddyapp_discover_click", "buddyapp_show", "buddyapp_enter_click", "buddyapp_auth_confirm_click", "buddyapp_bindaccount_skip_click"}
	requests := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v2/report" {
			t.Fatalf("unexpected report destination: %s %s", r.Method, r.URL.Path)
		}
		var events []map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&events); err != nil {
			t.Fatal(err)
		}
		if len(events) != len(wantCodes) {
			t.Fatalf("event count: got %d want %d", len(events), len(wantCodes))
		}
		for i, event := range events {
			var code string
			if err := json.Unmarshal(event["eventCode"], &code); err != nil || code != wantCodes[i] {
				t.Fatalf("event %d changed code or order", i)
			}
		}
		var firstPage bool
		if err := json.Unmarshal(events[2]["isFirstPage"], &firstPage); err != nil || !firstPage {
			// This reproduces the live partial-acceptance response without
			// contacting the real supplier or replaying accepted events.
			return jsonResp(http.StatusBadRequest, `{"code":10001,"msg":"partial success: kept=4 events, dropped=1 events by format validation","data":{"kept":4,"dropped":1,"invalidIndices":[2]}}`), nil
		}
		return jsonResp(http.StatusOK, `{"code":0,"data":{}}`), nil
	})
	err := c.ReportDesktopEvent(&auth.Auth{UID: "fixture-account", AccessToken: "fixture-token"}, DesktopBuddyAppSequence(buddyID, buddyName)...)
	if err != nil {
		t.Fatalf("Buddy app event failed wire-format validation: %v", err)
	}
	if requests != 1 {
		t.Fatalf("report sent %d times, want one batch", requests)
	}
}

func TestDesktopBuddyAppPartialAcceptanceIsNotReplayed(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			requests := 0
			c := testClient(func(r *http.Request) (*http.Response, error) {
				requests++
				return jsonResp(status, `{"code":10001,"msg":"partial success: kept=4 events, dropped=1 events by format validation","data":{"kept":4,"dropped":1,"invalidIndices":[2]}}`), nil
			})
			err := c.ReportDesktopEvent(&auth.Auth{UID: "fixture-account", AccessToken: "fixture-token"}, DesktopBuddyAppSequence("fixture-buddy", "fixture-app")...)
			if err == nil {
				t.Fatal("partially accepted batch was reported as successful")
			}
			if requests != 1 {
				t.Fatalf("partially accepted batch was sent %d times, want one attempt", requests)
			}
		})
	}
}
