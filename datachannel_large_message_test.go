// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

package webrtc

import (
	"crypto/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const largeMessageTestTimeout = 10 * time.Second

// watchForDataChannelOpen registers an OnOpen handler and returns a channel
// that is closed once the DataChannel opens. The handler must be registered
// before signaling so the event cannot be missed.
func watchForDataChannelOpen(dc *DataChannel) <-chan struct{} {
	opened := make(chan struct{})
	var once sync.Once
	dc.OnOpen(func() {
		once.Do(func() { close(opened) })
	})

	return opened
}

func waitForDataChannelOpen(t *testing.T, opened <-chan struct{}) {
	t.Helper()

	select {
	case <-opened:
	case <-time.After(largeMessageTestTimeout):
		t.Fatal("timed out waiting for the data channel to open")
	}
}

// TestDataChannelLargeMessageBoundaries asserts that SCTP messages around the
// historical 65535 byte boundary are delivered intact, in both directions and
// in back-to-back bursts.
func TestDataChannelLargeMessageBoundaries(t *testing.T) {
	tests := []struct {
		name string
		size int
	}{
		{name: "MaxUint16", size: 65535},
		{name: "MaxUint16PlusOne", size: 65536},
		{name: "TwoMaxUint16", size: 131070},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			offerPC, answerPC, err := newPair()
			require.NoError(t, err)
			defer closePairNow(t, offerPC, answerPC)

			offerDC, err := offerPC.CreateDataChannel("boundary", nil)
			require.NoError(t, err)
			opened := watchForDataChannelOpen(offerDC)

			received, echo := attachEcho(answerPC, "boundary")
			defer close(echo)
			require.NoError(t, signalPair(offerPC, answerPC))
			waitForDataChannelOpen(t, opened)

			message := make([]byte, tc.size)
			_, err = rand.Read(message)
			require.NoError(t, err)

			assertDelivered(t, offerDC, received, message)
		})
	}

	t.Run("Burst", func(t *testing.T) {
		offerPC, answerPC, err := newPair()
		require.NoError(t, err)
		defer closePairNow(t, offerPC, answerPC)

		offerDC, err := offerPC.CreateDataChannel("burst", nil)
		require.NoError(t, err)
		opened := watchForDataChannelOpen(offerDC)

		received, echo := attachEcho(answerPC, "burst")
		defer close(echo)
		require.NoError(t, signalPair(offerPC, answerPC))
		waitForDataChannelOpen(t, opened)

		sizes := []int{65535, 65536, 131070, 65536, 65535}
		messages := make([][]byte, len(sizes))
		for i, size := range sizes {
			messages[i] = make([]byte, size)
			_, err = rand.Read(messages[i])
			require.NoError(t, err)
			require.NoError(t, offerDC.Send(messages[i]))
		}

		for i := range messages {
			assertReceived(t, received, messages[i], i)
		}
	})
}

// attachEcho installs an OnMessage handler on the remote side which forwards
// every payload to the returned channel. The returned channel must be closed
// by the caller to release the handler goroutine.
func attachEcho(pc *PeerConnection, label string) (<-chan []byte, chan struct{}) {
	received := make(chan []byte, 16)
	done := make(chan struct{})

	pc.OnDataChannel(func(d *DataChannel) {
		if d.Label() != label {
			return
		}
		d.OnMessage(func(m DataChannelMessage) {
			select {
			case received <- m.Data:
			case <-done:
			}
		})
	})

	return received, done
}

func assertDelivered(t *testing.T, dc *DataChannel, received <-chan []byte, message []byte) {
	t.Helper()

	require.NoError(t, dc.Send(message))
	assertReceived(t, received, message, 0)
}

func assertReceived(t *testing.T, received <-chan []byte, expected []byte, index int) {
	t.Helper()

	select {
	case got := <-received:
		assert.Equal(t, expected, got)
	case <-time.After(largeMessageTestTimeout):
		t.Fatalf("timed out waiting for message %d (%d bytes)", index, len(expected))
	}
}

// TestDataChannelHonorsRemoteMaxMessageSize asserts that a sender respects the
// max-message-size advertised by the remote peer instead of silently dropping
// oversized messages.
func TestDataChannelHonorsRemoteMaxMessageSize(t *testing.T) {
	const remoteLimit = 4321

	settingEngine := SettingEngine{}
	settingEngine.SetSCTPMaxMessageSize(remoteLimit)

	offerPC, err := NewPeerConnection(Configuration{})
	require.NoError(t, err)

	answerPC, err := NewAPI(WithSettingEngine(settingEngine)).NewPeerConnection(Configuration{})
	require.NoError(t, err)
	defer closePairNow(t, offerPC, answerPC)

	offerDC, err := offerPC.CreateDataChannel("limited", nil)
	require.NoError(t, err)
	opened := watchForDataChannelOpen(offerDC)

	received, echo := attachEcho(answerPC, "limited")
	defer close(echo)

	require.NoError(t, signalPair(offerPC, answerPC))
	waitForDataChannelOpen(t, opened)

	// The peer that declared the limit is the one we must not exceed.
	require.Equal(t, uint32(remoteLimit), offerPC.SCTP().GetCapabilities().MaxMessageSize)

	// Sending more than the remote declared must fail loudly, not be dropped.
	require.Error(t, offerDC.Send(make([]byte, remoteLimit+1)))

	// A message within the declared limit is delivered intact.
	withinLimit := make([]byte, remoteLimit)
	_, err = rand.Read(withinLimit)
	require.NoError(t, err)

	require.NoError(t, offerDC.Send(withinLimit))
	assertReceived(t, received, withinLimit, 0)
}

// TestGrowMessageBuffer asserts the receive-side buffer never grows past the
// largest message this peer declared it is willing to accept.
func TestGrowMessageBuffer(t *testing.T) {
	const unsetLimit = sctpMaxMessageSizeUnsetValue

	tests := []struct {
		name        string
		currentSize int
		maxSize     uint32
		expected    int
		ok          bool
	}{
		{name: "Grows", currentSize: unsetLimit, maxSize: 1 << 20, expected: 2 * unsetLimit, ok: true},
		{name: "ClampsToLimit", currentSize: 700_000, maxSize: 1 << 20, expected: 1 << 20, ok: true},
		{name: "AtLimit", currentSize: 1 << 20, maxSize: 1 << 20, ok: false},
		{name: "AboveLimit", currentSize: 2 << 20, maxSize: 1 << 20, ok: false},
		{name: "ZeroLimit", currentSize: unsetLimit, maxSize: 0, ok: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			next, ok := growMessageBuffer(tc.currentSize, tc.maxSize)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.expected, next)
		})
	}
}
