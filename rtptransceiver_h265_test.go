// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js

package webrtc

import (
	"strconv"
	"strings"
	"testing"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4/internal/fmtp"
	"github.com/stretchr/testify/assert"
)

func newDualH265MediaEngine(t *testing.T) *MediaEngine {
	t.Helper()
	mediaEngine := &MediaEngine{}
	videoRTCPFeedback := []RTCPFeedback{{"goog-remb", ""}, {"ccm", "fir"}, {"nack", ""}, {"nack", "pli"}}
	for _, codec := range []RTPCodecParameters{
		{
			RTPCodecCapability: RTPCodecCapability{
				MimeTypeH265, 90000, 0,
				"profile-id=1;tier-flag=0;tx-mode=SRST;level-id=90",
				videoRTCPFeedback,
			},
			PayloadType: 96,
		},
		{
			RTPCodecCapability: RTPCodecCapability{MimeTypeRTX, 90000, 0, "apt=96", nil},
			PayloadType:        97,
		},
		{
			RTPCodecCapability: RTPCodecCapability{
				MimeTypeH265, 90000, 0,
				"profile-id=1;tier-flag=0;tx-mode=SRST;level-id=120",
				videoRTCPFeedback,
			},
			PayloadType: 98,
		},
		{
			RTPCodecCapability: RTPCodecCapability{MimeTypeRTX, 90000, 0, "apt=98", nil},
			PayloadType:        99,
		},
	} {
		assert.NoError(t, mediaEngine.RegisterCodec(codec, RTPCodecTypeVideo))
	}

	return mediaEngine
}

func mustUnmarshalSDP(t *testing.T, raw string) *sdp.MediaDescription {
	t.Helper()
	parsed := sdp.SessionDescription{}
	assert.NoError(t, parsed.Unmarshal([]byte(raw)))

	return parsed.MediaDescriptions[0]
}

func transceiverCodecsFromRemoteMedia(t *testing.T, mediaEngine *MediaEngine, media *sdp.MediaDescription) []RTPCodecParameters {
	t.Helper()
	api := NewAPI(WithMediaEngine(mediaEngine))
	transceiver := newRTPTransceiver(nil, nil, RTPTransceiverDirectionRecvonly, RTPCodecTypeVideo, api)
	transceiver.setCodecPreferencesFromRemoteDescription(media)

	return transceiver.getCodecs()
}

func payloadTypes(codecs []RTPCodecParameters) []PayloadType {
	out := make([]PayloadType, 0, len(codecs))
	for _, codec := range codecs {
		out = append(out, codec.PayloadType)
	}

	return out
}

// Two H.265 media formats that only differ in level-id are compatible and must
// keep their distinct payload types when mapped to local codecs.
func TestSetCodecPreferencesRemoteDualH265KeepsPayloadTypes(t *testing.T) {
	const offerSDP = `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
t=0 0
m=video 9 UDP/TLS/RTP/SAVPF 96 97
a=mid:0
a=sendonly
a=rtpmap:96 H265/90000
a=fmtp:96 profile-id=1;tier-flag=0;tx-mode=SRST;level-id=90
a=rtpmap:97 H265/90000
a=fmtp:97 profile-id=1;tier-flag=0;tx-mode=SRST;level-id=120
`
	media := mustUnmarshalSDP(t, offerSDP)

	for range 50 {
		codecs := transceiverCodecsFromRemoteMedia(t, newDualH265MediaEngine(t), media)
		assert.Len(t, codecs, 2)
		assert.Equal(t, []PayloadType{96, 98}, payloadTypes(codecs))

		// each remote format keeps its own parameters and distinct payload type
		assert.Equal(t, "video/H265", codecs[0].MimeType)
		assert.True(t, strings.Contains(codecs[0].SDPFmtpLine, "level-id=90"))
		assert.Equal(t, "video/H265", codecs[1].MimeType)
		assert.True(t, strings.Contains(codecs[1].SDPFmtpLine, "level-id=120"))
	}
}

// RTX streams stay attached to their original video codec: no duplicated or
// crossed apt associations, and all four payload types stay distinct.
func TestSetCodecPreferencesRemoteDualH265WithRTX(t *testing.T) {
	const offerSDP = `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
t=0 0
m=video 9 UDP/TLS/RTP/SAVPF 96 97 98 99
a=mid:0
a=sendonly
a=rtpmap:96 H265/90000
a=fmtp:96 profile-id=1;tier-flag=0;tx-mode=SRST;level-id=90
a=rtpmap:97 RTX/90000
a=fmtp:97 apt=96
a=rtpmap:98 H265/90000
a=fmtp:98 profile-id=1;tier-flag=0;tx-mode=SRST;level-id=120
a=rtpmap:99 RTX/90000
a=fmtp:99 apt=98
`
	media := mustUnmarshalSDP(t, offerSDP)

	for range 50 {
		codecs := transceiverCodecsFromRemoteMedia(t, newDualH265MediaEngine(t), media)
		assert.Len(t, codecs, 4)
		assert.Equal(t, []PayloadType{96, 98, 97, 99}, payloadTypes(codecs))

		seen := map[PayloadType]bool{}
		aptFor := map[PayloadType]PayloadType{}
		for _, codec := range codecs {
			assert.False(t, seen[codec.PayloadType], "duplicated payload type %d", codec.PayloadType)
			seen[codec.PayloadType] = true

			if strings.EqualFold(codec.MimeType, MimeTypeRTX) {
				aptStr, ok := fmtp.Parse(codec.MimeType, codec.ClockRate, codec.Channels, codec.SDPFmtpLine).
					Parameter("apt")
				assert.True(t, ok)
				aptPT, err := strconv.ParseUint(aptStr, 10, 8)
				assert.NoError(t, err)
				aptFor[codec.PayloadType] = PayloadType(aptPT)
			}
		}
		assert.Equal(t, PayloadType(96), aptFor[97])
		assert.Equal(t, PayloadType(98), aptFor[99])
	}
}

// A single H.265 codec still negotiates as before.
func TestSetCodecPreferencesRemoteSingleH265(t *testing.T) {
	const offerSDP = `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
t=0 0
m=video 9 UDP/TLS/RTP/SAVPF 96 97
a=mid:0
a=sendonly
a=rtpmap:96 H265/90000
a=fmtp:96 profile-id=1;tier-flag=0;tx-mode=SRST;level-id=90
a=rtpmap:97 RTX/90000
a=fmtp:97 apt=96
`
	media := mustUnmarshalSDP(t, offerSDP)

	for range 50 {
		codecs := transceiverCodecsFromRemoteMedia(t, newDualH265MediaEngine(t), media)
		assert.Len(t, codecs, 2)
		assert.Equal(t, []PayloadType{96, 97}, payloadTypes(codecs))
		assert.Equal(t, MimeTypeRTX, codecs[1].MimeType)
		assert.Equal(t, "apt=96", codecs[1].SDPFmtpLine)
	}
}

// End-to-end: the generated answer SDP keeps four distinct payload types with
// correct RTX apt associations across repeated negotiation rounds.
func TestAnswerSDPDualH265PayloadTypesStable(t *testing.T) {
	const offerSDP = `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
t=0 0
a=group:BUNDLE 0
m=video 9 UDP/TLS/RTP/SAVPF 96 97 98 99
a=mid:0
a=ice-ufrag:uFrag
a=ice-pwd:password12345678901234567890
a=fingerprint:sha-256 75:74:5A:A6:A4:E5:52:F4:A7:67:4C:01:C7:EE:91:3F:21:3D:A2:E3:53:7B:6F:30:86:F2:30:AA:65:FB:04:24
a=sendonly
a=rtpmap:96 H265/90000
a=fmtp:96 profile-id=1;tier-flag=0;tx-mode=SRST;level-id=90
a=rtpmap:97 RTX/90000
a=fmtp:97 apt=96
a=rtpmap:98 H265/90000
a=fmtp:98 profile-id=1;tier-flag=0;tx-mode=SRST;level-id=120
a=rtpmap:99 RTX/90000
a=fmtp:99 apt=98
`
	expected := []string{
		"m=video 9 UDP/TLS/RTP/SAVPF 96 98 97 99",
		"a=rtpmap:96 H265/90000",
		"a=fmtp:96 profile-id=1;tier-flag=0;tx-mode=SRST;level-id=90",
		"a=rtpmap:98 H265/90000",
		"a=fmtp:98 profile-id=1;tier-flag=0;tx-mode=SRST;level-id=120",
		"a=rtpmap:97 RTX/90000",
		"a=fmtp:97 apt=96",
		"a=rtpmap:99 RTX/90000",
		"a=fmtp:99 apt=98",
	}

	for range 50 {
		api := NewAPI(WithMediaEngine(newDualH265MediaEngine(t)))
		pc, err := api.NewPeerConnection(Configuration{})
		assert.NoError(t, err)

		assert.NoError(t, pc.SetRemoteDescription(SessionDescription{Type: SDPTypeOffer, SDP: offerSDP}))
		answer, err := pc.CreateAnswer(nil)
		assert.NoError(t, err)

		for _, line := range expected {
			assert.True(t, strings.Contains(answer.SDP, line), "answer missing %q:\n%s", line, answer.SDP)
		}
		assert.NoError(t, pc.Close())
	}
}

// Other video formats with multiple registered variants keep distinct payload
// types and respect the remote negotiation order.
func TestSetCodecPreferencesRemoteMultiVariantOtherCodecs(t *testing.T) {
	t.Run("VP9 profiles", func(t *testing.T) {
		const offerSDP = `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
t=0 0
m=video 9 UDP/TLS/RTP/SAVPF 98 100
a=mid:0
a=sendonly
a=rtpmap:98 VP9/90000
a=fmtp:98 profile-id=0
a=rtpmap:100 VP9/90000
a=fmtp:100 profile-id=2
`
		mediaEngine := &MediaEngine{}
		assert.NoError(t, mediaEngine.RegisterCodec(RTPCodecParameters{
			RTPCodecCapability: RTPCodecCapability{MimeTypeVP9, 90000, 0, "profile-id=0", nil},
			PayloadType:        98,
		}, RTPCodecTypeVideo))
		assert.NoError(t, mediaEngine.RegisterCodec(RTPCodecParameters{
			RTPCodecCapability: RTPCodecCapability{MimeTypeVP9, 90000, 0, "profile-id=2", nil},
			PayloadType:        100,
		}, RTPCodecTypeVideo))

		for range 50 {
			codecs := transceiverCodecsFromRemoteMedia(t, mediaEngine, mustUnmarshalSDP(t, offerSDP))
			assert.Equal(t, []PayloadType{98, 100}, payloadTypes(codecs))
		}
	})

	t.Run("H264 packetization modes", func(t *testing.T) {
		const offerSDP = `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
t=0 0
m=video 9 UDP/TLS/RTP/SAVPF 102 104
a=mid:0
a=sendonly
a=rtpmap:102 H264/90000
a=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f
a=rtpmap:104 H264/90000
a=fmtp:104 level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f
`
		mediaEngine := &MediaEngine{}
		assert.NoError(t, mediaEngine.RegisterCodec(RTPCodecParameters{
			RTPCodecCapability: RTPCodecCapability{
				MimeTypeH264, 90000, 0,
				"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", nil,
			},
			PayloadType: 102,
		}, RTPCodecTypeVideo))
		assert.NoError(t, mediaEngine.RegisterCodec(RTPCodecParameters{
			RTPCodecCapability: RTPCodecCapability{
				MimeTypeH264, 90000, 0,
				"level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f", nil,
			},
			PayloadType: 104,
		}, RTPCodecTypeVideo))

		for range 50 {
			codecs := transceiverCodecsFromRemoteMedia(t, mediaEngine, mustUnmarshalSDP(t, offerSDP))
			assert.Equal(t, []PayloadType{102, 104}, payloadTypes(codecs))
		}
	})

	t.Run("AV1 profiles", func(t *testing.T) {
		const offerSDP = `v=0
o=- 1 1 IN IP4 127.0.0.1
s=-
t=0 0
m=video 9 UDP/TLS/RTP/SAVPF 45 47
a=mid:0
a=sendonly
a=rtpmap:45 AV1/90000
a=fmtp:45 profile=0
a=rtpmap:47 AV1/90000
a=fmtp:47 profile=1
`
		mediaEngine := &MediaEngine{}
		assert.NoError(t, mediaEngine.RegisterCodec(RTPCodecParameters{
			RTPCodecCapability: RTPCodecCapability{MimeTypeAV1, 90000, 0, "profile=0", nil},
			PayloadType:        45,
		}, RTPCodecTypeVideo))
		assert.NoError(t, mediaEngine.RegisterCodec(RTPCodecParameters{
			RTPCodecCapability: RTPCodecCapability{MimeTypeAV1, 90000, 0, "profile=1", nil},
			PayloadType:        47,
		}, RTPCodecTypeVideo))

		for range 50 {
			codecs := transceiverCodecsFromRemoteMedia(t, mediaEngine, mustUnmarshalSDP(t, offerSDP))
			assert.Equal(t, []PayloadType{45, 47}, payloadTypes(codecs))
		}
	})
}
