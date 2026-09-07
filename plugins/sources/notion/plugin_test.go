package notion

import (
	"encoding/json"
	"testing"

	"github.com/setthasit/Lore/sdk"
)

// Every document lands in the instance's own namespace, so the id the host
// configures has to reach the connector this builds.
func TestNewSourceCarriesTheConfiguredInstanceID(t *testing.T) {
	c, err := Plugin().NewSource(lore.SourceConfig{
		Instance: "notion-acme",
		Config:   json.RawMessage(`{"root_pages":["Engineering"]}`),
		Secrets:  map[string]string{"token": fakeToken},
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if got := c.Name(); got != "notion-acme" {
		t.Errorf("Name() = %q, want the instance id", got)
	}
}
