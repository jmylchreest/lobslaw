package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type activityTestInbox struct {
	InboxAPI
	t      *testing.T
	limits chan int
}

func (s activityTestInbox) Recipients(context.Context) ([]string, error) {
	return []string{"first", "second", "private"}, nil
}

func (s activityTestInbox) List(_ context.Context, recipient string, f memory.InboxFilter) ([]*pb.BotInboxItem, error) {
	select {
	case s.limits <- f.Limit:
	default:
		s.t.Error("unexpected extra inbox read")
	}
	if f.Limit < 1 || f.Limit > maxActivityLimit {
		s.t.Errorf("unbounded inbox limit: %d", f.Limit)
	}
	if recipient == "private" {
		s.t.Error("read another owner's inbox")
	}
	ids := []string{"02", "04"}
	if recipient == "second" {
		ids = []string{"03", "01", "05"}
	}
	out := make([]*pb.BotInboxItem, 0, len(ids))
	for _, id := range ids {
		out = append(out, &pb.BotInboxItem{Id: id, Recipient: recipient})
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
		var items []*pb.BotInboxItem
		for i := range 17 {
			id := fmt.Sprintf("%04d", i*40+queue)
			items = append(items, &pb.BotInboxItem{Id: id})
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
