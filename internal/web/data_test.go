package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

const importHTML = `<!DOCTYPE NETSCAPE-Bookmark-file-1>
<DL><p>
<DT><H3>Reading</H3>
<DL><p>
<DT><A HREF="https://go.dev/doc/effective_go" ADD_DATE="1700000000">Effective Go</A>
<DD>Tips for writing clear Go
<DT><A HREF="https://sqlite.org/fts5.html" TAGS="sqlite">FTS5</A>
</DL><p>
<DT><A HREF="javascript:void(0)">Bookmarklet</A>
</DL><p>
`

func TestImportExport(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("csrf", c.csrf())
	fw, _ := mw.CreateFormFile("file", "bookmarks.html")
	fw.Write([]byte(importHTML))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/settings/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("upload: %d", res.StatusCode)
	}
	if _, body, _ := c.get("/settings/data"); !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("data page doesn't refresh while the import is queued")
	}
	if !e.worker.RunOnce(context.Background()) {
		t.Fatal("no job to run")
	}
	_, body, _ := c.get("/settings/data")
	if !strings.Contains(body, "2 added, 0 merged") || !strings.Contains(body, "1 skipped") {
		t.Fatalf("import summary missing:\n%s", body)
	}
	_, body, _ = c.get("/?q=%23Reading")
	if !strings.Contains(body, "Effective Go") || !strings.Contains(body, "FTS5") {
		t.Fatal("imported bookmarks not tagged with their folder")
	}

	res, err = c.http.Get(e.srv.URL + "/settings/export?format=json")
	if err != nil {
		t.Fatal(err)
	}
	exported, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(res.Header.Get("Content-Disposition"), ".json") || !strings.Contains(string(exported), `"Tips for writing clear Go"`) {
		t.Fatalf("export: %q\n%s", res.Header.Get("Content-Disposition"), exported)
	}

	// Bad files fail at upload, not later.
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	mw.WriteField("csrf", c.csrf())
	fw, _ = mw.CreateFormFile("file", "notes.txt")
	fw.Write([]byte("just some text"))
	mw.Close()
	req, _ = http.NewRequest(http.MethodPost, e.srv.URL+"/settings/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, _ = c.http.Do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad file: %d", res.StatusCode)
	}

	// The API imports too, and reports the job.
	a := apiClient{t, e.srv.URL, apiToken(t, c, "write")}
	req, _ = http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/import", strings.NewReader(importHTML))
	req.Header.Set("Authorization", "Bearer "+a.token)
	res, _ = http.DefaultClient.Do(req)
	var job map[string]any
	json.NewDecoder(res.Body).Decode(&job)
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("api import: %d", res.StatusCode)
	}
	e.worker.RunOnce(context.Background())
	code, status := a.do("GET", "/jobs/"+itoa(int64(job["job_id"].(float64))), nil)
	result, _ := status["result"].(map[string]any)
	if code != 200 || status["status"] != "done" || result["merged"] != float64(2) {
		t.Fatalf("api job: %d %v", code, status)
	}
}
