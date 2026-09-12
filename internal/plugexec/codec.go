package plugexec

import (
	"encoding/json"

	"github.com/setthasit/Lore/sdk"
)

// json.RawMessage(nil) marshals as `null`, which a plugin's config decoder never has to handle.
func emptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func secretsOrEmpty(secrets map[string]string) map[string]string {
	if secrets == nil {
		return map[string]string{}
	}
	return secrets
}

func cursorOrEmpty(cursor lore.Cursor) lore.Cursor {
	if cursor == nil {
		return lore.Cursor{}
	}
	return cursor
}
