// Command placecall places a real 1:1 Matrix call into a bridged portal and reports what happened.
//
// This is the piece that was missing from every other check: the bridges were verified ready, and
// the media paths were verified in isolation, but nothing actually rang anybody. It acts as a
// Matrix calling client - a real PeerConnection, a real offer, m.call.invite into the portal - and
// then waits to see whether the bridge answers, whether ICE connects, and whether any audio
// arrives.
//
// The Matrix identity comes from appservice login with the double-puppet token, which is the same
// mechanism the bridges use to act as the user. Credentials come from the environment.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/callbridge"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func main() {
	server := os.Getenv("MX_SERVER")
	roomID := id.RoomID(os.Getenv("MX_ROOM"))
	asToken := os.Getenv("MX_AS_TOKEN")
	userID := os.Getenv("MX_USER_ID") // full @user:server
	ringFor := 30 * time.Second
	if v := os.Getenv("RING_SECONDS"); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		if n > 0 {
			ringFor = time.Duration(n) * time.Second
		}
	}
	if server == "" || roomID == "" || asToken == "" || userID == "" {
		fmt.Println("need MX_SERVER, MX_ROOM, MX_AS_TOKEN, MX_USER_ID")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ringFor+90*time.Second)
	defer cancel()

	// Log in as the user through the appservice, the way a bridge double-puppets.
	cli, err := mautrix.NewClient(server, "", "")
	if err != nil {
		fmt.Println("FAIL client:", err)
		os.Exit(1)
	}
	parsed := id.UserID(userID)
	localpart, _, _ := parsed.Parse()
	// Appservice login authenticates with the appservice's own token, so it goes on the client
	// before the request rather than in the body.
	cli.AccessToken = asToken
	resp, err := cli.Login(ctx, &mautrix.ReqLogin{
		Type:                     "m.login.application_service",
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: localpart},
		StoreCredentials:         true,
		InitialDeviceDisplayName: "placecall",
	})
	if err != nil {
		fmt.Println("FAIL appservice login:", err)
		os.Exit(1)
	}
	fmt.Println("ok    logged in as", resp.UserID)

	if os.Getenv("MODE") == "answer" {
		answerCall(ctx, cli, roomID, ringFor)
		return
	}

	// A real PeerConnection, offering audio like any Matrix client would.
	leg, err := callbridge.NewLeg(callbridge.LegConfig{Name: "caller", OpusPT: 111, Log: zerolog.Nop()})
	if err != nil {
		fmt.Println("FAIL create leg:", err)
		os.Exit(1)
	}
	defer leg.Close()
	if _, err = leg.CreateOffer(); err != nil {
		fmt.Println("FAIL create offer:", err)
		os.Exit(1)
	}
	offer := leg.WaitGathering(ctx, 8*time.Second)
	if offer == "" {
		fmt.Println("FAIL no ICE candidates gathered locally")
		os.Exit(1)
	}
	fmt.Println("ok    offer built with candidates")

	callID := uuid.NewString()
	partyID := "placecall-" + uuid.NewString()[:8]
	base := event.BaseCallEventContent{CallID: callID, PartyID: partyID, Version: event.CallVersion("1")}

	// Watch the room for the bridge's answer before ringing, so nothing is missed.
	answered := make(chan string, 1)
	hungUp := make(chan string, 1)
	syncer := cli.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.CallAnswer, func(ctx context.Context, evt *event.Event) {
		if evt.RoomID != roomID {
			return
		}
		if a, ok := evt.Content.Parsed.(*event.CallAnswerEventContent); ok && a.CallID == callID && a.PartyID != partyID {
			select {
			case answered <- a.Answer.SDP:
			default:
			}
		}
	})
	syncer.OnEventType(event.CallHangup, func(ctx context.Context, evt *event.Event) {
		if evt.RoomID != roomID {
			return
		}
		if h, ok := evt.Content.Parsed.(*event.CallHangupEventContent); ok && h.CallID == callID && h.PartyID != partyID {
			select {
			case hungUp <- string(h.Reason):
			default:
			}
		}
	})
	syncCtx, stopSync := context.WithCancel(ctx)
	defer stopSync()
	go func() { _ = cli.SyncWithContext(syncCtx) }()
	time.Sleep(2 * time.Second)

	if _, err = cli.SendMessageEvent(ctx, roomID, event.CallInvite, &event.CallInviteEventContent{
		BaseCallEventContent: base,
		Lifetime:             int(ringFor / time.Millisecond),
		Offer:                event.CallData{SDP: offer, Type: event.CallDataTypeOffer},
	}); err != nil {
		fmt.Println("FAIL send invite:", err)
		os.Exit(1)
	}
	fmt.Println("ok    call invite sent, ringing")

	// Send a steady stream once connected, so the far side has something to count.
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			_ = leg.Local.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: uint32(i) * 960, Marker: i == 0},
				Payload: make([]byte, 80),
			})
		}
	}()

	connected := make(chan webrtc.PeerConnectionState, 4)
	leg.OnState(func(s webrtc.PeerConnectionState) {
		select {
		case connected <- s:
		default:
		}
	})

	var sawAnswer, sawConnected bool
	var audioPackets int
	deadline := time.After(ringFor)
loop:
	for {
		select {
		case sdp := <-answered:
			sawAnswer = true
			fmt.Println("ok    the bridge answered with an SDP answer")
			if err = leg.SetAnswer(sdp); err != nil {
				fmt.Println("FAIL apply answer:", err)
				break loop
			}
			go func() {
				track, terr := leg.RemoteTrack(ctx)
				if terr != nil {
					return
				}
				for {
					if _, _, rerr := track.ReadRTP(); rerr != nil {
						return
					}
					audioPackets++
				}
			}()
		case st := <-connected:
			if st == webrtc.PeerConnectionStateConnected && !sawConnected {
				sawConnected = true
				fmt.Println("ok    media connection established")
			}
		case reason := <-hungUp:
			fmt.Println("info  the other side hung up:", reason)
			break loop
		case <-deadline:
			fmt.Println("info  ring time elapsed")
			break loop
		}
	}

	_, _ = cli.SendMessageEvent(ctx, roomID, event.CallHangup, &event.CallHangupEventContent{
		BaseCallEventContent: base, Reason: event.CallHangupUserHangup,
	})
	fmt.Println("ok    hung up")

	fmt.Printf("\nRESULT answered=%v connected=%v audio_packets=%d\n", sawAnswer, sawConnected, audioPackets)
	if !sawAnswer {
		fmt.Println("(no answer - expected when nobody picks up; the invite still exercised the bridge)")
	}
}


// answerCall picks up the next call in the room and reports whether audio arrives.
//
// This is what makes an end-to-end audio check possible at all: every real callee in this setup
// declines, so the only way to see media flow through an answered call is to be the callee.
func answerCall(ctx context.Context, cli *mautrix.Client, roomID id.RoomID, wait time.Duration) {
	invites := make(chan *event.CallInviteEventContent, 1)
	syncer := cli.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.CallInvite, func(ctx context.Context, evt *event.Event) {
		if evt.RoomID != roomID {
			return
		}
		if inv, ok := evt.Content.Parsed.(*event.CallInviteEventContent); ok {
			select {
			case invites <- inv:
			default:
			}
		}
	})
	syncCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = cli.SyncWithContext(syncCtx) }()
	fmt.Println("ok    waiting for a call")

	var inv *event.CallInviteEventContent
	select {
	case inv = <-invites:
	case <-time.After(wait):
		fmt.Println("FAIL no call arrived")
		os.Exit(1)
	}
	fmt.Println("ok    call received")

	leg, err := callbridge.NewLeg(callbridge.LegConfig{Name: "callee", OpusPT: 111, Log: zerolog.Nop()})
	if err != nil {
		fmt.Println("FAIL create leg:", err)
		os.Exit(1)
	}
	defer leg.Close()
	if _, err = leg.AnswerOffer(inv.Offer.SDP); err != nil {
		fmt.Println("FAIL answer offer:", err)
		os.Exit(1)
	}
	answer := leg.WaitGathering(ctx, 8*time.Second)
	if _, err = cli.SendMessageEvent(ctx, roomID, event.CallAnswer, &event.CallAnswerEventContent{
		BaseCallEventContent: event.BaseCallEventContent{
			CallID: inv.CallID, PartyID: "callee-" + uuid.NewString()[:8], Version: event.CallVersion("1"),
		},
		Answer: event.CallData{SDP: answer, Type: event.CallDataTypeAnswer},
	}); err != nil {
		fmt.Println("FAIL send answer:", err)
		os.Exit(1)
	}
	fmt.Println("ok    answered")

	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			_ = leg.Local.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: uint32(i) * 960, Marker: i == 0},
				Payload: make([]byte, 80),
			})
		}
	}()

	count := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		track, terr := leg.RemoteTrack(ctx)
		if terr != nil {
			return
		}
		for count < 50 {
			if _, _, rerr := track.ReadRTP(); rerr != nil {
				return
			}
			count++
		}
	}()
	select {
	case <-done:
	case <-time.After(wait):
	}
	fmt.Printf("\nRESULT answered=true audio_packets=%d\n", count)
	if count == 0 {
		fmt.Println("FAIL no audio arrived on an answered call")
		os.Exit(1)
	}
}
