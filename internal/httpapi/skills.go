package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func (s *Server) createSkill(w http.ResponseWriter, r *http.Request) {
	if s.deps.Skills == nil {
		writeError(w, domain.Unsupported("Skills API is not configured"))
		return
	}
	files, title, err := readSkillMultipart(r, true)
	if err != nil {
		writeSkillMultipartError(w, err)
		return
	}
	created, err := s.deps.Skills.Create(r.Context(), app.SkillCreateInput{
		DisplayTitle: title, Files: files,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, skillToJSON(created))
}

func (s *Server) getSkill(w http.ResponseWriter, r *http.Request) {
	if s.deps.Skills == nil {
		writeError(w, domain.Unsupported("Skills API is not configured"))
		return
	}
	item, err := s.deps.Skills.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, skillToJSON(item))
}

func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	if s.deps.Skills == nil {
		writeError(w, domain.Unsupported("Skills API is not configured"))
		return
	}
	query, filter, err := parseSkillListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := s.deps.Skills.List(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(page.Skills))
	for _, item := range page.Skills {
		data = append(data, skillToJSON(item))
	}
	var nextPage any
	if page.HasNext && len(page.Skills) > 0 {
		last := page.Skills[len(page.Skills)-1]
		nextPage = encodeResourceCursor(resourceCursor{
			Kind: skillListCursorKind, CreatedAt: last.CreatedAt.Format(timeFmt),
			ID: last.ID, Filter: filter.fingerprint(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data, "has_more": page.HasNext, "next_page": nextPage,
	})
}

func (s *Server) deleteSkill(w http.ResponseWriter, r *http.Request) {
	if s.deps.Skills == nil {
		writeError(w, domain.Unsupported("Skills API is not configured"))
		return
	}
	item, err := s.deps.Skills.Delete(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": item.ID, "type": "skill_deleted"})
}

func (s *Server) createSkillVersion(w http.ResponseWriter, r *http.Request) {
	if s.deps.Skills == nil {
		writeError(w, domain.Unsupported("Skills API is not configured"))
		return
	}
	files, _, err := readSkillMultipart(r, false)
	if err != nil {
		writeSkillMultipartError(w, err)
		return
	}
	item, err := s.deps.Skills.CreateVersion(r.Context(), r.PathValue("id"), files)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, skillVersionToJSON(item))
}

func (s *Server) getSkillVersion(w http.ResponseWriter, r *http.Request) {
	if s.deps.Skills == nil {
		writeError(w, domain.Unsupported("Skills API is not configured"))
		return
	}
	item, err := s.deps.Skills.GetVersion(
		r.Context(), r.PathValue("id"), r.PathValue("version"),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, skillVersionToJSON(item))
}

func (s *Server) listSkillVersions(w http.ResponseWriter, r *http.Request) {
	if s.deps.Skills == nil {
		writeError(w, domain.Unsupported("Skills API is not configured"))
		return
	}
	skillID := r.PathValue("id")
	query, filter, err := parseSkillVersionListQuery(r, skillID)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := s.deps.Skills.ListVersions(r.Context(), skillID, query)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(page.Versions))
	for _, item := range page.Versions {
		data = append(data, skillVersionToJSON(item))
	}
	var nextPage any
	if page.HasNext && len(page.Versions) > 0 {
		last := page.Versions[len(page.Versions)-1]
		nextPage = encodeResourceCursor(resourceCursor{
			Kind: skillVersionListCursorKind, CreatedAt: last.CreatedAt.Format(timeFmt),
			ID: last.Version, Filter: filter.fingerprint(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data, "has_more": page.HasNext, "next_page": nextPage,
	})
}

func (s *Server) deleteSkillVersion(w http.ResponseWriter, r *http.Request) {
	if s.deps.Skills == nil {
		writeError(w, domain.Unsupported("Skills API is not configured"))
		return
	}
	item, err := s.deps.Skills.DeleteVersion(
		r.Context(), r.PathValue("id"), r.PathValue("version"),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": item.Version, "type": "skill_version_deleted",
	})
}

func (s *Server) downloadSkillVersion(w http.ResponseWriter, r *http.Request) {
	if s.deps.Skills == nil {
		writeError(w, domain.Unsupported("Skills API is not configured"))
		return
	}
	download, err := s.deps.Skills.Download(
		r.Context(), r.PathValue("id"), r.PathValue("version"),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	defer download.Body.Close() //nolint:errcheck // response copy owns completion
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.FormatInt(download.Version.SizeBytes, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": download.Version.Directory + "-" + download.Version.Version + ".zip",
	}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, download.Body)
}

func readSkillMultipart(
	r *http.Request,
	allowDisplayTitle bool,
) ([]app.SkillUploadFile, *string, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, nil, domain.Validation("invalid multipart body")
	}
	files := make([]app.SkillUploadFile, 0)
	var displayTitle *string
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, nil, skillMultipartReadError(nextErr)
		}
		disposition, params, parseErr := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if parseErr != nil || disposition != "form-data" {
			_ = part.Close()
			return nil, nil, domain.Validation("invalid Skill multipart body")
		}
		name := params["name"]
		filename := params["filename"]
		data, readErr := io.ReadAll(part)
		closeErr := part.Close()
		if readErr != nil {
			return nil, nil, skillMultipartReadError(readErr)
		}
		if closeErr != nil {
			return nil, nil, skillMultipartReadError(closeErr)
		}
		switch name {
		case "files", "files[]":
			if filename == "" {
				return nil, nil, domain.Validation("Skill file part is missing a filename")
			}
			files = append(files, app.SkillUploadFile{Filename: filename, Body: data})
			if len(files) > app.MaxSkillFiles {
				return nil, nil, domain.Validation("Skill bundle contains too many files")
			}
		case "display_title":
			if !allowDisplayTitle || filename != "" || displayTitle != nil {
				return nil, nil, domain.Validation("invalid display_title part")
			}
			value := string(data)
			displayTitle = &value
		default:
			return nil, nil, domain.Validation("Skill multipart body contains an unexpected part")
		}
	}
	if len(files) == 0 {
		return nil, nil, domain.Validation("files must contain a Skill bundle")
	}
	return files, displayTitle, nil
}

func skillMultipartReadError(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return err
	}
	return domain.Validation("invalid Skill multipart body")
}

func parseSkillListQuery(r *http.Request) (app.SkillListQuery, skillCursorFilter, error) {
	values := r.URL.Query()
	query := app.SkillListQuery{Limit: app.DefaultSkillListLimit, Source: values.Get("source")}
	filter := skillCursorFilter{Source: query.Source}
	if values.Has("limit") {
		limit, err := parseResourceListLimit(values.Get("limit"))
		if err != nil || limit > app.MaxSkillListLimit {
			return app.SkillListQuery{}, skillCursorFilter{},
				domain.Validation("limit must be between 1 and 100")
		}
		query.Limit = limit
	}
	if query.Source != "" && query.Source != "custom" && query.Source != "anthropic" {
		return app.SkillListQuery{}, skillCursorFilter{},
			domain.Validation("source must be custom or anthropic")
	}
	if values.Has("page") {
		cursor, ok := decodeResourceCursor(values.Get("page"), skillListCursorKind)
		if !ok || cursor.Filter != filter.fingerprint() {
			return app.SkillListQuery{}, skillCursorFilter{}, domain.Validation("invalid page cursor")
		}
		createdAt, ok := parseTimeParam(cursor.CreatedAt)
		if !ok {
			return app.SkillListQuery{}, skillCursorFilter{}, domain.Validation("invalid page cursor")
		}
		query.After = &app.ResourcePageBoundary{CreatedAt: createdAt.UTC(), ID: cursor.ID}
	}
	return query, filter, nil
}

func parseSkillVersionListQuery(
	r *http.Request,
	skillID string,
) (app.SkillVersionListQuery, skillVersionCursorFilter, error) {
	values := r.URL.Query()
	query := app.SkillVersionListQuery{Limit: app.DefaultSkillVersionListLimit}
	filter := skillVersionCursorFilter{SkillID: skillID}
	if values.Has("limit") {
		limit, err := parseResourceListLimit(values.Get("limit"))
		if err != nil || limit > app.MaxSkillVersionListLimit {
			return app.SkillVersionListQuery{}, skillVersionCursorFilter{},
				domain.Validation("limit must be between 1 and 1000")
		}
		query.Limit = limit
	}
	if values.Has("page") {
		cursor, ok := decodeResourceCursor(values.Get("page"), skillVersionListCursorKind)
		if !ok || cursor.Filter != filter.fingerprint() {
			return app.SkillVersionListQuery{}, skillVersionCursorFilter{},
				domain.Validation("invalid page cursor")
		}
		createdAt, ok := parseTimeParam(cursor.CreatedAt)
		if !ok || strings.TrimSpace(cursor.ID) == "" {
			return app.SkillVersionListQuery{}, skillVersionCursorFilter{},
				domain.Validation("invalid page cursor")
		}
		query.After = &app.ResourcePageBoundary{CreatedAt: createdAt.UTC(), ID: cursor.ID}
	}
	return query, filter, nil
}

func skillToJSON(item domain.Skill) map[string]any {
	var latest any
	if item.LatestVersion != "" {
		latest = item.LatestVersion
	}
	return map[string]any{
		"id": item.ID, "created_at": item.CreatedAt.Format(timeFmt),
		"display_title": item.DisplayTitle, "latest_version": latest,
		"source": item.Source, "type": "skill", "updated_at": item.UpdatedAt.Format(timeFmt),
	}
}

func skillVersionToJSON(item domain.SkillVersion) map[string]any {
	return map[string]any{
		"id": item.ID, "created_at": item.CreatedAt.Format(timeFmt),
		"checksum_sha256": item.ChecksumSHA256,
		"description": item.Description, "directory": item.Directory,
		"name": item.Name, "skill_id": item.SkillID,
		"size_bytes": item.SizeBytes,
		"type": "skill_version", "version": item.Version,
	}
}

func writeSkillMultipartError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		writeError(w, domain.TooLarge("Skill upload must be smaller than 30 MB"))
		return
	}
	writeError(w, err)
}
