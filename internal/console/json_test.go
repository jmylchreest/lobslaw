package console

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRESTRevisionsKeepLegacyNumbersAndExactLargeValues(t *testing.T) {
	for _, rev := range []uint64{42, 9007199254740993, 18446744073709551615} {
		for _, value := range []any{BotView{Revision: rev}, GroupView{Revision: rev}, InboxItemView{Revision: rev}} {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			quoted := strings.Contains(string(raw), `"revision":"`)
			if quoted != (rev > 9007199254740991) {
				t.Fatalf("unsafe revision encoding: %s", raw)
			}
		}
	}
}
func TestRESTPatchAcceptsExactStringRevisionAndLegacyNumber(t *testing.T) {
	for _, raw := range []string{`{"revision":"9007199254740993","enabled":false,"tools":[]}`, `{"revision":9007199254740993,"enabled":false,"tools":[]}`} {
		var p BotPatch
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		if p.Revision == nil || *p.Revision != 9007199254740993 || p.Enabled == nil || *p.Enabled || p.Tools == nil || len(*p.Tools) != 0 || p.Description != nil {
			t.Fatalf("patch lost precision or presence: %+v", p)
		}
		var g GroupInput
		if err := json.Unmarshal([]byte(raw), &g); err != nil {
			t.Fatal(err)
		}
		if g.Revision == nil || *g.Revision != 9007199254740993 {
			t.Fatal("group revision lost")
		}
	}
}
