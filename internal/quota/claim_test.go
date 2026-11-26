package quota

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

const testOrg = "0b6f1c3e-4d2a-4f7e-9a51-3c8d2e7f6a10"

func TestClaimResetSendsTheExpectedRequest(t *testing.T) {
	var got Request
	client := newTestClient(doerFunc(func(_ context.Context, req Request) (Response, error) {
		got = req
		return Response{StatusCode: 200, Body: []byte(`{"result":"reset","resets_left":0,
			"cleared":["five_hour","seven_day_overage_included","seven_day_cowork"],"cooldown_until":"2026-09-05T17:30:00Z"}`)}, nil
	}), time.Second)

	res, err := client.ClaimReset(context.Background(), testToken, testOrg, "launch-2608-fable", "req_1")
	if err != nil {
		t.Fatalf("ClaimReset: %v", err)
	}
	if got.Method != "POST" || got.URL != "https://usage.test/api/organizations/"+testOrg+"/reset_rate_limits" {
		t.Errorf("request = %s %s", got.Method, got.URL)
	}
	if got.Header["Authorization"] != "Bearer "+testToken || got.Header["anthropic-beta"] != oauthBeta {
		t.Errorf("header = %v", got.Header)
	}
	var body map[string]string
	if err := json.Unmarshal(got.Body, &body); err != nil || body["program"] != "cedar_ember" ||
		body["grant_id"] != "launch-2608-fable" || body["request_id"] != "req_1" || len(body) != 3 {
		t.Errorf("body = %s", got.Body)
	}
	fable := model.WindowRef{Kind: model.WindowWeeklyScoped, Scope: model.FamilyFable}
	if res.Result != model.ClaimReset || res.ResetsLeft == nil || *res.ResetsLeft != 0 || res.CooldownUntil == nil ||
		len(res.Cleared) != 2 || res.Cleared[0] != (model.WindowRef{Kind: model.WindowSession}) || res.Cleared[1] != fable {
		t.Errorf("result = %+v", res)
	}
}

func TestClaimResetAnswers(t *testing.T) {
	cases := map[string]struct {
		status       int
		body         string
		want         string
		wantCategory ErrCategory
	}{
		"refused":   {200, `{"result":"Not_Limited","reason":"not_next_grant"}`, model.ClaimNotLimited, ""},
		"new value": {200, `{"result":"queued"}`, model.ClaimUnavailable, ""},
		"no result": {200, `{"resets_left":1}`, "", CategoryBadJSON},
		"not json":  {200, `<html>`, "", CategoryBadJSON},
		"throttled": {429, `{}`, "", CategoryRateLimited},
		"auth":      {401, `{}`, "", CategoryAuth},
	}
	for name, tc := range cases {
		client := newTestClient(respondWith(tc.status, []byte(tc.body)), time.Second)
		res, err := client.ClaimReset(context.Background(), testToken, testOrg, "full-reset-2609", "req_2")
		if tc.wantCategory != "" {
			if Category(err) != tc.wantCategory {
				t.Errorf("%s: err = %v, want category %s", name, err, tc.wantCategory)
			}
			continue
		}
		if err != nil || res.Result != tc.want {
			t.Errorf("%s: result = %+v, %v; want %s", name, res, err, tc.want)
		}
	}
}

func TestClaimResetSendsNothingForAMalformedID(t *testing.T) {
	sent := false
	client := newTestClient(doerFunc(func(context.Context, Request) (Response, error) {
		sent = true
		return Response{StatusCode: 200, Body: []byte(`{"result":"reset"}`)}, nil
	}), time.Second)
	for _, ids := range [][3]string{
		{"../x", "full-reset", "req"},
		{testOrg, "Full Reset", "req"},
		{testOrg, "full-reset", "req/1"},
		{testOrg, "full-reset", ""},
	} {
		if _, err := client.ClaimReset(context.Background(), testToken, ids[0], ids[1], ids[2]); !errors.Is(err, ErrClaimInput) || !errors.Is(err, model.ErrClaimNotSent) {
			t.Errorf("%v: err = %v, want ErrClaimInput", ids, err)
		}
	}
	if sent {
		t.Error("a malformed claim was sent")
	}
}

func TestClaimResetNeverLeaksTheToken(t *testing.T) {
	client := newTestClient(doerFunc(func(_ context.Context, req Request) (Response, error) {
		return Response{}, errors.New("dial failed for " + req.Header["Authorization"])
	}), time.Second)
	_, err := client.ClaimReset(context.Background(), testToken, testOrg, "full-reset", "req_3")
	if err == nil {
		t.Fatal("want an error")
	}
	assertNoToken(t, err.Error())
}

// A claim waits for its answer past the client's per-read timeout: the
// endpoint answers only once it has cleared the windows.
func TestClaimResetOutlastsTheReadTimeout(t *testing.T) {
	client := newTestClient(doerFunc(func(ctx context.Context, _ Request) (Response, error) {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
		return Response{StatusCode: 200, Body: []byte(`{"result":"reset"}`)}, nil
	}), 20*time.Millisecond)

	res, err := client.ClaimReset(context.Background(), testToken, testOrg, "launch-2608-fable", "req_1")
	if err != nil || res.Result != model.ClaimReset {
		t.Fatalf("ClaimReset = %+v, %v; want the answer that came after the read timeout", res, err)
	}
}
