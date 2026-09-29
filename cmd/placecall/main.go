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
