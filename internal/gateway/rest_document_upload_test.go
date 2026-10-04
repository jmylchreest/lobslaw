package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestDocumentUploadsKeepSafeNamesAndUseCanonicalPaths(t *testing.T) {
	for _, media := range []string{"application/pdf", "text/plain", "text/csv", "text/markdown", "application/json", "application/xml", "application/yaml"} {
		t.Run(media, func(t *testing.T) {
			server, _ := mediaServer(t)
			request := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader("file bytes"))
			request.Header.Set("Authorization", "Bearer "+mintJWTForUser(t, "alice"))
			request.Header.Set("Content-Type", media)
			request.Header.Set("X-Upload-Name", url.PathEscape("../../<untrusted>report.txt"))
			response := httptest.NewRecorder()
			server.handleUpload(response, request)
			id := uploadID(t, response)
			var result uploadResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Filename != "untrustedreport.txt" {
				t.Fatal(result.Filename)
			}
			attachments, release, err := server.uploads.acquire("alice", []string{id}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if attachments[0].Kind != types.AttachmentDocument || strings.Contains(attachments[0].LocalPath, "report") {
				t.Fatal("client name selected a path")
			}
		})
	}
}

func TestBotChatReceivesOwnerBoundFilesAndAttachmentOnlyMessage(t *testing.T) {
	runner := &captureRunner{}
	server := startWebREST(t, runner, func(c *RESTConfig) {
		c.IncomingDir = t.TempDir()
		c.Bots = stubBots{rec: &pb.BotRecord{Id: "chief", Owner: "user:alice", Enabled: true}}
	})
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}, "Content-Type": {"text/plain"}}
	response := doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/uploads", "Please read this note", auth)
	var upload uploadResponse
	if err := json.NewDecoder(response.Body).Decode(&upload); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatal(response.StatusCode)
	}
	for _, user := range []string{"bob", "alice@idp"} {
		response = doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/bots/chief/messages", `{"upload_ids":["`+upload.UploadID+`"]}`, http.Header{"Authorization": {"Bearer " + mintJWTWith(t, user, nil)}})
		if user == "bob" && response.StatusCode != 404 && response.StatusCode != 403 {
			t.Fatal("foreign upload accepted")
		}
		if user == "alice@idp" && response.StatusCode != 200 {
			t.Fatal(response.StatusCode)
		}
		_ = response.Body.Close()
	}
	if got := runner.lastRequest(); len(got.Attachments) != 1 || got.Attachments[0].Reference != upload.UploadID || got.Message != "Please examine the attached files." {
		t.Fatalf("files missing from bot request: %+v", got)
	}
}

func TestRetainedAttachmentsStayPinnedAndReplayAfterUploadExpiry(t *testing.T) {
	runner := &reconnectRunner{started: make(chan struct{}, 1), release: make(chan struct{}), cancelled: make(chan struct{})}
	server := startWebREST(t, runner, func(c *RESTConfig) {
		c.IncomingDir = t.TempDir()
		c.Bots = stubBots{rec: &pb.BotRecord{Id: "chief", Owner: "user:alice", Enabled: true}}
	})
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}, "Content-Type": {"text/plain"}, "X-Upload-Name": {"notes.txt"}}
	response := doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/uploads", "Attached note", auth)
	var upload uploadResponse
	if err := json.NewDecoder(response.Body).Decode(&upload); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	body := `{"id":"with-files","bot":"chief","upload_ids":["` + upload.UploadID + `"]}`
	response = doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/chat-turns", body, auth)
	if response.StatusCode != 202 {
		t.Fatal(response.StatusCode)
	}
	retained := readChatTurn(t, response)
	if len(retained.Files) != 1 || retained.Files[0].Name != "notes.txt" {
		t.Fatal("file metadata was not retained")
	}
	<-runner.started
	response = doJSON(t, http.MethodDelete, webBaseURL(server)+"/v1/uploads/"+upload.UploadID, "", auth)
	if response.StatusCode != 409 {
		t.Fatal("active upload was discarded")
	}
	_ = response.Body.Close()
	server.uploads.mu.Lock()
	server.uploads.entries[upload.UploadID].expires = time.Now().Add(-time.Hour)
	server.uploads.mu.Unlock()
	server.uploads.sweep(time.Now())
	server.uploads.mu.Lock()
	pinned := server.uploads.entries[upload.UploadID] != nil
	server.uploads.mu.Unlock()
	if !pinned {
		t.Fatal("expiry cleanup removed a running turn's file")
	}
	close(runner.release)
	awaitChatTurn(t, server, "with-files", "completed", auth)
	server.chatTurns.wg.Wait()
	response = doJSON(t, http.MethodPost, webBaseURL(server)+"/v1/chat-turns", body, auth)
	if response.StatusCode != 202 || readChatTurn(t, response).State != "completed" || runner.calls.Load() != 1 {
		t.Fatal("expired upload prevented idempotent recovery or ran twice")
	}
}

func TestDiscardUploadEnforcesOwnershipAndActivePins(t *testing.T) {
	server, _ := mediaServer(t)
	id := uploadID(t, uploadRequest(t, server, "alice", "text/plain", strings.NewReader("note")))
	remove := func(user string) int {
		r := httptest.NewRequest(http.MethodDelete, "/v1/uploads/"+id, nil)
		r.Header.Set("Authorization", "Bearer "+mintJWTForUser(t, user))
		w := httptest.NewRecorder()
		server.handleUploadDelete(w, r)
		return w.Code
	}
	if remove("bob") != 404 {
		t.Fatal("cross-owner deletion")
	}
	_, release, err := server.uploads.acquire("alice", []string{id}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if remove("alice") != 409 {
		t.Fatal("deleted a pinned file")
	}
	release()
	if remove("alice") != 204 {
		t.Fatal("discard failed")
	}
	if _, _, err := server.uploads.acquire("alice", []string{id}, time.Now()); err == nil {
		t.Fatal("deleted file is still available")
	}
}
