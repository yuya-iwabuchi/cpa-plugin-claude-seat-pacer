package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

const testOrg = "0f8fad5b-d9cb-469f-a165-70867728950e"

// claimSource is a stubSource that uses limit resets.
type claimSource struct {
	stubSource
	claims   []string
	checks   []string
	claimErr error
	read     bool
}

func (s *claimSource) ClaimReset(_ context.Context, authID, grantID, requestID string) (model.ResetClaim, error) {
	s.claims = append(s.claims, authID+"|"+grantID+"|"+requestID)
	if s.claimErr != nil {
		return model.ResetClaim{}, s.claimErr
	}
	return model.ResetClaim{Result: model.ClaimReset, Cleared: []model.WindowRef{{Kind: model.WindowSession}}}, nil
}

func (s *claimSource) RecheckSeat(_ context.Context, authID string) (bool, error) {
	s.checks = append(s.checks, authID)
	return s.read, nil
}

// The page names a seat by its published id, and the route claims on the
// credential that id stands for, answering with the endpoint's answer.
func TestResetRouteClaimsOnThePublishedSeat(t *testing.T) {
	t.Parallel()
	src := &claimSource{stubSource: stubSource{status: idStatus()}}
	h := testHandler(src)

	rec := post(h, "/api/reset?seat="+testID(seatAID)+"&grant=full-reset-2611&request=req-1")
	if rec.Code != http.StatusOK || len(src.claims) != 1 || src.claims[0] != seatAID+"|full-reset-2611|req-1" {
		t.Fatalf("claim: code %d, claims %v, body %s", rec.Code, src.claims, rec.Body)
	}
	var got model.ResetClaim
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Result != model.ClaimReset {
		t.Errorf("claim response = %s (%v), want the endpoint's answer", rec.Body, err)
	}
	if strings.Contains(rec.Body.String(), seatAID) {
		t.Errorf("claim response %s names the credential id", rec.Body)
	}

	for _, tc := range []struct {
		target string
		code   int
	}{
		{"/api/reset?seat=" + seatAID + "&grant=g&request=r", http.StatusNotFound},
		{"/api/reset?grant=g&request=r", http.StatusNotFound},
		{"/api/reset?seat=" + testID(seatAID) + "&request=r", http.StatusBadRequest},
		{"/api/reset?seat=" + testID(seatAID) + "&grant=g", http.StatusBadRequest},
	} {
		if rec := post(h, tc.target); rec.Code != tc.code {
			t.Errorf("POST %s: code %d, want %d", tc.target, rec.Code, tc.code)
		}
	}
	if len(src.claims) != 1 {
		t.Errorf("claims = %v, want none from the refused requests", src.claims)
	}
	if rec := get(t, h, "/api/reset?seat="+testID(seatAID)+"&grant=g&request=r"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: code %d, want 405", rec.Code)
	}
	if rec := post(testHandler(&stubSource{status: idStatus()}), "/api/reset?seat="+testID(seatAID)+"&grant=g&request=r"); rec.Code != http.StatusNotImplemented {
		t.Errorf("a source that uses no resets: code %d, want 501", rec.Code)
	}

	src.claimErr = errors.New(`quota: transport: request failed: Post "https://api.anthropic.com/api/organizations/` + testOrg + `/reset_rate_limits": dial tcp 10.1.2.3:443: connect: connection refused (` + seatAID + `)`)
	rec = post(h, "/api/reset?seat="+testID(seatAID)+"&grant=g&request=r")
	body := rec.Body.String()
	if rec.Code != http.StatusBadGateway || !strings.Contains(body, "connection refused") {
		t.Errorf("failed claim: code %d body %s, want 502 with the reason", rec.Code, body)
	}
	src.claimErr = fmt.Errorf("the seat's credential names no organization: %w", model.ErrClaimNotSent)
	if rec := post(h, "/api/reset?seat="+testID(seatAID)+"&grant=g&request=r"); rec.Code != http.StatusConflict {
		t.Errorf("claim never sent: code %d, want 409", rec.Code)
	}
	for _, leak := range []string{testOrg, "10.1.2.3", seatAID, "api.anthropic.com"} {
		if strings.Contains(body, leak) {
			t.Errorf("failed claim body %s carries %q", body, leak)
		}
	}
}

func TestResetRouteRechecksASeat(t *testing.T) {
	t.Parallel()
	src := &claimSource{stubSource: stubSource{status: idStatus()}, read: true}
	h := testHandler(src)

	rec := post(h, "/api/reset?seat="+testID(seatBID)+"&check=1")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"read":true}` {
		t.Fatalf("check: code %d body %s, want {\"read\":true}", rec.Code, rec.Body)
	}
	if len(src.checks) != 1 || src.checks[0] != seatBID || len(src.claims) != 0 {
		t.Errorf("checks %v claims %v, want one check of seat B and no claim", src.checks, src.claims)
	}
}
