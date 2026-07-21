package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestBeadMetadataSearchReturnsCreatedAscendingCompactMatches(t *testing.T) {
	fs := newFakeState(t)
	store := fs.BeadStore("myrig")
	for _, b := range []beads.Bead{
		{ID: "later", Title: "later", Metadata: map[string]string{"pr_number": "9"}, Type: "task"},
		{ID: "excluded", Title: "excluded", Metadata: map[string]string{"pr_number": "9"}, Type: "epic"},
		{ID: "first", Title: "first", Metadata: map[string]string{"pr_number": "9"}, Type: "task"},
	} {
		if _, err := store.Create(b); err != nil {
			t.Fatalf("create %q: %v", b.ID, err)
		}
	}
	body := []byte(`{"metadata":{"pr_number":"9"},"exclude_types":["epic"],"limit":1}`)
	rec := httptest.NewRecorder()
	newTestCityHandler(t, fs).ServeHTTP(rec, newPostRequest(cityURL(fs, "/beads/search"), bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Items []beads.Bead `json:"items"`
		Total int          `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Total != 2 || len(got.Items) != 1 || got.Items[0].Title != "later" {
		t.Fatalf("got total=%d titles=%v, want total=2 titles=[later]", got.Total, beadTitlesForSearch(got.Items))
	}
}

func TestBeadMetadataSearchPreservesPriorityTieBreakForPatrolParity(t *testing.T) {
	fs := newFakeState(t)
	created := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	priorityZero, priorityThree := 0, 3
	fs.stores["myrig"] = beads.NewMemStoreFrom(2, []beads.Bead{
		{ID: "lower-priority", Title: "lower-priority", Type: "task", Status: "open", Priority: &priorityThree, CreatedAt: created, Metadata: map[string]string{"pr_number": "9"}},
		{ID: "higher-priority", Title: "higher-priority", Type: "task", Status: "open", Priority: &priorityZero, CreatedAt: created, Metadata: map[string]string{"pr_number": "9"}},
	}, nil)
	rec := httptest.NewRecorder()
	newTestCityHandler(t, fs).ServeHTTP(rec, newPostRequest(cityURL(fs, "/beads/search"), bytes.NewReader([]byte(`{"metadata":{"pr_number":"9"},"limit":1}`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Items []beads.Bead `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].ID != "higher-priority" {
		t.Fatalf("selected=%+v, want higher-priority", got.Items)
	}
}

func TestBeadMetadataSearchRejectsEmptyMetadata(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"metadata":{},"limit":1}`),
		[]byte(`{"metadata":{"pr_number":"9"}}`),
		[]byte(`{"metadata":{"pr_number":"9"},"limit":0}`),
	} {
		fs := newFakeState(t)
		rec := httptest.NewRecorder()
		newTestCityHandler(t, fs).ServeHTTP(rec, newPostRequest(cityURL(fs, "/beads/search"), bytes.NewReader(body)))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body=%s status=%d response=%s, want 422", body, rec.Code, rec.Body.String())
		}
	}
}

func TestBeadMetadataSearchAppliesScopedFiltersAndReturnsNoMatch(t *testing.T) {
	fs := newFakeState(t)
	fs.stores["other"] = beads.NewMemStore()
	fs.cfg.Rigs = append(fs.cfg.Rigs, config.Rig{Name: "other", Path: t.TempDir()})
	store := fs.BeadStore("myrig")
	for _, b := range []beads.Bead{
		{Title: "want", Type: "task", Assignee: "worker", Metadata: map[string]string{"pr_number": "9"}},
		{Title: "wrong-assignee", Type: "task", Assignee: "elsewhere", Metadata: map[string]string{"pr_number": "9"}},
		{Title: "wrong-type", Type: "epic", Assignee: "worker", Metadata: map[string]string{"pr_number": "9"}},
	} {
		if _, err := store.Create(b); err != nil {
			t.Fatalf("create %q: %v", b.ID, err)
		}
	}
	wrongStatus, err := store.Create(beads.Bead{Title: "wrong-status", Type: "task", Assignee: "worker", Metadata: map[string]string{"pr_number": "9"}})
	if err != nil {
		t.Fatalf("create wrong-status: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(wrongStatus.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("update wrong-status: %v", err)
	}
	if _, err := fs.BeadStore("other").Create(beads.Bead{Title: "other", Type: "task", Assignee: "worker", Metadata: map[string]string{"pr_number": "9"}}); err != nil {
		t.Fatalf("create other-rig bead: %v", err)
	}
	h := newTestCityHandler(t, fs)

	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"scoped", `{"metadata":{"pr_number":"9"},"exclude_types":["epic"],"status":"open","assignee":"worker","rig":"myrig","limit":10}`, []string{"want"}},
		{"no-match", `{"metadata":{"pr_number":"missing"},"limit":1}`, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/beads/search"), bytes.NewReader([]byte(tc.body))))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var got struct {
				Items []beads.Bead `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if titles := beadTitlesForSearch(got.Items); !equalSearchTitles(titles, tc.want) {
				t.Fatalf("titles=%v, want %v", titles, tc.want)
			}
		})
	}
}

func TestBeadMetadataSearchSurfacesPartialAndTotalOutage(t *testing.T) {
	boom := errors.New("metadata read failed")
	fs := newFakeState(t)
	if _, err := fs.BeadStore("myrig").Create(beads.Bead{ID: "good", Title: "good", Metadata: map[string]string{"pr_number": "9"}}); err != nil {
		t.Fatalf("create good bead: %v", err)
	}
	fs.stores["bad"] = &metadataFailingStore{Store: beads.NewMemStore(), err: boom}
	fs.cfg.Rigs = append(fs.cfg.Rigs, config.Rig{Name: "bad", Path: t.TempDir()})
	h := newTestCityHandler(t, fs)
	body := []byte(`{"metadata":{"pr_number":"9"},"limit":1}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/beads/search"), bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("partial status=%d body=%s", rec.Code, rec.Body.String())
	}
	var partial struct {
		Items   []beads.Bead `json:"items"`
		Partial bool         `json:"partial"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &partial); err != nil {
		t.Fatalf("decode partial: %v", err)
	}
	if !partial.Partial || len(partial.Items) != 1 || partial.Items[0].Title != "good" {
		t.Fatalf("partial=%v items=%+v, want marked partial good result", partial.Partial, partial.Items)
	}

	fs.stores = map[string]beads.Store{"bad": &metadataFailingStore{Store: beads.NewMemStore(), err: boom}}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/beads/search"), bytes.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("total outage status=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
}

type metadataFailingStore struct {
	beads.Store
	err error
}

func (s *metadataFailingStore) ListByMetadata(_ map[string]string, _ int, _ ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

func beadTitlesForSearch(in []beads.Bead) []string {
	out := make([]string, 0, len(in))
	for _, b := range in {
		out = append(out, b.Title)
	}
	return out
}

func equalSearchTitles(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
