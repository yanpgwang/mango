// Command quickstart is included directly in Mango's documentation.
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
}

func run() (result error) {
	if os.Getenv("MANGO_API_KEY") == "" {
		return errors.New("set MANGO_API_KEY before running this example")
	}
	// #region client
	baseURL := os.Getenv("MANGO_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	client, err := mango.New(mango.Config{
		BaseURL: baseURL,
		APIKey:  os.Getenv("MANGO_API_KEY"),
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// #endregion client

	var environmentID, agentID, sessionID string
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if sessionID != "" {
			_, err := client.DeleteSession(cleanup, sessionID)
			result = errors.Join(result, err)
		}
		if agentID != "" {
			_, err := client.ArchiveAgent(cleanup, agentID)
			result = errors.Join(result, err)
		}
		if environmentID != "" {
			_, err := client.DeleteEnvironment(cleanup, environmentID)
			result = errors.Join(result, err)
		}
	}()

	// #region environment
	environment, err := client.CreateEnvironment(ctx, mango.EnvironmentCreateRequest{
		Name: "Quickstart", // defaults to a cloud Environment
	})
	if err != nil {
		return err
	}
	// #endregion environment
	environmentID = environment.ID

	// #region agent
	agent, err := client.CreateAgent(ctx, mango.AgentCreateRequest{
		Name:   "Assistant",
		Model:  mango.ModelInput{String: mango.Ptr("offline-fake")},
		System: mango.SomePtr("Be concise."),
	})
	if err != nil {
		return err
	}
	// #endregion agent
	agentID = agent.ID

	// #region session
	session, err := client.CreateSession(ctx, mango.SessionCreateRequest{
		Agent:         mango.SessionAgentInput{String: mango.Ptr(agent.ID)},
		EnvironmentID: environment.ID,
		Title:         mango.Some("First session"),
	})
	if err != nil {
		return err
	}
	// #endregion session
	sessionID = session.ID

	// #region stream
	// Subscribe before sending: the stream does not replay earlier events.
	stream, err := client.StreamSessionEvents(ctx, session.ID, mango.StreamSessionEventsParams{})
	if err != nil {
		return err
	}
	defer stream.Close()
	_, err = client.SendSessionEvents(ctx, session.ID, mango.SendSessionEventsRequest{
		Events: []mango.ClientSessionEventInput{{
			UserMessageEventInput: &mango.UserMessageEventInput{
				Type: "user.message",
				Content: []mango.MessageContentInput{{
					TextBlockInput: &mango.TextBlockInput{Type: "text", Text: "Hello, Mango!"},
				}},
			},
		}},
	})
	if err != nil {
		return err
	}
	completed := false
	for stream.Next() {
		var frame mango.EventStreamFrame
		if err := stream.Event().Decode(&frame); err != nil {
			return err
		}
		if frame.SessionEvent == nil {
			continue // ephemeral previews are not persisted Session Events
		}
		if message := frame.SessionEvent.AgentMessageEvent; message != nil {
			fmt.Println(message.Content)
		}
		if idle := frame.SessionEvent.SessionStatusIdleEvent; idle != nil {
			if idle.StopReason.SessionEndTurn == nil {
				return errors.New("the turn requires attention")
			}
			completed = true
			break
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	if !completed {
		return errors.New("stream ended before completion; reconcile persisted history")
	}
	// #endregion stream

	// #region history
	history := client.ListSessionEventsAutoPaging(ctx, session.ID, mango.ListSessionEventsParams{
		Order: mango.Some("asc"), Limit: mango.Some(int64(100)),
	})
	var events []mango.SessionEvent
	for history.Next() {
		events = append(events, history.Value())
	}
	if err := history.Err(); err != nil {
		return err
	}
	fmt.Printf("Persisted events: %d\n", len(events))
	// #endregion history
	found := false
	for _, event := range events {
		found = found || event.AgentMessageEvent != nil
	}
	if !found {
		return errors.New("missing persisted response")
	}
	fmt.Println("Quickstart completed")
	return nil
}
