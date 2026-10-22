package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/marcelhoffs/burrow/internal/importexport"
	"github.com/marcelhoffs/burrow/internal/store"
)

type jobRow struct {
	store.Job
	Summary store.ImportResult
	Percent int
}

func (s *Server) dataPage(w http.ResponseWriter, r *http.Request) {
	s.renderData(w, r, http.StatusOK, data{})
}

func (s *Server) renderData(w http.ResponseWriter, r *http.Request, status int, d data) {
	jobs, err := s.store.ListJobs(r.Context(), currentUser(r).ID, 10)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	rows := make([]jobRow, len(jobs))
	running := false
	for i, j := range jobs {
		rows[i] = jobRow{Job: j}
		json.Unmarshal([]byte(j.Result), &rows[i].Summary)
		if j.Total > 0 {
			rows[i].Percent = j.Progress * 100 / j.Total
		}
		running = running || !j.Finished()
	}
	d["Tab"] = "data"
	d["Jobs"] = rows
	d["Refresh"] = running // the page reloads itself while an import runs
	s.render(w, r, status, "data", d)
}

// readImport returns the uploaded file, checked to be a bookmark file we can
// parse, so a wrong upload fails right away rather than in the background.
func readImport(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(r)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return nil, fmt.Errorf("the file is larger than %d MB", maxImportBody>>20)
	}
	if err != nil {
		return nil, err
	}
	if _, err := importexport.Parse(b); err != nil {
		return nil, err
	}
	return b, nil
}

// queueImport starts a background import job.
func (s *Server) queueImport(ctx context.Context, userID int64, file []byte) (int64, error) {
	id, err := s.store.CreateJob(ctx, userID, store.JobImport, string(file))
	if err == nil && s.jobs != nil {
		s.jobs.Notify()
	}
	return id, err
}

func (s *Server) importFile(w http.ResponseWriter, r *http.Request) {
	f, _, err := r.FormFile("file")
	if err != nil {
		s.renderData(w, r, http.StatusUnprocessableEntity, data{"Error": "Choose a bookmark file to import."})
		return
	}
	defer f.Close()
	b, err := readImport(f)
	if err != nil {
		s.renderData(w, r, http.StatusUnprocessableEntity, data{"Error": "Couldn't import that file: " + err.Error() + "."})
		return
	}
	if _, err := s.queueImport(r.Context(), currentUser(r).ID, b); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/data", http.StatusSeeOther)
}

func (s *Server) exportFile(w http.ResponseWriter, r *http.Request) {
	s.writeExport(w, r, currentUser(r).ID, r.URL.Query().Get("format"))
}

// writeExport streams the user's bookmarks as html (default) or json.
func (s *Server) writeExport(w http.ResponseWriter, r *http.Request, userID int64, format string) {
	if format != "json" {
		format = "html"
	}
	name := "burrow-" + time.Now().UTC().Format("20060102") + "." + format
	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")

	var out importexport.Writer
	var err error
	if format == "json" {
		out, err = importexport.NewJSONWriter(w, time.Now())
	} else {
		out, err = importexport.NewNetscapeWriter(w)
	}
	if err == nil {
		err = s.store.ExportBookmarks(r.Context(), userID, out.Write)
	}
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		// Headers are gone; all we can do is log and cut the response short.
		s.log.Error("export", "user", userID, "err", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: userID, Event: store.EventExport, Detail: format})
}
