// Command multiagent runs a specialist team through Mango's public SDK.
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
	model, environmentID := os.Getenv("MANGO_MODEL_ID"), os.Getenv("MANGO_ENVIRONMENT_ID")
	if model == "" || environmentID == "" || os.Getenv("MANGO_API_KEY") == "" {
		return errors.New("set MANGO_MODEL_ID, MANGO_ENVIRONMENT_ID and MANGO_API_KEY")
	}
	baseURL := os.Getenv("MANGO_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	client, err := mango.New(mango.Config{BaseURL: baseURL, APIKey: os.Getenv("MANGO_API_KEY")})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	var agentIDs []string
	var sessionID string
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if sessionID != "" {
			_, err := client.Sessions.Delete(cleanup, sessionID)
			result = errors.Join(result, err)
		}
		for i := len(agentIDs) - 1; i >= 0; i-- {
			_, err := client.Agents.Archive(cleanup, agentIDs[i])
			result = errors.Join(result, err)
		}
	}()
	// #region team
	researcher, err := client.Agents.New(ctx, mango.AgentCreateRequest{
		Name: "researcher", Model: mango.ModelID(model),
		System: mango.SomePtr("Analyze the proposal and report concrete benefits and tradeoffs."),
	})
	if err != nil {
		return err
	}
	agentIDs = append(agentIDs, researcher.ID)
	reviewer, err := client.Agents.New(ctx, mango.AgentCreateRequest{
		Name: "reviewer", Model: mango.ModelID(model),
		System: mango.SomePtr("Review the proposal independently and identify risks and missing assumptions."),
	})
	if err != nil {
		return err
	}
	agentIDs = append(agentIDs, reviewer.ID)
	roster := []mango.MultiagentRosterEntryInput{
		mango.RosterAgentVersion(researcher.ID, researcher.Version),
		mango.RosterAgentVersion(reviewer.ID, reviewer.Version),
	}
	if advisor := os.Getenv("MANGO_ADVISOR_MODEL"); advisor != "" {
		roster = append(roster, mango.Advisor(advisor))
	}
	coordinator, err := client.Agents.New(ctx, mango.AgentCreateRequest{
		Name: "lead", Model: mango.ModelID(model),
		System:     mango.SomePtr("Delegate to both specialists, wait for their reports, then synthesize. For follow-ups, reuse the existing specialist threads."),
		Multiagent: mango.Some(mango.Coordinator(roster...)),
	})
	if err != nil {
		return err
	}
	agentIDs = append(agentIDs, coordinator.ID)
	session, err := client.Sessions.New(ctx, mango.SessionCreateRequest{
		Agent: mango.AgentID(coordinator.ID), EnvironmentID: environmentID,
	})
	if err != nil {
		return err
	}
	sessionID = session.ID
	// #endregion team

	turn := func(text string) error {
		// #region observe
		stream, err := client.Sessions.Events.Stream(ctx, session.ID, mango.StreamSessionEventsParams{})
		if err != nil {
			return err
		}
		defer stream.Close()
		_, err = client.Sessions.Events.Send(ctx, session.ID, mango.SendSessionEventsRequest{
			Events: []mango.ClientSessionEventInput{mango.UserMessage(text)},
		})
		if err != nil {
			return err
		}
		for stream.Next() {
			var frame mango.EventStreamFrame
			if err := stream.Event().Decode(&frame); err != nil {
				return err
			}
			event := frame.SessionEvent
			if event == nil {
				continue
			}
			if event.AgentMessageEvent != nil {
				fmt.Println(event.AgentMessageEvent.Content)
			}
			if idle := event.SessionStatusIdleEvent; idle != nil {
				if idle.StopReason.SessionEndTurn == nil {
					return errors.New("session needs attention; inspect its persisted events")
				}
				return nil
			}
			if event.SessionStatusTerminatedEvent != nil || event.SessionDeletedEvent != nil {
				return errors.New("session terminated before completing the turn")
			}
		}
		if err := stream.Err(); err != nil {
			return err
		}
		return errors.New("stream disconnected; reconcile persisted events before resending")
		// #endregion observe
	}
	task := os.Getenv("MANGO_TASK")
	if task == "" {
		task = "Compare a monolith and microservices for a three-person team."
	}
	if err := turn(task); err != nil {
		return err
	}
	if err := turn("Ask the existing reviewer thread to challenge its earlier conclusion, then summarize."); err != nil {
		return err
	}
	// #region threads
	threads := client.Sessions.Threads.ListAutoPaging(ctx, session.ID, mango.ListSessionThreadsParams{})
	for threads.Next() {
		thread := threads.Value()
		fmt.Println(thread.ID, thread.Status)
		events := client.Sessions.Threads.Events.ListAutoPaging(ctx, session.ID, thread.ID, mango.ListSessionThreadEventsParams{})
		for events.Next() {
			if message := events.Value().AgentMessageEvent; message != nil {
				fmt.Println(message.Content)
			}
		}
		if err := events.Err(); err != nil {
			return err
		}
	}
	return threads.Err()
	// #endregion threads
}
