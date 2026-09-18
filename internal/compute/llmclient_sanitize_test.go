package compute

import (
	"strings"
	"testing"
)

func TestTruncateBodyRedactsBeforeCut(t *testing.T) {
	token := "123456789:" + strings.Repeat("a", 600)
	got := truncateBody([]byte("https://api.telegram.org/bot" + token + "/sendMessage"))
	if strings.Contains(got, "123456789") || strings.Contains(got, strings.Repeat("a", 20)) {
		t.Fatal(got)
	}
}
