// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js

package webrtc

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pion/transport/v5/test"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pcUndeclaredProcessors counts the undeclared RTP/RTCP media processor
// goroutines owned by the given PeerConnection, matching its receiver pointer
// in the goroutine stacks. Counting per-PC (rather than process-wide) keeps
// the assertion independent of other connections being torn down concurrently.
func pcUndeclaredProcessors(pc *PeerConnection) (rtp, rtcp int) {
	addr := fmt.Sprintf("%p", pc)
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
		if !strings.Contains(stack, addr) {
			continue
		}
		if strings.Contains(stack, "undeclaredRTPMediaProcessor") {
			rtp++
		}
		if strings.Contains(stack, "undeclaredRTCPMediaProcessor") {
			rtcp++
		}
	}

	return rtp, rtcp
}

// dcTextRoundtrip sends a text message over the given channel and waits for it
// to arrive on the remote channel.
func dcTextRoundtrip(t *testing.T, sender, receiver *DataChannel, tag string) {
	t.Helper()
	got := make(chan struct{}, 1)
	receiver.OnMessage(func(msg DataChannelMessage) {
		if string(msg.Data) == tag {
			select {
			case got <- struct{}{}:
			default:
			}
		}
	})
	require.NoError(t, sender.SendText(tag))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatalf("data channel text message %q did not arrive", tag)
	}
}

// runDataChannelFirstOnce performs one full flow:
//  1. establish a connection carrying only a data channel (no media tracks),
//  2. exchange text over it,
//  3. add a video track from the side selected by offererAddsTrack and renegotiate,
//  4. assert video is received and text still flows afterwards.
func runDataChannelFirstOnce(t *testing.T, offererAddsTrack bool) {
	t.Helper()

	pcOffer, pcAnswer, err := newPair()
	require.NoError(t, err)

	dcOffer, err := pcOffer.CreateDataChannel("text", nil)
	require.NoError(t, err)

	dcOpen := make(chan struct{})
	dcOffer.OnOpen(func() { close(dcOpen) })

	dcAnswerCh := make(chan *DataChannel, 1)
	pcAnswer.OnDataChannel(func(d *DataChannel) {
		dcAnswerCh <- d
	})

	require.NoError(t, signalPairWithOptions(pcOffer, pcAnswer, withDisableInitialDataChannel(true)))

	select {
	case <-dcOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("offerer data channel did not open")
	}

	var dcAnswer *DataChannel
	select {
	case dcAnswer = <-dcAnswerCh:
	case <-time.After(5 * time.Second):
		t.Fatal("answerer did not receive data channel")
	}

	// Text works over the media-less connection.
	dcTextRoundtrip(t, dcOffer, dcAnswer, "before-track-offer->answer")
	dcTextRoundtrip(t, dcAnswer, dcOffer, "before-track-answer->offer")

	// Add a video track from one side and renegotiate.
	var senderPC, receiverPC *PeerConnection
	if offererAddsTrack {
		senderPC, receiverPC = pcOffer, pcAnswer
	} else {
		senderPC, receiverPC = pcAnswer, pcOffer
	}

	videoTrack, err := NewTrackLocalStaticSample(
		RTPCodecCapability{MimeType: MimeTypeVP8}, "video", "stream",
	)
	require.NoError(t, err)
	_, err = senderPC.AddTrack(videoTrack)
	require.NoError(t, err)

	gotRTP := make(chan struct{})
	receiverPC.OnTrack(func(track *TrackRemote, r *RTPReceiver) {
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, readErr := track.Read(buf); readErr != nil {
					return
				}
				select {
				case gotRTP <- struct{}{}:
				default:
				}
			}
		}()
	})

	if offererAddsTrack {
		require.NoError(t, signalPair(pcOffer, pcAnswer))
	} else {
		require.NoError(t, signalPair(pcAnswer, pcOffer))
	}

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = videoTrack.WriteSample(media.Sample{Data: []byte{0x00}, Duration: 20 * time.Millisecond})
			}
		}
	}()
	select {
	case <-gotRTP:
	case <-time.After(10 * time.Second):
		close(stop)
		t.Fatal("did not receive video RTP after renegotiation")
	}
	close(stop)

	// Once RTP media is negotiated, the undeclared-media processors must be
	// running (started lazily by the renegotiation), exactly one per PeerConnection.
	offerRTP, offerRTCP := pcUndeclaredProcessors(pcOffer)
	assert.Equal(t, 1, offerRTP, "offerer: expected one undeclared RTP processor after media added")
	assert.Equal(t, 1, offerRTCP, "offerer: expected one undeclared RTCP processor after media added")
	answerRTP, answerRTCP := pcUndeclaredProcessors(pcAnswer)
	assert.Equal(t, 1, answerRTP, "answerer: expected one undeclared RTP processor after media added")
	assert.Equal(t, 1, answerRTCP, "answerer: expected one undeclared RTCP processor after media added")

	// Text channel must still work after the media renegotiation.
	dcTextRoundtrip(t, dcOffer, dcAnswer, "after-track-offer->answer")
	dcTextRoundtrip(t, dcAnswer, dcOffer, "after-track-answer->offer")

	closePairNow(t, pcOffer, pcAnswer)
}

func TestPeerConnection_DataChannelFirst_OffererAddsTrack(t *testing.T) {
	lim := test.TimeOut(60 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()
	runDataChannelFirstOnce(t, true)
}

func TestPeerConnection_DataChannelFirst_AnswererAddsTrack(t *testing.T) {
	lim := test.TimeOut(60 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()
	runDataChannelFirstOnce(t, false)
}

// TestPeerConnection_DataChannelFirst_20Rounds exercises both add-track
// directions for 20 rounds each.
func TestPeerConnection_DataChannelFirst_20Rounds(t *testing.T) {
	lim := test.TimeOut(10 * time.Minute)
	defer lim.Stop()

	for _, offererAddsTrack := range []bool{true, false} {
		name := "offerer-adds-track"
		if !offererAddsTrack {
			name = "answerer-adds-track"
		}
		t.Run(name, func(t *testing.T) {
			for i := 0; i < 20; i++ {
				t.Run("round", func(t *testing.T) {
					runDataChannelFirstOnce(t, offererAddsTrack)
				})
			}
		})
	}
}

// TestPeerConnection_DataChannelOnly_NoMedia asserts that a media-less
// connection does not start the undeclared-media processing goroutines and
// shuts down without leaking routines.
func TestPeerConnection_DataChannelOnly_NoMedia(t *testing.T) {
	lim := test.TimeOut(30 * time.Second)
	defer lim.Stop()
	report := test.CheckRoutines(t)
	defer report()

	pcOffer, pcAnswer, err := newPair()
	require.NoError(t, err)

	dcOffer, err := pcOffer.CreateDataChannel("text", nil)
	require.NoError(t, err)

	dcOpen := make(chan struct{})
	dcOffer.OnOpen(func() { close(dcOpen) })

	dcAnswerCh := make(chan *DataChannel, 1)
	pcAnswer.OnDataChannel(func(d *DataChannel) { dcAnswerCh <- d })

	require.NoError(t, signalPairWithOptions(pcOffer, pcAnswer, withDisableInitialDataChannel(true)))

	select {
	case <-dcOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("data channel did not open")
	}
	select {
	case <-dcAnswerCh:
	case <-time.After(5 * time.Second):
		t.Fatal("answerer did not receive data channel")
	}

	// A media-less connection must not run any undeclared media processors.
	offerRTP, offerRTCP := pcUndeclaredProcessors(pcOffer)
	assert.Equal(t, 0, offerRTP, "offerer: data-only connection started undeclared RTP processor")
	assert.Equal(t, 0, offerRTCP, "offerer: data-only connection started undeclared RTCP processor")
	answerRTP, answerRTCP := pcUndeclaredProcessors(pcAnswer)
	assert.Equal(t, 0, answerRTP, "answerer: data-only connection started undeclared RTP processor")
	assert.Equal(t, 0, answerRTCP, "answerer: data-only connection started undeclared RTCP processor")

	closePairNow(t, pcOffer, pcAnswer)
}
