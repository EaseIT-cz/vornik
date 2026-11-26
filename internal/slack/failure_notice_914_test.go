package slack

// GitHub #914 (outcome-quality design, Amendment 2026-11-25 (#914)): a Slack
// turn whose model call fails used to post nothing; the error went to the
// Warn log only. These tests drive the real webhook handler, the real
// dispatcher.ChannelReceiver and the chat.postMessage path against a stub
// Slack API.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
)

const leakedUpstream = "upstream 502: <html>internal-host.example secret-body</html>"

// failingDoer fails every turn with an error that carries upstream text.
type failingDoer struct{ calls atomic.Int64 }

func (f *failingDoer) Process(context.Context, dispatcher.Request) dispatcher.Result {
	f.calls.Add(1)
	return dispatcher.Result{Err: errors.New(leakedUpstream)}
}

func (f *failingDoer) ProcessStreaming(ctx context.Context, req dispatcher.Request, _ chat.StreamCallback) dispatcher.Result {
	return f.Process(ctx, req)
}

// appendFailingStore answers the turn, then fails to persist it: Receive
// errors after the reply posted.
type appendFailingStore struct{ stubSessionStore }

func (appendFailingStore) Append(context.Context, conversation.ChannelMessage, dispatcher.Result) error {
	return errors.New("session store down")
}

type noticeRig struct {
	stub *slackStub
	ch   *Channel
	mux  *MuxHandler
	now  time.Time
}

func newNoticeRig(t *testing.T, doer dispatcher.Doer, sessions dispatcher.SessionStore) *noticeRig {
	t.Helper()
	stub := newSlackStub(t)
	srv := httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(srv.Close)
	cfg := validConfig() // production-shaped allowlists; U_alice in C_general is admitted
	cfg.APIBaseURL, cfg.HTTPClient = srv.URL, srv.Client()
	now := time.Unix(1700000000, 0)
	ch := makeChannel(t, cfg, now)
	bindReceiver(ch, &dispatcher.ChannelReceiver{Channel: ch, Agent: doer, Sessions: sessions})
	return &noticeRig{stub: stub, ch: ch, mux: NewMuxHandler([]*Channel{ch}, zerolog.Nop()), now: now}
}

func (r *noticeRig) mention(t *testing.T, eventID, ts string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"type": "event_callback", "team_id": "T123", "event_id": eventID,
		"event": map[string]any{
			"type": "app_mention", "user": "U_alice", "text": "<@bot> hello",
			"channel": "C_general", "ts": ts, "thread_ts": "1700000001.000100", "channel_type": "channel",
		},
	})
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, signedRequest(t, "shhh-signing-secret", r.now.Unix(), payload))
	r.mux.waitInFlight()
	if w.Code != http.StatusOK {
		t.Fatalf("webhook status = %d", w.Code)
	}
}

func (r *noticeRig) posts() []chatPostMessageRequest {
	var out []chatPostMessageRequest
	for i := 0; i < int(r.stub.calls.Load()); i++ {
		var p chatPostMessageRequest
		if json.Unmarshal([]byte(r.stub.snapshotBody(i)), &p) == nil && p.Text != "" {
			out = append(out, p)
		}
	}
	return out
}

func slackNotices(outcome string) float64 {
	return testutil.ToFloat64(conversation.FailureNotices.WithLabelValues("slack", outcome))
}

// The user is told, in the thread, with the shared wording and no error
// text; a second failure in the same thread within the hour is not posted.
func TestSlackFailedTurn_PostsSharedNoticeOncePerHour_914(t *testing.T) {
	rig := newNoticeRig(t, &failingDoer{}, &stubSessionStore{projectID: "p"})
	sent, suppressed := slackNotices("sent"), slackNotices("suppressed")

	rig.mention(t, "Ev914_1", "1700000010.000100")
	posts := rig.posts()
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want one failure notice: %+v", len(posts), posts)
	}
	want := "[Sorry, I could not answer: the model call failed. Please try again.]"
	// The notice speaks as the assistant, so on a first contact it is led by
	// the Art 50 notice (SendDisclosed).
	if !strings.HasSuffix(posts[0].Text, want) {
		t.Fatalf("notice = %q, want it to end %q", posts[0].Text, want)
	}
	if strings.Contains(posts[0].Text, "secret-body") || strings.Contains(posts[0].Text, "502") {
		t.Fatalf("notice leaks the error text: %q", posts[0].Text)
	}
	if posts[0].ThreadTs != "1700000001.000100" || posts[0].Channel != "C_general" {
		t.Fatalf("notice went to %s/%s, want the turn's thread", posts[0].Channel, posts[0].ThreadTs)
	}
	if got := slackNotices("sent") - sent; got != 1 {
		t.Fatalf("sent counter moved by %v, want 1", got)
	}

	rig.mention(t, "Ev914_2", "1700000020.000100")
	if n := len(rig.posts()); n != 1 {
		t.Fatalf("posts after a second failure in the hour = %d, want still 1", n)
	}
	if got := slackNotices("suppressed") - suppressed; got != 1 {
		t.Fatalf("suppressed counter moved by %v, want 1", got)
	}
}

// A turn that already posted its reply and then failed (here: the session
// store) gets no notice on top of the answer.
func TestSlackFailedTurn_NoNoticeAfterReply_914(t *testing.T) {
	rig := newNoticeRig(t, &stubDoer{reply: "the answer"}, &appendFailingStore{stubSessionStore{projectID: "p"}})
	rig.mention(t, "Ev914_3", "1700000030.000100")
	posts := rig.posts()
	if len(posts) != 1 || posts[0].Text != "the answer" {
		t.Fatalf("posts = %+v, want only the answer", posts)
	}
}

// A slash command's notice posts where its answer would: the channel level.
func TestSlackFailedSlashCommand_PostsNotice_914(t *testing.T) {
	rig := newNoticeRig(t, &failingDoer{}, &stubSessionStore{projectID: "p"})
	form := url.Values{
		"team_id": {"T123"}, "channel_id": {"C_general"}, "user_id": {"U_alice"},
		"command": {"/vornik"}, "text": {"summarize"}, "trigger_id": {"trigger-914"},
	}.Encode()
	req := signedRequest(t, "shhh-signing-secret", rig.now.Unix(), []byte(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rig.mux.ServeHTTP(httptest.NewRecorder(), req)
	deadline := time.Now().Add(2 * time.Second)
	for len(rig.posts()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	posts := rig.posts()
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "[Sorry, I could not answer:") {
		t.Fatalf("posts = %+v, want one failure notice", posts)
	}
	if posts[0].ThreadTs != "" {
		t.Fatalf("slash notice thread_ts = %q, want channel level", posts[0].ThreadTs)
	}
}

// The Art 50 disclosure, sent through the same Send before the turn runs,
// is not a reply: a first turn that fails still gets the notice.
func TestSlackFailedTurn_DisclosureIsNotAReply_914(t *testing.T) {
	stub := newSlackStub(t)
	ch := outboundChannel(t, stub)
	ch.postMessageRPS = 0 // two posts in one instant; the bucket is not under test
	ctx, replied := conversation.WithReplyMarker(context.Background())
	if _, err := ch.Send(conversation.WithDisclosureSend(ctx), conversation.ChannelMessage{
		SessionID: "T123/C_general#1700000010.000100", Text: "You are interacting with an AI system.",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if replied.Load() {
		t.Fatal("a disclosure send marked the turn as replied")
	}
	if _, err := ch.Send(ctx, conversation.ChannelMessage{SessionID: "T123/C_general#1700000010.000100", Text: "answer"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !replied.Load() {
		t.Fatal("a reply send did not mark the turn as replied")
	}
}

// A stopped daemon did not fail the request: no notice.
func TestSlackFailedTurn_CancelledIsNotAFailure_914(t *testing.T) {
	stub := newSlackStub(t)
	ch := outboundChannel(t, stub)
	msg := conversation.ChannelMessage{SessionID: "T123/C_general#1700000010.000100"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch.afterFailedTurn(ctx, msg, context.Canceled)
	ch.afterFailedTurn(context.Background(), msg, context.Canceled)
	if stub.calls.Load() != 0 {
		t.Fatalf("posts = %d, want none for a cancelled turn", stub.calls.Load())
	}
}

// A notice refused by the local per-channel bucket (the disclosure or the
// placeholder just posted) is retried once after the bucket's wait.
func TestSlackFailedTurn_RetriesOnceWhenBucketEmpty_914(t *testing.T) {
	stub := newSlackStub(t)
	ch := outboundChannel(t, stub)
	ch.postMessageRPS, ch.postMessageBurst = 50, 1 // refill in 20ms
	now := time.Unix(1700000000, 0)
	ch.clock = func() time.Time { return now }
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "T123/C_general#1700000010.000100", Text: "first"}); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	ch.noticeSleep = func(d time.Duration) { now = now.Add(d) } // the wait elapses
	ch.afterFailedTurn(context.Background(), conversation.ChannelMessage{SessionID: "T123/C_general#1700000010.000100"}, errors.New("boom"))
	if stub.calls.Load() != 2 {
		t.Fatalf("posts = %d, want the notice after one retry", stub.calls.Load())
	}
}
