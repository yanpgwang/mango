// Command mongodb-query asks an agent to query MongoDB from a Docker worker.
// The application and worker use only Mango's public Go SDK.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	mango "github.com/yanpgwang/mango/sdk/go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	if len(os.Args) == 2 && os.Args[1] == "worker" {
		err = runWorker(ctx)
	} else if len(os.Args) == 1 {
		err = run(ctx)
	} else {
		err = errors.New("usage: mongodb-query [worker]")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(parent context.Context) (result error) {
	for _, name := range []string{"MANGO_API_KEY", "MANGO_MODEL_ID", "MONGO_URI"} {
		if os.Getenv(name) == "" {
			return fmt.Errorf("set %s before running this example", name)
		}
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	client, err := mango.New(mango.Config{
		BaseURL: envOr("MANGO_BASE_URL", "http://localhost:8080"),
		APIKey:  os.Getenv("MANGO_API_KEY"),
	})
	if err != nil {
		return err
	}

	var environmentID, agentID, sessionID string
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if sessionID != "" {
			_, err := client.Sessions.Delete(cleanup, sessionID)
			result = errors.Join(result, err)
		}
		if agentID != "" {
			_, err := client.Agents.Archive(cleanup, agentID)
			result = errors.Join(result, err)
		}
		if environmentID != "" {
			_, err := client.Environments.Delete(cleanup, environmentID)
			result = errors.Join(result, err)
		}
	}()
	environment, err := client.Environments.New(ctx, mango.EnvironmentCreateRequest{Name: "MongoDB query example"})
	if err != nil {
		return err
	}
	environmentID = environment.ID
	agent, err := client.Agents.New(ctx, mango.AgentCreateRequest{
		Name: "Inventory assistant", Model: mango.ModelID(os.Getenv("MANGO_MODEL_ID")),
		System: mango.SomePtr(`Answer inventory questions by querying MongoDB through Bash and Python's pymongo.
Read the connection string from os.environ["MONGO_URI"], the database name from
os.environ["MONGO_DATABASE"], and the collection name from os.environ["MONGO_COLLECTION"].
Never print the connection string. Use read-only queries with a serverSelectionTimeoutMS of 10000.
Documents have sku, name, stock, and reorder_point fields. Base your answer on retrieved records.
If the query fails, explain the failure instead of inventing inventory data.`),
		Tools: mango.Some([]mango.AgentTool{{BuiltinToolset: &mango.BuiltinToolset{
			Type:    "agent_toolset_20260401",
			Configs: mango.Some([]mango.BuiltinToolsetConfigsItem{{Name: "bash", Enabled: mango.Some(true)}}),
			DefaultConfig: mango.Some(mango.ToolDefaultConfig{
				Enabled: mango.Some(false), PermissionPolicy: mango.Some(mango.PermissionPolicy{Type: "always_allow"}),
			}),
		}}}),
	})
	if err != nil {
		return err
	}
	agentID = agent.ID
	session, err := client.Sessions.New(ctx, mango.SessionCreateRequest{
		Agent: mango.AgentID(agent.ID), EnvironmentID: environment.ID,
		Title: mango.Some("Inventory replenishment"),
	})
	if err != nil {
		return err
	}
	sessionID = session.ID
	fmt.Println("Session:", sessionID)

	// Subscribe before sending; the stream does not replay older events.
	stream, err := client.Sessions.Events.Stream(ctx, sessionID, mango.StreamSessionEventsParams{})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() {
		err := launchWorker(workerCtx, client, environmentID)
		workerDone <- err
		if err != nil {
			cancel() // Unblock the event reader if Docker or the worker fails.
		}
	}()
	defer func() {
		stopWorker()
		result = errors.Join(result, <-workerDone)
	}()

	question := envOr("MANGO_EXAMPLE_PROMPT", "Which products need reordering? List each SKU and the units needed to reach its reorder point.")
	fmt.Println("Question:", question)
	_, err = client.Sessions.Events.Send(ctx, sessionID, mango.SendSessionEventsRequest{
		Events: []mango.ClientSessionEventInput{mango.UserMessage(question)},
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
		if tool := event.AgentToolUseEvent; tool != nil {
			fmt.Println("Tool:", tool.Name)
		}
		if toolResult := event.PersistedUserToolResultEvent; toolResult != nil {
			if content, ok := toolResult.Content.Get(); ok {
				for _, block := range content {
					if block.TextBlockInput != nil {
						fmt.Println("Tool output:", block.TextBlockInput.Text)
					}
				}
			}
		}
		if message := event.AgentMessageEvent; message != nil {
			for _, block := range message.Content {
				fmt.Println(block.Text)
			}
		}
		if idle := event.SessionStatusIdleEvent; idle != nil {
			switch {
			case idle.StopReason.SessionRequiresAction != nil:
				// The SDK worker supplies the pending Bash result.
				continue
			case idle.StopReason.SessionEndTurn != nil:
				return nil
			default:
				return fmt.Errorf("session paused: %+v", idle.StopReason)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	return errors.New("stream disconnected before the answer; inspect the deployment logs")
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
