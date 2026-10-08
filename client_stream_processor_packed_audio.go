package gohlslib

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"reflect"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/ac3"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg1audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

// Specification: RFC 8216, section 3.4
const packedAudioTimestampOwner = "com.apple.streaming.transportStreamTimestamp"

type packedAudioFrame struct {
	codec       codecs.Codec
	sampleRate  int
	sampleCount int
	data        []byte
}

func id3SyncSafe(buf []byte) int {
	return int(buf[0])<<21 | int(buf[1])<<14 | int(buf[2])<<7 | int(buf[3])
}

func isID3Tag(buf []byte) bool {
	return bytes.HasPrefix(buf, []byte("ID3"))
}

// id3TagLen returns the length of the ID3v2 tag at the start of buf, footer included.
func id3TagLen(buf []byte) (int, error) {
	if len(buf) < 10 {
		return 0, fmt.Errorf("ID3 tag is too short")
	}

	l := 10 + id3SyncSafe(buf[6:10])

	if (buf[5] & 0x10) != 0 {
		l += 10
	}

	if len(buf) < l {
		return 0, fmt.Errorf("ID3 tag is too short")
	}

	return l, nil
}

// id3Timestamp returns the 33-bit timestamp of the PRIV frame of an ID3v2.3 or ID3v2.4 tag.
func id3Timestamp(tag []byte) (int64, error) {
	version := tag[3]
	if version != 3 && version != 4 {
		return 0, fmt.Errorf("unsupported ID3 version: %d", version)
	}

	frames := tag[10 : 10+id3SyncSafe(tag[6:10])]

	if (tag[5] & 0x40) != 0 {
		if len(frames) < 4 {
			return 0, fmt.Errorf("ID3 extended header is too short")
		}

		var l int64
		if version == 4 {
			l = int64(id3SyncSafe(frames))
		} else {
			l = 4 + int64(binary.BigEndian.Uint32(frames))
		}

		if int64(len(frames)) < l {
			return 0, fmt.Errorf("ID3 extended header is too short")
		}
		frames = frames[l:]
	}

	for len(frames) >= 10 && frames[0] != 0 {
		var size int64
		if version == 4 {
			size = int64(id3SyncSafe(frames[4:8]))
		} else {
			size = int64(binary.BigEndian.Uint32(frames[4:8]))
		}

		if int64(len(frames)) < 10+size {
			return 0, fmt.Errorf("ID3 frame is too short")
		}

		id, body := string(frames[:4]), frames[10:10+size]
		frames = frames[10+size:]

		owner, payload, found := bytes.Cut(body, []byte{0})
		if id != "PRIV" || !found || string(owner) != packedAudioTimestampOwner {
			continue
		}

		if len(payload) != 8 {
			return 0, fmt.Errorf("invalid timestamp length: %d", len(payload))
		}

		return int64(binary.BigEndian.Uint64(payload) & 0x1FFFFFFFF), nil
	}

	return 0, fmt.Errorf("ID3 tag doesn't contain a %s PRIV frame", packedAudioTimestampOwner)
}

func unmarshalADTSFrame(buf []byte) (*packedAudioFrame, int, error) {
	if len(buf) < 7 {
		return nil, 0, fmt.Errorf("ADTS frame is too short")
	}

	l := int(buf[3]&0x03)<<11 | int(buf[4])<<3 | int(buf[5])>>5
	if len(buf) < l {
		return nil, 0, fmt.Errorf("ADTS frame is too short")
	}

	var pkts mpeg4audio.ADTSPackets
	err := pkts.Unmarshal(buf[:l])
	if err != nil {
		return nil, 0, err
	}

	pkt := pkts[0]

	return &packedAudioFrame{
		codec: &codecs.MPEG4Audio{
			Config: mpeg4audio.AudioSpecificConfig{
				Type:          pkt.Type,
				SampleRate:    pkt.SampleRate,
				ChannelConfig: pkt.ChannelConfig,
				ChannelCount:  pkt.ChannelCount, //nolint:staticcheck
			},
		},
		sampleRate:  pkt.SampleRate,
		sampleCount: mpeg4audio.SamplesPerAccessUnit,
		data:        pkt.AU,
	}, l, nil
}

func unmarshalMPEG1AudioFrame(buf []byte) (*packedAudioFrame, int, error) {
	var h mpeg1audio.FrameHeader
	err := h.Unmarshal(buf)
	if err != nil {
		return nil, 0, err
	}

	l := h.FrameLen()
	if len(buf) < l {
		return nil, 0, fmt.Errorf("MPEG-1 Audio frame is too short")
	}

	return &packedAudioFrame{
		codec:       &codecs.MPEG1Audio{},
		sampleRate:  h.SampleRate,
		sampleCount: h.SampleCount(),
		data:        buf[:l],
	}, l, nil
}

func unmarshalAC3Frame(buf []byte) (*packedAudioFrame, int, error) {
	var syncInfo ac3.SyncInfo
	err := syncInfo.Unmarshal(buf)
	if err != nil {
		return nil, 0, err
	}

	l := syncInfo.FrameSize()
	if len(buf) < l {
		return nil, 0, fmt.Errorf("AC-3 frame is too short")
	}

	var bsi ac3.BSI
	err = bsi.Unmarshal(buf[5:l])
	if err != nil {
		return nil, 0, err
	}

	return &packedAudioFrame{
		codec: &codecs.AC3{
			SampleRate:   syncInfo.SampleRate(),
			ChannelCount: bsi.ChannelCount(),
		},
		sampleRate:  syncInfo.SampleRate(),
		sampleCount: ac3.SamplesPerFrame,
		data:        buf[:l],
	}, l, nil
}

func unmarshalPackedAudioFrame(buf []byte) (*packedAudioFrame, int, error) {
	switch {
	// E-AC-3 shares the AC-3 sync word and has a bsid greater than 10
	case len(buf) >= 6 && buf[0] == 0x0B && buf[1] == 0x77 && (buf[5]>>3) > 10:
		return nil, 0, fmt.Errorf("E-AC-3 is not supported")

	case len(buf) >= 2 && buf[0] == 0x0B && buf[1] == 0x77:
		return unmarshalAC3Frame(buf)

	case len(buf) >= 2 && buf[0] == 0xFF && (buf[1]&0xF6) == 0xF0:
		return unmarshalADTSFrame(buf)

	case len(buf) >= 2 && buf[0] == 0xFF && (buf[1]&0xE0) == 0xE0:
		return unmarshalMPEG1AudioFrame(buf)

	default:
		return nil, 0, fmt.Errorf("unsupported packed audio frame")
	}
}

// unmarshalPackedAudio returns the timestamp of the first sample of a Packed Audio Segment and its frames.
func unmarshalPackedAudio(buf []byte) (int64, []*packedAudioFrame, error) {
	l, err := id3TagLen(buf)
	if err != nil {
		return 0, nil, err
	}

	pts, err := id3Timestamp(buf[:l])
	if err != nil {
		return 0, nil, err
	}

	buf = buf[l:]

	var frames []*packedAudioFrame

	for len(buf) != 0 {
		if isID3Tag(buf) {
			l, err = id3TagLen(buf)
			if err != nil {
				return 0, nil, err
			}

			buf = buf[l:]
			continue
		}

		var frame *packedAudioFrame
		frame, l, err = unmarshalPackedAudioFrame(buf)
		if err != nil {
			return 0, nil, err
		}

		frames = append(frames, frame)
		buf = buf[l:]
	}

	if len(frames) == 0 {
		return 0, nil, fmt.Errorf("packed audio segment doesn't contain any frame")
	}

	return pts, frames, nil
}

type clientStreamProcessorPackedAudio struct {
	isLeading        bool
	rendition        *playlist.MultivariantRendition
	segmentQueue     *clientSegmentQueue
	rp               *clientRoutinePool
	streamDownloader clientStreamProcessorStreamDownloader
	client           clientStreamDownloaderClient

	codec          codecs.Codec
	sampleRate     int
	trackProcessor *clientTrackProcessorMPEGTS
	timeConv       *clientTimeConvMPEGTS

	chTrackProcessorDone chan struct{}
}

func (p *clientStreamProcessorPackedAudio) initialize() {
	p.chTrackProcessorDone = make(chan struct{})
}

func (p *clientStreamProcessorPackedAudio) run(ctx context.Context) error {
	for {
		seg, ok := p.segmentQueue.pull(ctx)
		if !ok {
			return fmt.Errorf("terminated")
		}

		if seg.err != nil {
			p.streamDownloader.onProcessorError(ctx, seg.err)
			<-ctx.Done()
			return fmt.Errorf("terminated")
		}

		err := p.processSegment(ctx, seg)
		if err != nil {
			return err
		}
	}
}

func (p *clientStreamProcessorPackedAudio) processSegment(ctx context.Context, seg *segmentData) error {
	rawPTS, frames, err := unmarshalPackedAudio(seg.payload)
	if err != nil {
		return err
	}

	if p.trackProcessor == nil {
		err = p.initializeTrackProcessor(ctx, frames[0])
		if err != nil {
			return err
		}
	}

	if p.timeConv == nil {
		err = p.initializeTimeConv(ctx, rawPTS)
		if err != nil {
			return err
		}
	}

	sampleCount := int64(0)

	for i, frame := range frames {
		if !reflect.DeepEqual(frame.codec, p.codec) || frame.sampleRate != p.sampleRate {
			return fmt.Errorf("packed audio codec parameters changed")
		}

		pts := p.timeConv.convert((rawPTS + sampleCount*90000/int64(p.sampleRate)) & 0x1FFFFFFFF)
		sampleCount += int64(frame.sampleCount)

		if i == 0 && p.isLeading {
			if seg.dateTime != nil {
				p.timeConv.setNTP(*seg.dateTime, pts)
			}
			p.timeConv.setLeadingNTPReceived()
		}

		err = p.trackProcessor.push(ctx, &procEntryMPEGTS{
			pts:  pts,
			dts:  pts,
			ntp:  p.timeConv.getNTP(ctx, pts),
			data: [][]byte{frame.data},
		})
		if err != nil {
			return err
		}
	}

	err = p.trackProcessor.push(ctx, nil)
	if err != nil {
		return err
	}

	select {
	case <-p.chTrackProcessorDone:
	case <-ctx.Done():
	}

	return nil
}

func (p *clientStreamProcessorPackedAudio) onPartProcessorDone(ctx context.Context) {
	select {
	case p.chTrackProcessorDone <- struct{}{}:
	case <-ctx.Done():
	}
}

func (p *clientStreamProcessorPackedAudio) initializeTrackProcessor(
	ctx context.Context,
	frame *packedAudioFrame,
) error {
	track := &Track{
		Codec:     frame.codec,
		ClockRate: 90000,
	}

	if !p.isLeading {
		track.Name = p.rendition.Name
		track.Language = p.rendition.Language
		track.IsDefault = p.rendition.Default
	}

	streamTracks, ok := p.streamDownloader.setTracks(ctx, []*Track{track})
	if !ok {
		return fmt.Errorf("terminated")
	}

	p.codec = frame.codec
	p.sampleRate = frame.sampleRate

	p.trackProcessor = &clientTrackProcessorMPEGTS{
		track:           streamTracks[0],
		streamProcessor: p,
	}
	p.trackProcessor.initialize()
	p.rp.add(p.trackProcessor)

	return nil
}

func (p *clientStreamProcessorPackedAudio) initializeTimeConv(ctx context.Context, startDTS int64) error {
	if p.isLeading {
		p.timeConv = &clientTimeConvMPEGTS{
			startDTS: startDTS,
		}
		p.timeConv.initialize()

		p.client.setTimeConv(p.timeConv)
	} else {
		tmp, ok := p.client.waitTimeConv(ctx)
		if !ok {
			return fmt.Errorf("terminated")
		}

		p.timeConv, ok = tmp.(*clientTimeConvMPEGTS)
		if !ok {
			return fmt.Errorf("stream playlists are mixed packed audio/FMP4")
		}
	}

	return nil
}
