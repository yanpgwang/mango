// Command conformance exercises the public API against the repository's local
// HTTP-handler test harness. It does not call or verify a live model.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	mango "github.com/yanpgwang/mango/sdk/go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Go SDK local Mango HTTP conformance passed")
}

func run() (result error) {
	client, err := mango.New(mango.Config{BaseURL: os.Getenv("MANGO_SDK_TEST_URL"), APIKey: os.Getenv("MANGO_SDK_TEST_KEY")})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.System.Health(ctx); err != nil {
		return err
	}
	var environmentID, sessionID string
	var agentIDs []string
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if sessionID != "" {
			_, err := client.Sessions.Delete(cleanupCtx, sessionID)
			result = errors.Join(result, err)
		}
		for _, id := range agentIDs {
			_, err := client.Agents.Archive(cleanupCtx, id)
			result = errors.Join(result, err)
		}
		if environmentID != "" {
			_, err := client.Environments.Delete(cleanupCtx, environmentID)
			result = errors.Join(result, err)
		}
	}()
	environment, err := client.Environments.New(ctx, mango.EnvironmentCreateRequest{
		Name:   "go-sdk-conformance",
		Config: mango.Some(mango.EnvironmentConfigInput{Type: "self_hosted"}),
	})
	if err != nil {
		return err
	}
	environmentID = environment.ID
	for _, suffix := range []string{"one", "two"} {
		agent, err := client.Agents.New(ctx, mango.AgentCreateRequest{Name: "go-sdk-" + suffix, Model: mango.ModelID("sdk-conformance")})
		if err != nil {
			return err
		}
		agentIDs = append(agentIDs, agent.ID)
		fetched, err := client.Agents.Get(ctx, agent.ID)
		if err != nil {
			return err
		}
		if fetched.Name != "go-sdk-"+suffix {
			return fmt.Errorf("wrong retrieved Agent name: %q", fetched.Name)
		}
	}
	page, err := client.Agents.List(ctx, mango.ListAgentsParams{Limit: mango.Some(int64(1))})
	if err != nil {
		return err
	}
	if len(page.Data) != 1 || page.NextPage == nil {
		return errors.New("expected a paginated Agent result")
	}
	listed := make(map[string]bool)
	iterator := client.Agents.ListAutoPaging(ctx, mango.ListAgentsParams{Limit: mango.Some(int64(1))})
	for iterator.Next() {
		listed[iterator.Value().ID] = true
	}
	if iterator.Err() != nil {
		return iterator.Err()
	}
	for _, id := range agentIDs {
		if !listed[id] {
			return fmt.Errorf("pagination omitted Agent %s", id)
		}
	}
	coordinator, err := client.Agents.New(ctx, mango.AgentCreateRequest{
		Name: "go-sdk-lead", Model: mango.ModelID("sdk-conformance"),
		Multiagent: mango.Some(mango.Coordinator(
			mango.RosterAgentVersion(agentIDs[0], 1), mango.RosterAgent(agentIDs[1]),
			mango.Self(), mango.Advisor("review-model"),
		)),
	})
	if err != nil {
		return err
	}
	agentIDs = append(agentIDs, coordinator.ID)
	roster := coordinator.Multiagent.ResolvedMultiagent
	if roster == nil || len(roster.Agents) != 4 || roster.Agents[0].ResolvedAgentReference == nil || roster.Agents[0].ResolvedAgentReference.Version != 1 || roster.Agents[3].MultiagentAdvisor == nil || roster.Agents[3].MultiagentAdvisor.Model != "review-model" {
		return errors.New("coordinator roster lost its pinned Agent or Advisor")
	}
	session, err := client.Sessions.New(ctx, mango.SessionCreateRequest{
		Agent:         mango.AgentID(coordinator.ID),
		EnvironmentID: environmentID,
	})
	if err != nil {
		return err
	}
	sessionID = session.ID
	stream, err := client.Sessions.Events.Stream(ctx, session.ID, mango.StreamSessionEventsParams{})
	if err != nil {
		return err
	}
	defer stream.Close()
	sent, err := client.Sessions.Events.Send(ctx, session.ID, mango.SendSessionEventsRequest{Events: []mango.ClientSessionEventInput{mango.UserMessage("sdk test")}})
	if err != nil {
		return err
	}
	if len(sent.Data) != 1 {
		return errors.New("expected one admitted user event")
	}
	observed := false
	for stream.Next() {
		var frame mango.EventStreamFrame
		if err := stream.Event().Decode(&frame); err != nil {
			return err
		}
		if frame.SessionEvent != nil && frame.SessionEvent.PersistedUserMessageEvent != nil {
			observed = true
			break
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	if !observed {
		return errors.New("ready subscription did not receive the submitted user event")
	}
	if err := stream.Close(); err != nil {
		return err
	}
	foundUserMessage := false
	history := client.Sessions.Events.ListAutoPaging(ctx, session.ID, mango.ListSessionEventsParams{Order: mango.Some("asc")})
	for history.Next() {
		if history.Value().PersistedUserMessageEvent != nil {
			foundUserMessage = true
		}
	}
	if history.Err() != nil {
		return history.Err()
	}
	if !foundUserMessage {
		return errors.New("user message missing from typed Session history")
	}
	_, err = client.Sessions.Get(ctx, "sesn_go_missing")
	var apiError *mango.APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != 404 || apiError.Type != "not_found_error" || apiError.RequestID == "" {
		return fmt.Errorf("expected typed correlated 404, got %v", err)
	}
	return nil
}
