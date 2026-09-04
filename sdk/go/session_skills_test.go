package mango

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareSessionSkillsMaterializesPrimaryAndRosterPins(t *testing.T) {
	t.Parallel()
	archives := map[string][]byte{
		"skill_primary@100": sessionSkillArchive(t, "primary_tools", map[string]string{
			"SKILL.md":       "---\nname: primary-tools\ndescription: Primary\n---\nbody\n",
			"scripts/run.sh": "#!/bin/sh\nprintf primary\n",
		}),
		"skill_child@200": sessionSkillArchive(t, "child-tools", map[string]string{
			"SKILL.md": "---\nname: child-tools\ndescription: Child\n---\nbody\n",
		}),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		if len(parts) < 5 || parts[0] != "v1" || parts[1] != "skills" || parts[3] != "versions" {
			http.NotFound(w, request)
			return
		}
		skillID, version := parts[2], parts[4]
		key := skillID + "@" + version
		archive, ok := archives[key]
		if !ok {
			http.NotFound(w, request)
			return
		}
		directory := "primary_tools"
		name := "primary-tools"
		if skillID == "skill_child" {
			directory, name = "child-tools", "child-tools"
		}
		if len(parts) == 6 && parts[5] == "content" {
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(archive)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": version, "skill_id": skillID, "version": version,
			"name": name, "directory": directory,
			"size_bytes": len(archive), "checksum_sha256": fmt.Sprintf("%x", sha256.Sum256(archive)),
		})
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, APIKey: "session-token"})
	if err != nil {
		t.Fatal(err)
	}
	primary := SkillReferenceResponse{ResolvedSkillReference: &ResolvedSkillReference{
		Type: "custom", SkillID: "skill_primary", Version: "100",
	}}
	child := SkillReferenceResponse{ResolvedSkillReference: &ResolvedSkillReference{
		Type: "custom", SkillID: "skill_child", Version: "200",
	}}
	session := Session{Agent: AgentSnapshot{
		ID: "agent_primary", Version: 1, Skills: []SkillReferenceResponse{primary},
		Multiagent: SessionMultiagent{SessionResolvedMultiagent: &SessionResolvedMultiagent{
			Type: "coordinator",
			Agents: []SessionThreadAgent{{ManagedAgentThreadAgent: &ManagedAgentThreadAgent{
				ID: "agent_child", Version: 2, Type: "agent",
				Skills: []SkillReferenceResponse{child},
			}}},
		}},
	}}
	workdir := t.TempDir()
	setup, err := PrepareSessionSkills(context.Background(), client, session, workdir)
	if err != nil {
		t.Fatal(err)
	}
	primaryScript := filepath.Join(workdir, "skills", "primary-tools", "scripts", "run.sh")
	info, err := os.Stat(primaryScript)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("downloaded script mode = %o, want executable", info.Mode().Perm())
	}
	agentScopes, err := filepath.Glob(filepath.Join(workdir, "skills", ".agents", "*", "child-tools", "SKILL.md"))
	if err != nil || len(agentScopes) != 1 {
		t.Fatalf("roster Skill paths = %v, %v", agentScopes, err)
	}
	if err := setup.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "skills", "primary-tools")); !os.IsNotExist(err) {
		t.Fatalf("primary Skill survived cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(agentScopes[0])); !os.IsNotExist(err) {
		t.Fatalf("roster Skill survived cleanup: %v", err)
	}
}

func TestPrepareSessionSkillsRejectsArchiveEscapeAndCleansPriorSkill(t *testing.T) {
	t.Parallel()
	good := sessionSkillArchive(t, "good", map[string]string{"SKILL.md": "body"})
	var hostile bytes.Buffer
	writer := zip.NewWriter(&hostile)
	entry, err := writer.Create("bad/../escape")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("escape"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		skillID, version := parts[2], parts[4]
		if len(parts) == 6 {
			if skillID == "skill_good" {
				_, _ = w.Write(good)
			} else {
				_, _ = w.Write(hostile.Bytes())
			}
			return
		}
		name := strings.TrimPrefix(skillID, "skill_")
		archive := hostile.Bytes()
		if skillID == "skill_good" {
			archive = good
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": version, "skill_id": skillID, "version": version,
			"name": name, "directory": name,
			"size_bytes": len(archive), "checksum_sha256": fmt.Sprintf("%x", sha256.Sum256(archive)),
		})
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ref := func(id string) SkillReferenceResponse {
		return SkillReferenceResponse{ResolvedSkillReference: &ResolvedSkillReference{
			Type: "custom", SkillID: id, Version: "1",
		}}
	}
	workdir := t.TempDir()
	_, err = PrepareSessionSkills(context.Background(), client, Session{Agent: AgentSnapshot{
		Skills: []SkillReferenceResponse{ref("skill_good"), ref("skill_bad")},
	}}, workdir)
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("archive escape error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workdir, "skills", "good")); !os.IsNotExist(statErr) {
		t.Fatalf("prior Skill survived failed setup: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(workdir, "escape")); !os.IsNotExist(statErr) {
		t.Fatalf("archive escaped workdir: %v", statErr)
	}
}

func TestPrepareSessionSkillsDoesNotFollowPriorOrReplacementRootSymlink(t *testing.T) {
	t.Parallel()
	archive := sessionSkillArchive(t, "safe", map[string]string{"SKILL.md": "body"})
	server := singleSessionSkillServer(t, "skill_safe", "1", "safe", "safe", archive, "")
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workdir, "skills")); err != nil {
		t.Fatal(err)
	}
	session := Session{Agent: AgentSnapshot{Skills: []SkillReferenceResponse{{
		ResolvedSkillReference: &ResolvedSkillReference{
			Type: "custom", SkillID: "skill_safe", Version: "1",
		},
	}}}}
	setup, err := PrepareSessionSkills(context.Background(), client, session, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("publishing followed prior root symlink: %v", err)
	}
	rootInfo, err := os.Lstat(filepath.Join(workdir, "skills"))
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("published root = %#v, %v", rootInfo, err)
	}
	if err := os.RemoveAll(filepath.Join(workdir, "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workdir, "skills")); err != nil {
		t.Fatal(err)
	}
	if err := setup.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("cleanup followed replacement root symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(workdir, "skills")); !os.IsNotExist(err) {
		t.Fatalf("replacement root symlink survived cleanup: %v", err)
	}
}

func TestPrepareSessionSkillsRejectsIntegrityMismatch(t *testing.T) {
	t.Parallel()
	archive := sessionSkillArchive(t, "safe", map[string]string{"SKILL.md": "body"})
	server := singleSessionSkillServer(
		t, "skill_safe", "1", "safe", "safe", archive, strings.Repeat("0", 64),
	)
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = PrepareSessionSkills(context.Background(), client, Session{Agent: AgentSnapshot{
		Skills: []SkillReferenceResponse{{ResolvedSkillReference: &ResolvedSkillReference{
			Type: "custom", SkillID: "skill_safe", Version: "1",
		}}},
	}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("integrity error = %v", err)
	}
}

func TestPrepareSessionSkillsAppliesAggregateExpandedAndFileBudgets(t *testing.T) {
	t.Parallel()
	archive := sessionSkillArchive(t, "safe", map[string]string{
		"SKILL.md": strings.Repeat("a", 1000), "notes.txt": strings.Repeat("b", 1000),
	})
	server := singleSessionSkillServer(t, "skill_safe", "1", "safe", "safe", archive, "")
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	session := Session{Agent: AgentSnapshot{Skills: []SkillReferenceResponse{{
		ResolvedSkillReference: &ResolvedSkillReference{
			Type: "custom", SkillID: "skill_safe", Version: "1",
		},
	}}}}
	base := sessionSkillBudget{
		archiveLimit: 1 << 20, fileLimit: 100, countLimit: 10,
		totalByteLimit: 1 << 20, totalFileLimit: 100,
	}
	byteBudget := base
	byteBudget.totalByteLimit = 500
	if _, err := prepareSessionSkills(
		context.Background(), client, session, t.TempDir(), &byteBudget,
	); err == nil || !strings.Contains(err.Error(), "expanded-size") {
		t.Fatalf("expanded budget error = %v", err)
	}
	fileBudget := base
	fileBudget.totalFileLimit = 1
	if _, err := prepareSessionSkills(
		context.Background(), client, session, t.TempDir(), &fileBudget,
	); err == nil || !strings.Contains(err.Error(), "file limit") {
		t.Fatalf("file budget error = %v", err)
	}
}

func singleSessionSkillServer(
	t *testing.T,
	skillID, version, name, directory string,
	archive []byte,
	checksum string,
) *httptest.Server {
	t.Helper()
	if checksum == "" {
		checksum = fmt.Sprintf("%x", sha256.Sum256(archive))
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/skills/"+skillID+"/versions/"+version+"/content" {
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(archive)
			return
		}
		if request.URL.Path == "/v1/skills/"+skillID+"/versions/"+version {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": version, "skill_id": skillID, "version": version,
				"name": name, "directory": directory,
				"size_bytes": len(archive), "checksum_sha256": checksum,
			})
			return
		}
		http.NotFound(w, request)
	}))
}

func sessionSkillArchive(t *testing.T, root string, files map[string]string) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	for name, content := range files {
		header := &zip.FileHeader{Name: root + "/" + name, Method: zip.Deflate}
		header.SetMode(0o644)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprint(entry, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}
