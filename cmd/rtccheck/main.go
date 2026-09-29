// Command rtccheck exercises the MatrixRTC helpers against a real homeserver and LiveKit focus.
//
// Everything in callbridge/matrixrtc.go is HTTP against services that only exist in a deployment,
// so unit tests can only cover the pure halves. This drives the rest: discover the focus, get a
// LiveKit token for a real room as a real user, join the SFU, and publish audio.
//
// Credentials come from the environment and are never logged.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pion/rtp"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/callbridge"
	"maunium.net/go/mautrix/id"
)

func fail(step string, err error) {
	fmt.Printf("FAIL  %s: %v\n", step, err)
	os.Exit(1)
}

func ok(step string, detail string) { fmt.Printf("ok    %s%s\n", step, detail) }

func main() {
	ctx := context.Background()
	server := os.Getenv("MX_SERVER")     // e.g. https://maximiliangaedig.com
	domain := os.Getenv("MX_DOMAIN")     // e.g. maximiliangaedig.com
	user := os.Getenv("MX_USER")         // localpart
	pass := os.Getenv("MX_PASS")

	cli, err := mautrix.NewClient(server, "", "")
	if err != nil {
		fail("create client", err)
	}
	resp, err := cli.Login(ctx, &mautrix.ReqLogin{
		Type:             mautrix.AuthTypePassword,
		Identifier:       mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: user},
		Password:         pass,
		StoreCredentials: true,
	})
	if err != nil {
		fail("login", err)
	}
	ok("login", fmt.Sprintf(" as %s", resp.UserID))

	// A room of our own to hold the call, so nothing existing is disturbed.
	room, err := cli.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Name:   "rtccheck",
		Preset: "private_chat",
	})
	if err != nil {
		fail("create room", err)
	}
	ok("create room", " "+string(room.RoomID))
	defer func() {
		if _, err := cli.LeaveRoom(ctx, room.RoomID); err != nil {
			fmt.Printf("warn  leave room: %v\n", err)
		} else {
			ok("leave room", "")
		}
	}()

	// 1. Focus discovery - the real thing this could only fake in a unit test.
	focus, err := callbridge.DiscoverRTCTransport(ctx, cli, domain)
	if err != nil {
		fail("discover focus", err)
	}
	ok("discover focus", " "+focus.LivekitServiceURL)

	// 2. A LiveKit token for this room, proven with an OpenID token.
	url, token, err := callbridge.LiveKitToken(ctx, cli, focus, room.RoomID, "RTCCHECK")
	if err != nil {
		fail("livekit token", err)
	}
	ok("livekit token", fmt.Sprintf(" url=%s jwt=%d bytes", url, len(token)))

	// 3. Publish the membership, the way a bridge ghost would.
	stateKey := callbridge.RTCStateKey(cli.UserID, "RTCCHECK")
	content := callbridge.RTCMemberContent(focus, room.RoomID, "RTCCHECK", string(cli.UserID), false, time.Now().UnixMilli())
	if _, err = cli.SendStateEvent(ctx, room.RoomID, callbridge.CallMemberEventType, stateKey, content); err != nil {
		fail("publish membership", err)
	}
	ok("publish membership", " "+stateKey)

	// And read it back the way another participant would.
	var readBack map[string]any
	if err = cli.StateEvent(ctx, room.RoomID, callbridge.CallMemberEventType, stateKey, &readBack); err != nil {
		fail("read membership", err)
	}
	parsed := callbridge.ParseRTCMembership(readBack)
	if !parsed.Joined || parsed.DeviceID != "RTCCHECK" {
		fail("membership round trip", fmt.Errorf("read back joined=%v device=%q", parsed.Joined, parsed.DeviceID))
	}
	ok("membership round trip", " joined, device RTCCHECK")

	// 4. Actually join the SFU - twice.
	//
	// One leg publishes and one subscribes, which is the shape a bridged group call has: a Discord
	// speaker's leg puts audio in, and the Matrix participants take it out. Connecting alone only
	// proves the token was accepted; it says nothing about whether media reaches anyone.
	publisher, err := callbridge.JoinRTC(ctx, callbridge.RTCLegConfig{
		URL: url, Token: token, Log: zerolog.Nop(),
	})
	if err != nil {
		fail("join livekit (publisher)", err)
	}
	defer publisher.Close()
	ok("join livekit", " publisher connected")

	// A second identity in the same room, so the SFU has somebody to route to.
	subURL, subToken, err := callbridge.LiveKitToken(ctx, cli, focus, room.RoomID, "RTCCHECK2")
	if err != nil {
		fail("livekit token (subscriber)", err)
	}
	subscriber, err := callbridge.JoinRTC(ctx, callbridge.RTCLegConfig{
		URL: subURL, Token: subToken, Log: zerolog.Nop(),
	})
	if err != nil {
		fail("join livekit (subscriber)", err)
	}
	defer subscriber.Close()
	ok("join livekit", " subscriber connected")

	// 5. Publish audio, the way a Discord speaker's leg would, while the other leg listens.
	received := make(chan int, 1)
	go func() {
		track, trackErr := subscriber.RemoteAudio(ctx)
		if trackErr != nil {
			received <- 0
			return
		}
		count := 0
		for count < 10 {
			if _, _, readErr := track.ReadRTP(); readErr != nil {
				break
			}
			count++
		}
		received <- count
	}()

	writer := publisher.AudioWriter()
	sent := 0
	deadline := time.After(25 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
publish:
	for i := 0; ; i++ {
		select {
		case got := <-received:
			if got == 0 {
				fail("audio between participants", fmt.Errorf("the subscriber received nothing after %d sent", sent))
			}
			ok("audio between participants", fmt.Sprintf(" %d packets arrived (of %d sent)", got, sent))
			break publish
		case <-deadline:
			fail("audio between participants", fmt.Errorf("nothing arrived within 25s (%d sent)", sent))
		case <-ticker.C:
		}
		err = writer.WriteRTP(&rtp.Packet{
			Header: rtp.Header{
				Version: 2, SequenceNumber: uint16(1000 + i), Timestamp: uint32(i) * 960, Marker: i == 0,
			},
			Payload: make([]byte, 80),
		})
		if err != nil {
			fail("publish audio", err)
		}
		sent++
	}

	// Clear the membership, as a leaving participant does.
	if _, err = cli.SendStateEvent(ctx, room.RoomID, callbridge.CallMemberEventType, stateKey, map[string]any{}); err != nil {
		fmt.Printf("warn  clear membership: %v\n", err)
	} else {
		ok("clear membership", "")
	}
	_ = id.RoomID("")
	fmt.Println("\nALL STEPS PASSED")
}
