package httpapi

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestSkillsHTTP_BearerMultipartAndLimits(t *testing.T) {
	service := newSDKSkillService()
	handler := NewServer(Deps{Skills: service}, Config{
		RequireAuth: true,
	}).Handler()
	body, contentType := skillMultipart(t, sdkSkillZip(t, "reviewing-code"), false)
	req := httptest.NewRequest(http.MethodPost, "/v1/skills", body)
	req.Header.Set("content-type", contentType)
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("x-api-key-only upload = %d: %s", rec.Code, rec.Body.String())
	}

	body, contentType = skillMultipart(t, sdkSkillZip(t, "reviewing-code"), true)
	req = httptest.NewRequest(http.MethodPost, "/v1/skills", body)
	req.Header.Set("content-type", contentType)
	req.Header.Set("authorization", "Bearer sk-test")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected multipart part = %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/skills", bytes.NewReader(nil))
	req.ContentLength = maxSkillRequestBytes + 1
	rec = httptest.NewRecorder()
	NewServer(Deps{Skills: service}, Config{}).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize Skill request = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSkillsHTTP_DisabledAndListValidation(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/skills", nil)
	rec := httptest.NewRecorder()
	NewServer(Deps{}, Config{}).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("disabled Skills = %d: %s", rec.Code, rec.Body.String())
	}

	handler := NewServer(Deps{Skills: newSDKSkillService()}, Config{}).Handler()
	for _, target := range []string{
		"/v1/skills?source=third_party",
		"/v1/skills?limit=101",
		"/v1/skills?page=not-a-cursor",
		"/v1/skills/skill_sdk/versions?limit=1001",
	} {
		req = httptest.NewRequest(http.MethodGet, target, nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
		}
	}
}

func TestSkillsHTTP_VersionIntegrityMetadataShape(t *testing.T) {
	service := newSDKSkillService()
	now := time.Date(2026, 9, 5, 12, 0, 0, 123000000, time.UTC)
	service.skill = domain.Skill{ID: "skill_sdk", LatestVersion: "1"}
	service.versions["1"] = domain.SkillVersion{
		ID: "1", SkillID: "skill_sdk", Version: "1", CreatedAt: now,
		Name: "reviewing-code", Directory: "reviewing-code",
		Description: "Reviews code.", SizeBytes: 321,
		ChecksumSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	req := httptest.NewRequest(
		http.MethodGet, "/v1/skills/skill_sdk/versions/1", nil,
	)
	rec := httptest.NewRecorder()
	NewServer(Deps{Skills: service}, Config{}).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Get Skill Version = %d: %s", rec.Code, rec.Body.String())
	}
	expected := map[string]any{
		"id": "1", "skill_id": "skill_sdk", "version": "1",
		"type": "skill_version", "created_at": now.Format(timeFmt),
		"name": "reviewing-code", "directory": "reviewing-code",
		"description": "Reviews code.", "size_bytes": float64(321),
		"checksum_sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	assertJSONFields(t, rec.Body.Bytes(), expected)
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response) != len(expected) {
		t.Fatalf("Skill Version fields = %#v", response)
	}
}

func skillMultipart(t *testing.T, archive []byte, extra bool) (*bytes.Buffer, string) {
	t.Helper()
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("files[]", "reviewing-code.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatal(err)
	}
	if extra {
		if err := writer.WriteField("unexpected", "value"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body, writer.FormDataContentType()
}
