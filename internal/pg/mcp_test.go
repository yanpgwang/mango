package pg

import (
	"context"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/mcpclient"
)

func TestMCPDiscoverySnapshot_IsInsertOncePerThreadServer(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sess_mcp_snapshot")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatal(err)
	}
	threadID, err := store.q.GetPrimarySessionThreadID(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := domain.MCPServer{
		Name: "github", URL: "https://mcp.example.com",
	}
	first := []mcpclient.Tool{{
		Name: "list_issues", InputSchema: map[string]any{"type": "object"},
	}}
	got, err := store.PutMCPDiscoverySnapshot(
		ctx,
		session.ID,
		threadID,
		server,
		first,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "list_issues" {
		t.Fatalf("first snapshot = %#v", got)
	}
	second := []mcpclient.Tool{{
		Name: "new_remote_tool", InputSchema: map[string]any{"type": "object"},
	}}
	got, err = store.PutMCPDiscoverySnapshot(
		ctx,
		session.ID,
		threadID,
		server,
		second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "list_issues" {
		t.Fatalf("snapshot changed after second insert: %#v", got)
	}
}

func TestMCPDiscoverySnapshot_IsolatedBySessionThread(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sess_mcp_thread_isolation")
	session.AgentSnapshot = domain.Agent{
		ID: session.AgentID, Version: session.AgentVersion, Name: "coordinator",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{
			Type: "agent", ID: "agent_peer", Version: 2,
		}}},
	}
	session.MultiagentRoster = []domain.Agent{{
		ID: "agent_peer", Version: 2, Name: "reviewer",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
	}}
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatal(err)
	}
	primaryID, err := store.q.GetPrimarySessionThreadID(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := store.CreateChildSessionThread(
		ctx, session.ID, primaryID, "reviewer",
	)
	if err != nil {
		t.Fatal(err)
	}
	primaryServer := domain.MCPServer{Name: "github", URL: "https://primary.example.com"}
	childServer := domain.MCPServer{Name: "github", URL: "https://child.example.com"}
	if _, err := store.PutMCPDiscoverySnapshot(ctx, session.ID, primaryID, primaryServer,
		[]mcpclient.Tool{{Name: "primary_tool"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutMCPDiscoverySnapshot(ctx, session.ID, child.ID, childServer,
		[]mcpclient.Tool{{Name: "child_tool"}}); err != nil {
		t.Fatal(err)
	}
	primary, found, err := store.GetMCPDiscoverySnapshot(
		ctx, session.ID, primaryID, primaryServer,
	)
	if err != nil || !found || len(primary) != 1 || primary[0].Name != "primary_tool" {
		t.Fatalf("primary snapshot = %#v, found=%v, err=%v", primary, found, err)
	}
	childTools, found, err := store.GetMCPDiscoverySnapshot(
		ctx, session.ID, child.ID, childServer,
	)
	if err != nil || !found || len(childTools) != 1 || childTools[0].Name != "child_tool" {
		t.Fatalf("child snapshot = %#v, found=%v, err=%v", childTools, found, err)
	}
}

func TestMCPDiscoverySnapshot_PrimaryConfigurationUpdateInvalidatesCache(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sess_mcp_update")
	session.AgentSnapshot.Tools = []any{map[string]any{
		"type": "mcp_toolset", "mcp_server_name": "github",
	}}
	session.AgentSnapshot.MCPServers = []any{map[string]any{
		"type": "url", "name": "github", "url": "https://old.example.com",
	}}
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatal(err)
	}
	primaryID, err := store.q.GetPrimarySessionThreadID(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := domain.MCPServer{Name: "github", URL: "https://old.example.com"}
	if _, err := store.PutMCPDiscoverySnapshot(ctx, session.ID, primaryID, server,
		[]mcpclient.Tool{{Name: "old_tool"}}); err != nil {
		t.Fatal(err)
	}
	replacementTools := []any{}
	replacementServers := []any{}
	if _, err := store.UpdateSession(ctx, session.ID, domain.SessionUpdate{
		AgentTools:      &replacementTools,
		AgentMCPServers: &replacementServers,
	}); err != nil {
		t.Fatal(err)
	}
	_, found, err := store.GetMCPDiscoverySnapshot(
		ctx, session.ID, primaryID, server,
	)
	if err != nil || found {
		t.Fatalf("stale primary snapshot found=%v, err=%v", found, err)
	}
}
