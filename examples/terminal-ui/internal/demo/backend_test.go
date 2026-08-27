package demo

import (
	"context"
	"testing"
	"time"

	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

func TestAttachmentsEachReceiveDemoUpdates(t *testing.T) {
	backend := New()
	first, err := backend.Attach(context.Background(), "sesn_demo_product_launch")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cancel()
	second, err := backend.Attach(context.Background(), "sesn_demo_product_launch")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Cancel()

	if _, err := backend.SendMessage(context.Background(), "sesn_demo_product_launch", "hello"); err != nil {
		t.Fatal(err)
	}
	for index, updates := range []<-chan mango.StreamUpdate{first.Updates, second.Updates} {
		select {
		case update := <-updates:
			if update.Frame.Type() != "user.message" {
				t.Fatalf("attachment %d update=%#v", index, update)
			}
		case <-time.After(time.Second):
			t.Fatalf("attachment %d missed broadcast", index)
		}
	}
}
