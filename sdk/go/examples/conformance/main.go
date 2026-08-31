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
	if err := client.Health(ctx); err != nil {
		return err
	}
	var environmentID, sessionID string
	var agentIDs []string
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if sessionID != "" {
			_, err := client.DeleteSession(cleanupCtx, sessionID)
			result = errors.Join(result, err)
		}
		for _, id := range agentIDs {
			_, err := client.ArchiveAgent(cleanupCtx, id)
			result = errors.Join(result, err)
		}
		if environmentID != "" {
			_, err := client.DeleteEnvironment(cleanupCtx, environmentID)
			result = errors.Join(result, err)
		}
	}()
	environment, err := client.CreateEnvironment(ctx, mango.EnvironmentCreateRequest{
		Name:   "go-sdk-conformance",
		Config: mango.Some(mango.EnvironmentConfigInput{CloudEnvironmentConfigInput: &mango.CloudEnvironmentConfigInput{Type: "cloud"}}),
	})
	if err != nil {
		return err
	}
	environmentID = environment.ID
	for _, suffix := range []string{"one", "two"} {
		agent, err := client.CreateAgent(ctx, mango.AgentCreateRequest{Name: "go-sdk-" + suffix, Model: mango.ModelInput{String: mango.Ptr("sdk-conformance")}})
		if err != nil {
			return err
		}
		agentIDs = append(agentIDs, agent.ID)
		fetched, err := client.GetAgent(ctx, agent.ID)
		if err != nil {
			return err
		}
		if fetched.Name != "go-sdk-"+suffix {
			return fmt.Errorf("wrong retrieved Agent name: %q", fetched.Name)
		}
	}
	page, err := client.ListAgents(ctx, mango.ListAgentsParams{Limit: mango.Some(int64(1))})
	if err != nil {
		return err
	}
	if len(page.Data) != 1 || page.NextPage == nil {
		return errors.New("expected a paginated Agent result")
	}
	listed := make(map[string]bool)
	iterator := client.ListAgentsAutoPaging(ctx, mango.ListAgentsParams{Limit: mango.Some(int64(1))})
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
	session, err := client.CreateSession(ctx, mango.SessionCreateRequest{
		Agent:         mango.SessionAgentInput{AgentReference: &mango.AgentReference{Type: "agent", ID: agentIDs[0]}},
		EnvironmentID: environmentID,
	})
	if err != nil {
		return err
	}
	sessionID = session.ID
	stream, err := client.StreamSessionEvents(ctx, session.ID, mango.StreamSessionEventsParams{})
	if err != nil {
		return err
	}
	defer stream.Close()
	sent, err := client.SendSessionEvents(ctx, session.ID, mango.SendSessionEventsRequest{Events: []mango.ClientSessionEventInput{{
		UserMessageEventInput: &mango.UserMessageEventInput{Type: "user.message", Content: []mango.MessageContentInput{{
			TextBlockInput: &mango.TextBlockInput{Type: "text", Text: "sdk test"},
		}}},
	}}})
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
	history := client.ListSessionEventsAutoPaging(ctx, session.ID, mango.ListSessionEventsParams{Order: mango.Some("asc")})
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
	_, err = client.GetSession(ctx, "sesn_go_missing")
	var apiError *mango.APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != 404 || apiError.Type != "not_found_error" || apiError.RequestID == "" {
		return fmt.Errorf("expected typed correlated 404, got %v", err)
	}
	return nil
}
