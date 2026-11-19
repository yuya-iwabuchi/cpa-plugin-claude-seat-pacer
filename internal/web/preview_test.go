package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// previewSource is a stubSource that scores drafts, recording the draft.
type previewSource struct {
	stubSource
	draft *model.PaceConfig
}

func (s *previewSource) Preview(now time.Time, modelID string, draft model.PaceConfig) model.Status {
	s.draft = &draft
	st := s.Status(now, modelID)
	st.Config.Pace = draft
	return st
}

func decodeStatus(t *testing.T, body []byte) model.Status {
	t.Helper()
	var st model.Status
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	return st
}

// A preview scores the page's draft, held to the config's own bounds, and
// says it is one; a status without it is the running one.
func TestStatusPreviewScoresTheDraft(t *testing.T) {
	t.Parallel()
	src := &previewSource{stubSource: stubSource{status: idStatus()}}
	h := testHandler(src)

	draft := `{"shape":"power","curve_exponent":2,"landing_target":9,"hold_at_full":true}`
	rec := get(t, h, "/api/status?preview="+url.QueryEscape(draft))
	if rec.Code != http.StatusOK || src.draft == nil {
		t.Fatalf("preview: code %d, draft %v", rec.Code, src.draft)
	}
	if d := *src.draft; d.Shape != model.ShapePower || d.CurveExponent != 2 || !d.HoldAtFull || d.LandingTarget != model.Defaults().Pace.LandingTarget {
		t.Errorf("draft = %+v, want power, exponent 2, held, and the out-of-range landing back at its default", d)
	}
	if st := decodeStatus(t, rec.Body.Bytes()); !st.Preview || st.Config.Pace.Shape != model.ShapePower {
		t.Errorf("preview status: preview=%v pace=%+v", st.Preview, st.Config.Pace)
	}

	src.draft = nil
	if st := decodeStatus(t, get(t, h, "/api/status").Body.Bytes()); st.Preview || src.draft != nil {
		t.Errorf("plain status: preview=%v, draft %v", st.Preview, src.draft)
	}
	if rec := get(t, h, "/api/status?preview=%7Bnot"); rec.Code != http.StatusBadRequest {
		t.Errorf("an undecodable preview: code %d, want 400", rec.Code)
	}
	if st := decodeStatus(t, get(t, testHandler(&stubSource{status: idStatus()}), "/api/status?preview="+url.QueryEscape(draft)).Body.Bytes()); st.Preview {
		t.Error("a source that cannot preview answered a preview")
	}
}
