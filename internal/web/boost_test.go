package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// boostSource is a stubSource that takes boosts.
type boostSource struct {
	stubSource
	boosted map[string]string
	cleared []string
}

func (s *boostSource) Boost(authID, span string, now time.Time) (model.Boost, error) {
	if span == "bad" {
		return model.Boost{}, errors.New("unknown boost span")
	}
	s.boosted[authID] = span
	return model.Boost{AuthID: authID, Span: span, SetAt: now}, nil
}

func (s *boostSource) Unboost(authID string, _ time.Time) bool {
	s.cleared = append(s.cleared, authID)
	return true
}

func post(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, nil))
	return rec
}

// The page names a seat by its published id, and the route boosts the
// credential that id stands for, answering in published ids only.
func TestBoostRouteResolvesThePublishedID(t *testing.T) {
	t.Parallel()
	src := &boostSource{stubSource: stubSource{status: idStatus()}, boosted: map[string]string{}}
	h := testHandler(src)

	rec := post(h, "/api/boost?seat="+testID(seatAID)+"&span=4h")
	if rec.Code != http.StatusOK || src.boosted[seatAID] != "4h" {
		t.Fatalf("boost: code %d, boosted %v, body %s", rec.Code, src.boosted, rec.Body)
	}
	if strings.Contains(rec.Body.String(), seatAID) {
		t.Errorf("boost response %s names the credential id", rec.Body)
	}
	var got struct {
		Seat  string       `json:"seat"`
		Boost *model.Boost `json:"boost"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Seat != testID(seatAID) || got.Boost == nil || got.Boost.AuthID != testID(seatAID) {
		t.Errorf("boost response = %s (%v), want the seat and its boost under the published id", rec.Body, err)
	}

	if rec := post(h, "/api/boost?seat="+testID(seatBID)+"&span=off"); rec.Code != http.StatusOK || len(src.cleared) != 1 || src.cleared[0] != seatBID {
		t.Errorf("clear: code %d, cleared %v", rec.Code, src.cleared)
	}
	for _, tc := range []struct {
		target string
		code   int
	}{
		{"/api/boost?seat=" + seatAID + "&span=1h", http.StatusNotFound},
		{"/api/boost?span=1h", http.StatusNotFound},
		{"/api/boost?seat=" + testID(seatAID) + "&span=bad", http.StatusBadRequest},
	} {
		if rec := post(h, tc.target); rec.Code != tc.code {
			t.Errorf("POST %s: code %d, want %d", tc.target, rec.Code, tc.code)
		}
	}
	if rec := get(t, h, "/api/boost?seat="+testID(seatAID)+"&span=1h"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: code %d, want 405", rec.Code)
	}
	if rec := post(testHandler(&stubSource{status: idStatus()}), "/api/boost?seat="+testID(seatAID)+"&span=1h"); rec.Code != http.StatusNotImplemented {
		t.Errorf("a source that takes no boosts: code %d, want 501", rec.Code)
	}
}
