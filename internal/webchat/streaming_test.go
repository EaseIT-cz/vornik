package webchat

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

// Part 2 of the web chat (web-chat-design.md §3.2): a channel given a sink
// streams each delta into it; one without a sink stays one-shot.
func TestStreamingChannel_HandsDeltasToTheSink_WebChat(t *testing.T) {
	var got []string
	ch := NewStreaming("p1", conversation.Speaker{ID: "s"}, func(delta string) { got = append(got, delta) })
	var _ conversation.StreamingChannel = ch
	st, err := ch.StreamingSend(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append("Hel"); err != nil {
		t.Fatal(err)
	}
	if err := st.Append("lo"); err != nil {
		t.Fatal(err)
	}
	id, err := st.Close()
	if err != nil || id != "web-chat-stream" {
		t.Fatalf("Close = %q, %v", id, err)
	}
	if len(got) != 2 || got[0] != "Hel" || got[1] != "lo" {
		t.Errorf("sink got %q, want the two deltas", got)
	}
	if ch.Name() != ChannelName {
		t.Error("a streaming channel keeps the web chat's name")
	}
}

func TestChannel_WithoutASinkIsOneShot_WebChat(t *testing.T) {
	var c conversation.Channel = New("p1", conversation.Speaker{ID: "s"})
	if _, ok := c.(conversation.StreamingChannel); ok {
		t.Error("New must stay one-shot: the no-JS page streams to nobody")
	}
}

// Web chat cannot send a message later (web-chat-design.md §4.2, until #771):
// neither channel says it can.
func TestChannel_CannotDeliverLater_WebChat(t *testing.T) {
	for _, c := range []conversation.Channel{New("p1", conversation.Speaker{}), NewStreaming("p1", conversation.Speaker{}, nil)} {
		if l, ok := c.(conversation.LaterDeliveryChannel); ok && l.CanDeliverLater() {
			t.Errorf("%T claims it can deliver later", c)
		}
	}
}
