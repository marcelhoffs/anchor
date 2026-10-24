package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/marcelhoffs/anchor/internal/i18n"
	"github.com/marcelhoffs/anchor/internal/importexport"
	"github.com/marcelhoffs/anchor/internal/store"
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
		return nil, fmt.Errorf("the file is larger than %d MB: %w", maxImportBody>>20, err)
	}
	if err != nil {
		return nil, err
	}
	if _, err := importexport.Parse(b); err != nil {
		return nil, err
	}
	return b, nil
}

// queueImport starts a background import job into sc's space.
func (s *Server) queueImport(ctx context.Context, sc store.Scope, file []byte) (int64, error) {
	id, err := s.store.CreateJob(ctx, sc, store.JobImport, string(file))
	if err == nil && s.jobs != nil {
		s.jobs.Notify()
	}
	return id, err
}

func (s *Server) importFile(w http.ResponseWriter, r *http.Request) {
	f, _, err := r.FormFile("file")
	if err != nil {
		s.renderData(w, r, http.StatusUnprocessableEntity, data{"Error": i18n.M("Choose a bookmark file to import.")})
		return
	}
	defer f.Close()
	b, err := readImport(f)
	if err != nil {
		var tooBig *http.MaxBytesError
		var version importexport.VersionError
		msg := i18n.M("Couldn't read that file. Try uploading it again.")
		switch {
		case errors.As(err, &tooBig):
			msg = i18n.M("Couldn't import that file: it's larger than %d MB.", maxImportBody>>20)
		case errors.As(err, &version):
			msg = i18n.M("That export comes from a newer version of Anchor. Update this Anchor to import it.")
		case errors.Is(err, importexport.ErrNotAnchorJSON):
			msg = i18n.M("That file isn't a valid Anchor JSON export. Export it from Anchor again.")
		case errors.Is(err, importexport.ErrNotBookmarkFile):
			msg = i18n.M("Couldn't import that file. Export your bookmarks from a browser as HTML, or from Anchor as JSON.")
		}
		s.renderData(w, r, http.StatusUnprocessableEntity, data{"Error": msg})
		return
	}
	if _, err := s.queueImport(r.Context(), currentScope(r), b); !s.storeError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/settings/data", http.StatusSeeOther)
}

func (s *Server) exportFile(w http.ResponseWriter, r *http.Request) {
	s.writeExport(w, r, currentScope(r), r.URL.Query().Get("format"))
}

// writeExport streams the space's bookmarks as html (default) or json.
func (s *Server) writeExport(w http.ResponseWriter, r *http.Request, sc store.Scope, format string) {
	if format != "json" {
		format = "html"
	}
	name := "anchor-" + time.Now().UTC().Format("20060102") + "." + format
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
		err = s.store.ExportBookmarks(r.Context(), sc, out.Write)
	}
	if err == nil {
		err = out.Close()
	}
	if err != nil {
		// Headers are gone; all we can do is log and cut the response short.
		s.log.Error("export", "user", sc.UserID(), "space", sc.SpaceID(), "err", err)
		return
	}
}
