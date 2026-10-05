package kubernetes_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

type objectSnapshot struct {
	key, media string
	body       []byte
}

func (s *stateFixture) quiesce() {
	c := s.cluster
	c.t.Helper()
	c.kube(nil, "-n", s.namespace, "scale", "deployment/"+s.namespace+"-api", "deployment/"+s.namespace+"-orchestrator", "--replicas=0")
	c.await(90*time.Second, func() bool {
		raw := c.kube(nil, "-n", s.namespace, "get", "pods", "-l", "app.kubernetes.io/instance="+s.namespace, "-o", "json")
		var page struct{ Items []json.RawMessage }
		require.NoError(c.t, json.Unmarshal(raw, &page))
		return len(page.Items) == 0
	})
	s.compose(nil, "stop", "--timeout", "60", "temporal")
	// Compose stop must have completed before either database dump begins.
	id := string(bytes.TrimSpace(s.compose(nil, "ps", "--all", "-q", "temporal")))
	require.Equal(c.t, "false", string(bytes.TrimSpace(c.command(nil, nil, "docker", "inspect", "--format", "{{.State.Running}}", id))))
}

func (s *stateFixture) snapshotObjects() []objectSnapshot {
	c := s.cluster
	c.t.Helper()
	var result []objectSnapshot
	pages := s3.NewListObjectsV2Paginator(s.s3, &s3.ListObjectsV2Input{Bucket: aws.String("mango-alpha")})
	for pages.HasMorePages() {
		page, err := pages.NextPage(c.ctx)
		require.NoError(c.t, err)
		for _, object := range page.Contents {
			got, err := s.s3.GetObject(c.ctx, &s3.GetObjectInput{Bucket: aws.String("mango-alpha"), Key: object.Key})
			require.NoError(c.t, err)
			body, err := io.ReadAll(got.Body)
			require.NoError(c.t, err)
			require.NoError(c.t, got.Body.Close())
			result = append(result, objectSnapshot{key: aws.ToString(object.Key), media: aws.ToString(got.ContentType), body: body})
		}
	}
	require.GreaterOrEqual(c.t, len(result), 2, "backup must contain both File and Skill bytes")
	return result
}

func (s *stateFixture) verifyQuiescedRestore(evidence journeyEvidence) {
	c, t := s.cluster, s.cluster.t
	t.Helper()
	t.Log("Preparing original custom-tool wait and immutable recovery evidence")
	filePath := "/v1/files/" + evidence.fileID
	fileBefore := s.json("GET", filePath, nil, 200)
	skillPath := "/v1/skills/" + evidence.skillID + "/versions/" + evidence.skillVersion
	skillBefore := s.json("GET", skillPath, nil, 200)
	skillBytes := s.request("GET", skillPath+"/content", "", nil, 200, true)
	skillSum := sha256.Sum256(skillBytes)
	require.Equal(t, fmt.Sprintf("%x", skillSum), skillBefore["checksum_sha256"])
	fileSum := sha256.Sum256([]byte("alpha-file-bytes"))
	require.Equal(t, fmt.Sprintf("%x", fileSum), fileBefore["checksum_sha256"])
	memoryPath := "/v1/memory_stores/" + evidence.storeID
	memoryBefore := s.json("GET", memoryPath+"/memories?view=full", nil, 200)
	versionsBefore := s.json("GET", memoryPath+"/memory_versions?view=full", nil, 200)
	require.GreaterOrEqual(t, len(versionsBefore["data"].([]any)), 2)
	require.Equal(t, false, versionsBefore["has_more"])
	vault := s.json("POST", "/v1/vaults", map[string]any{"display_name": "alpha-restore-vault"}, 200)
	vaultPath := "/v1/vaults/" + vault["id"].(string)
	credential := s.json("POST", vaultPath+"/credentials", map[string]any{"auth": map[string]any{
		"type": "static_bearer", "token": "fixture-vault-secret-only", "mcp_server_url": "https://fixture.example.invalid/mcp"}}, 200)
	credentialPath := vaultPath + "/credentials/" + credential["id"].(string)
	credentialBefore := s.json("GET", credentialPath, nil, 200)
	public, err := json.Marshal(credentialBefore)
	require.NoError(t, err)
	require.NotContains(t, string(public), "fixture-vault-secret-only")
	agent := s.json("POST", "/v1/agents", map[string]any{"name": "alpha-checkpoint", "model": "alpha-fixture",
		"tools": []any{map[string]any{"type": "custom", "name": "checkpoint", "description": "Record a test checkpoint.",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"label": map[string]any{"type": "string"}}, "required": []any{"label"}}}}}, 200)
	session := s.json("POST", "/v1/sessions", map[string]any{"agent": agent["id"], "environment_id": evidence.environment}, 200)
	sessionID := session["id"].(string)
	hash := sha256.Sum256([]byte(sessionID))
	t.Cleanup(func() { c.removeWorkspace(fmt.Sprintf("mango-workspace-%x", hash[:12])) })
	stop := s.startSupervisor(evidence.environment)
	s.send(sessionID, map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": "alpha:custom"}}}, 200)
	action := s.waitAction(sessionID, "agent.custom_tool_use", 1)
	stop()
	eventsBefore := s.events(sessionID)
	t.Log("Quiescing all writers, then capturing Mango/Temporal/visibility databases and bucket bytes")
	s.quiesce()
	databases := map[string][]byte{}
	for _, database := range []string{"mango", "temporal", "temporal_visibility"} {
		data := s.compose(nil, "exec", "-T", "postgres", "pg_dump", "-U", "postgres", "-d", database, "--format=custom", "--no-owner", "--no-acl")
		require.True(t, bytes.HasPrefix(data, []byte("PGDMP")), "logical dump format")
		databases[database] = data
	}
	objects := s.snapshotObjects()
	s.compose(nil, "stop", "--timeout", "30", "postgres", "nats", "seaweedfs", "model")
	restored := c.startState("restore")
	require.NotEqual(t, s.addresses["postgres"], restored.addresses["postgres"], "restore must use independent database containers")
	require.NotEqual(t, s.addresses["seaweedfs"], restored.addresses["seaweedfs"], "restore must use independent object storage")
	for _, database := range []string{"mango", "temporal", "temporal_visibility"} {
		if database != "mango" {
			restored.compose(nil, "exec", "-T", "postgres", "createdb", "-U", "postgres", database)
		}
		restored.compose(databases[database], "exec", "-T", "postgres", "pg_restore", "-U", "postgres", "-d", database, "--clean", "--if-exists", "--no-owner", "--no-acl", "--exit-on-error")
	}
	for _, object := range objects {
		_, err := restored.s3.PutObject(c.ctx, &s3.PutObjectInput{Bucket: aws.String("mango-alpha"), Key: aws.String(object.key), Body: bytes.NewReader(object.body), ContentType: aws.String(object.media)})
		require.NoError(t, err)
	}
	c.install(restored, "recovered", true)
	t.Log("Checking original IDs, bytes, Memory Versions, keyring verification and resumed Workflow")
	require.Equal(t, fileBefore, restored.json("GET", filePath, nil, 200))
	require.Equal(t, []byte("alpha-file-bytes"), restored.request("GET", filePath+"/content", "", nil, 200, true))
	require.Equal(t, skillBefore, restored.json("GET", skillPath, nil, 200))
	require.Equal(t, skillBytes, restored.request("GET", skillPath+"/content", "", nil, 200, true))
	require.Equal(t, memoryBefore, restored.json("GET", memoryPath+"/memories?view=full", nil, 200))
	require.Equal(t, versionsBefore, restored.json("GET", memoryPath+"/memory_versions?view=full", nil, 200))
	// VaultService.GetCredential decrypts/verifies before returning metadata.
	// Successful HTTP therefore exercises the restored envelope and original
	// random keyring, while the plaintext fixture secret remains write-only.
	require.Equal(t, credentialBefore, restored.json("GET", credentialPath, nil, 200))
	require.Equal(t, eventsBefore, restored.events(sessionID))
	require.Equal(t, action, restored.waitAction(sessionID, "agent.custom_tool_use", 1))
	result := map[string]any{"type": "user.custom_tool_result", "custom_tool_use_id": action,
		"content": []any{map[string]any{"type": "text", "text": "alpha-proof"}}}
	restored.send(sessionID, result, 200)
	restored.send(sessionID, result, 409)
	completed := restored.waitEvents(sessionID, func(events []map[string]any) bool {
		return eventCount(events, "agent.message") == 1 && latestIdle(events) == "end_turn"
	})
	require.Equal(t, 1, eventCount(completed, "agent.custom_tool_use"))
	require.Equal(t, 1, eventCount(completed, "user.custom_tool_result"))
	t.Log("Independent quiesced restore and original pending custom action continuation passed")
}
