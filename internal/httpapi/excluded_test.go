package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQueryToolsDoNotAdvertiseDeniedTraceCounts(t *testing.T) {
	s := &Server{}
	for _, tool := range s.mcpTools() {
		data, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "excluded") {
			t.Errorf("tool %v advertises denied trace counts: %s", tool["name"], data)
		}
	}
}
