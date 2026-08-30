package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"lore/internal/entities"
	"lore/internal/services"
)

var testCreatedAt = time.Date(2025, 3, 12, 9, 30, 0, 0, time.UTC)

func testBundle() *entities.EvidenceBundle {
	return &entities.EvidenceBundle{
		Question: testQuestion,
		Anchor: entities.Anchor{
			Kind:  entities.AnchorQuery | entities.AnchorTimeWindow,
			Query: testQuestion,
			Window: &entities.TimeWindow{
				From:       testCreatedAt.AddDate(0, 0, -30),
				To:         testCreatedAt,
				Derivation: "date 2025-03-12 +/- 30d",
				AnchoredBy: "jira:ticket:PROJ-1",
			},
		},
		Nodes: []entities.EvidenceNode{{
			Doc: entities.DocumentMeta{
				ID:        "github:pr:42",
				Source:    "github",
				Type:      entities.DocTypePR,
				Title:     "Switch the store to postgres",
				Author:    "ada",
				URL:       "https://github.com/acme/lore/pull/42",
				CreatedAt: testCreatedAt,
				UpdatedAt: testCreatedAt,
			},
			Excerpt: "we need transactional DDL",
			Role:    entities.RoleSeed,
			Score:   0.5,
			Via: []entities.Edge{{
				Src:        "github:pr:42",
				Dst:        "jira:ticket:PROJ-1",
				Kind:       entities.EdgeKindPRClosesIssue,
				Confidence: 1,
			}},
		}},
		Chains: [][]entities.DocID{{"jira:ticket:PROJ-1", "github:pr:42"}},
		Gaps:   []string{"trail ends at PROJ-1; no linked follow-up"},
	}
}

const testBundleJSON = `{
  "question": "why postgres over mysql",
  "anchor": {
    "kinds": ["query", "time_window"],
    "query": "why postgres over mysql",
    "window": {
      "from": "2025-02-10T09:30:00Z",
      "to": "2025-03-12T09:30:00Z",
      "derivation": "date 2025-03-12 +/- 30d",
      "anchored_by": "jira:ticket:PROJ-1"
    }
  },
  "nodes": [{
    "doc": {
      "id": "github:pr:42",
      "source": "github",
      "type": "pr",
      "title": "Switch the store to postgres",
      "author": "ada",
      "url": "https://github.com/acme/lore/pull/42",
      "created_at": "2025-03-12T09:30:00Z",
      "updated_at": "2025-03-12T09:30:00Z"
    },
    "excerpt": "we need transactional DDL",
    "role": "seed",
    "score": 0.5,
    "via": [{
      "src": "github:pr:42",
      "dst": "jira:ticket:PROJ-1",
      "kind": "pr_closes_issue",
      "confidence": 1
    }]
  }],
  "chains": [["jira:ticket:PROJ-1", "github:pr:42"]],
  "gaps": ["trail ends at PROJ-1; no linked follow-up"]
}`

func assertSameJSON(t *testing.T, got, want []byte) {
	t.Helper()

	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("unmarshal result: %v (%s)", err, got)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("json =\n%s\nwant\n%s", got, want)
	}
}

func TestEncodeBundleIsTheToolWireShape(t *testing.T) {
	encoded, err := EncodeBundle(testBundle())
	if err != nil {
		t.Fatalf("EncodeBundle: %v", err)
	}
	assertSameJSON(t, encoded, []byte(testBundleJSON))

	f := newToolFixture(t)
	f.query.EXPECT().
		FindDecision(gomock.Any(), services.FindDecisionRequest{Question: testQuestion}).
		Return(testBundle(), nil)

	res := f.call(t, questionArgs(testQuestion))
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", errorText(t, res))
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	assertSameJSON(t, structured, encoded)
}
