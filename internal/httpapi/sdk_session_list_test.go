package httpapi

import (
	"context"
	"testing"

	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestSDK_SessionListBidirectionalPaginationAndStatusesFilter(t *testing.T) {
	client, server := sdkClientAndServer(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "opus", "sys")
	environmentID := mustEnv(t, server.URL)

	created := make([]string, 0, 5)
	for range 5 {
		session, err := client.Sessions.New(ctx, mango.SessionCreateRequest{
			Agent:         mango.AgentID(agent.ID),
			EnvironmentID: environmentID,
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		created = append(created, session.ID)
	}

	params := mango.ListSessionsParams{
		AgentID:  mango.Some(agent.ID),
		Limit:    mango.Some[int64](2),
		Order:    mango.Some("desc"),
		Statuses: mango.Some([]mango.SessionStatus{"idle"}),
	}
	first, err := client.Sessions.List(ctx, params)
	if err != nil {
		t.Fatalf("list first page: %v", err)
	}
	if len(first.Data) != 2 || first.NextPage == nil || first.PrevPage != nil {
		t.Fatalf("first page: rows=%d next=%v prev=%v", len(first.Data), first.NextPage, first.PrevPage)
	}
	if first.Data[0].ID != created[4] || first.Data[1].ID != created[3] {
		t.Fatalf("first page ids = [%s %s]", first.Data[0].ID, first.Data[1].ID)
	}

	params.Page = mango.Some(*first.NextPage)
	second, err := client.Sessions.List(ctx, params)
	if err != nil {
		t.Fatalf("get next page: %v", err)
	}
	if len(second.Data) != 2 || second.NextPage == nil || second.PrevPage == nil {
		t.Fatalf("second page: %#v", second)
	}
	if second.Data[0].ID != created[2] || second.Data[1].ID != created[1] {
		t.Fatalf("second page ids = [%s %s]", second.Data[0].ID, second.Data[1].ID)
	}

	params.Page = mango.Some(*second.PrevPage)
	back, err := client.Sessions.List(ctx, params)
	if err != nil {
		t.Fatalf("list previous page: %v", err)
	}
	if len(back.Data) != 2 ||
		back.Data[0].ID != created[4] ||
		back.Data[1].ID != created[3] {
		t.Fatalf("previous page did not return the first page: %#v", back.Data)
	}
}
