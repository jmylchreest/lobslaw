package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type activityTestInbox struct {
	InboxAPI
	t      *testing.T
	limits chan int
}

func (s activityTestInbox) Recent(_ context.Context, recipient string, limit int) ([]*pb.ConsoleInboxItem, error) {
	select {
	case s.limits <- limit:
	default:
		s.t.Error("unexpected extra inbox read")
	}
	if limit < 1 || limit > maxActivityLimit {
		s.t.Errorf("unbounded inbox limit: %d", limit)
	}
	if recipient == "private" {
		s.t.Error("read another owner's inbox")
	}
	ids := []string{"02", "04"}
	if recipient == "second" {
		ids = []string{"03", "01", "05"}
	}
	out := make([]*pb.ConsoleInboxItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, &pb.ConsoleInboxItem{Id: id, Recipient: recipient})
	}
	return out, nil
}

func TestActivityLimitsLocalAndRemote(t *testing.T) {
	t.Parallel()
	limits := make(chan int, 2)
	backend := startWebREST(t, nil, func(c *RESTConfig) {
		c.Inbox = activityTestInbox{t: t, limits: limits}
		c.Bots = &memBots{recs: map[string]*pb.BotRecord{
			"first":   {Id: "first", Owner: "user:alice"},
			"second":  {Id: "second", Owner: "user:alice"},
			"private": {Id: "private", Owner: "user:bob"},
		}}
	})
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = testConsoleClient(t, backend) })
	for _, srv := range []*Server{backend, front} {
		for _, tc := range []struct {
			raw  string
			want int
		}{
			{"", 5}, {"0", 5}, {"1", 1}, {"2", 2},
			{strconv.Itoa(defaultActivityLimit), 5}, {strconv.Itoa(maxActivityLimit), 5},
			{"-1", -1}, {strconv.Itoa(maxActivityLimit + 1), -1},
			{"2147483647", -1}, {"9223372036854775807", -1},
			{"9223372036854775808", -1}, {"999999999999999999999999999999999999", -1},
			{"-9223372036854775809", -1}, {"invalid", -1},
		} {
			t.Run(srv.Addr()+"/"+tc.raw, func(t *testing.T) {
				response := doJSON(t, http.MethodGet, webBaseURL(srv)+"/v1/activity?limit="+tc.raw, "", http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
				if tc.want < 0 {
					select {
					case limit := <-limits:
						t.Fatalf("invalid request reached inbox with limit %d", limit)
					default:
					}
					if response.StatusCode != http.StatusBadRequest {
						t.Fatalf("status=%d, want 400", response.StatusCode)
					}
					return
				}
				if response.StatusCode != http.StatusOK {
					t.Fatalf("status=%d", response.StatusCode)
				}
				wantLimit := defaultActivityLimit
				if tc.raw != "" && tc.raw != "0" {
					wantLimit, _ = strconv.Atoi(tc.raw)
				}
				for range 2 {
					select {
					case limit := <-limits:
						if limit != wantLimit {
							t.Fatalf("inbox limit=%d, want %d", limit, wantLimit)
						}
					default:
						t.Fatal("owned inbox not read")
					}
				}
				var body struct {
					Items []inboxItemJSON `json:"items"`
				}
				if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if len(body.Items) != tc.want {
					t.Fatalf("items=%v, want %d", body.Items, tc.want)
				}
				for i, item := range body.Items {
					if item.ID != fmt.Sprintf("%02d", 5-i) {
						t.Fatalf("not global newest first: %v", body.Items)
					}
				}
			})
		}
	}
}

func TestActivityCollectionBoundsAcrossQueues(t *testing.T) {
	t.Parallel()
	const limit = 3
	out := make([]inboxItemJSON, 0, 2*limit)
	var all []string
	for queue := range 40 {
		var items []*pb.ConsoleInboxItem
		for i := range 17 {
			id := fmt.Sprintf("%04d", i*40+queue)
			items = append(items, &pb.ConsoleInboxItem{Id: id})
			all = append(all, id)
		}
		out = collectActivity(out, items, limit)
		if len(out) > limit || cap(out) > 2*limit {
			t.Fatalf("unbounded collection: len=%d cap=%d", len(out), cap(out))
		}
	}
	slices.Sort(all)
	slices.Reverse(all)
	for i, item := range out {
		if item.ID != all[i] {
			t.Fatalf("item %d = %s, want %s", i, item.ID, all[i])
		}
	}
}

func TestActivityUsesRealNewestInboxWindowLocalAndRemote(t *testing.T) {
	t.Parallel()
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), crypto.Key{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// Production projection writes are exercised here. Corrupt source/projection
	// isolation is tested inside memory, where raw bbolt fixtures are available.
	for _, row := range []struct {
		recipient, id string
		priority      int32
	}{
		{"first", "01", 20}, {"first", "03", 20}, {"first", "05", -20},
		{"second", "02", 20}, {"second", "04", 20}, {"second", "06", -20},
	} {
		raw, err := proto.Marshal(&pb.BotInboxItem{Id: row.id, Recipient: row.recipient, Priority: row.priority})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(memory.BucketBotInbox, row.recipient+":"+row.id, raw); err != nil {
			t.Fatal(err)
		}
	}
	backend := startWebREST(t, nil, func(c *RESTConfig) {
		c.Inbox = memory.NewInboxService(nil, store, 0)
		c.Bots = &memBots{recs: map[string]*pb.BotRecord{
			"first":   {Id: "first", Owner: "user:alice"},
			"second":  {Id: "second", Owner: "user:alice"},
			"private": {Id: "private", Owner: "user:bob"},
		}}
	})
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = testConsoleClient(t, backend) })
	for _, srv := range []*Server{backend, front} {
		for _, limit := range []int{1, 2, 3} {
			response := doJSON(t, http.MethodGet, webBaseURL(srv)+"/v1/activity?limit="+strconv.Itoa(limit), "", http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", response.StatusCode)
			}
			var body struct {
				Items []inboxItemJSON `json:"items"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Items) != limit {
				t.Fatalf("got %d items, want %d", len(body.Items), limit)
			}
			for i, item := range body.Items {
				if item.ID != fmt.Sprintf("%02d", 6-i) {
					t.Fatalf("priority queue used for activity: %v", body.Items)
				}
			}
		}
	}
}

func TestActivityLargeRecordsPreserveDetailAndRemoteMetadata(t *testing.T) {
	t.Parallel()
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), crypto.Key{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	item := &pb.BotInboxItem{Id: "item", Recipient: "worker", Revision: 17, Status: pb.InboxStatus_INBOX_STATUS_FAILED,
		Error: strings.Repeat("error", 14<<10), Body: "private body", Result: strings.Repeat("result", 3000),
		TaskClaims: &pb.Claims{UserId: strings.Repeat("claims", 20000)},
		ToolsUsed:  slices.Repeat([]string{strings.Repeat("tool", 1000)}, 100),
		SessionId:  strings.Repeat("session", 10000), CorrelationId: "correlation", TaskId: "task",
	}
	raw, err := proto.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketBotInbox, "worker:item", raw); err != nil {
		t.Fatal(err)
	}
	backend := startWebREST(t, nil, func(c *RESTConfig) {
		c.Inbox = memory.NewInboxService(nil, store, 0)
		c.Bots = &memBots{recs: map[string]*pb.BotRecord{"worker": {Id: "worker", Owner: "user:alice"}}}
	})
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = testConsoleClient(t, backend) })
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	for _, srv := range []*Server{backend, front} {
		response := doJSON(t, http.MethodGet, webBaseURL(srv)+"/v1/activity?limit=500", "", auth)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("activity status=%d", response.StatusCode)
		}
		var body struct {
			Items []inboxItemJSON `json:"items"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Items) != 1 {
			t.Fatal("missing activity")
		}
		summary := body.Items[0]
		if summary.Revision != item.Revision || summary.DetailPath != "/v1/inbox/worker/item" || summary.TaskID != "task" || summary.CorrelationID != "correlation" || summary.SessionID != "" || summary.Body != "" || len(summary.Error) != memory.InboxActivityTextBytes {
			t.Fatalf("incorrect summary: %+v", summary)
		}
		for _, field := range []string{"error", "result", "tools_used", "session_id"} {
			if !slices.Contains(summary.TruncatedFields, field) {
				t.Fatalf("lost truncation %s", field)
			}
		}
		response = doJSON(t, http.MethodGet, webBaseURL(srv)+summary.DetailPath, "", auth)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("detail status=%d", response.StatusCode)
		}
		var detail inboxItemJSON
		if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
			t.Fatal(err)
		}
		if detail.Error != item.Error || detail.Result != item.Result || detail.SessionID != item.SessionId || detail.Body != item.Body || !slices.Equal(detail.ToolsUsed, item.ToolsUsed) || len(detail.TruncatedFields) != 0 {
			t.Fatal("detail was truncated")
		}
		response = doJSON(t, http.MethodGet, webBaseURL(srv)+summary.DetailPath, "", http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "bob", nil)}})
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-owner detail status=%d", response.StatusCode)
		}
	}
}
