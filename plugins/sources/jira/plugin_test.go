package jira

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

func TestNewSourceSyncsTheConfiguredSiteWithTheInjectedCredentials(t *testing.T) {
	s := newStub(t)
	c, err := Plugin().NewSource(lore.SourceConfig{
		Instance: "jira-acme",
		Config:   json.RawMessage(`{"base_url":"` + s.server.URL + `","projects":["PROJ"]}`),
		Secrets:  map[string]string{"email": fakeEmail, "token": fakeToken},
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if got := c.Name(); got != "jira-acme" {
		t.Errorf("Name() = %q, want the instance id", got)
	}

	got := drain(t, c.(*Connector), nil)
	if got.err != nil {
		t.Fatalf("Changes: %v", got.err)
	}
	if len(allDocs(got.batches)) == 0 {
		t.Fatal("the configured site was never read")
	}

	if auth := s.authHeader(); auth != wantBasicAuth {
		t.Errorf("Authorization = %q, want basic auth built from the injected email and token", auth)
	}
	jqls := s.sentJQL()
	if len(jqls) == 0 {
		t.Fatal("no search reached the site")
	}
	if !strings.Contains(jqls[0], "project IN (PROJ)") {
		t.Errorf("jql %q does not scope the sync to the configured project", jqls[0])
	}
}
