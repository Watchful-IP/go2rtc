package core

import (
	"encoding/binary"
	"time"
)

// Limits apply only to complete raw H264/H265 pictures in lossy live senders.
// One blocked transport write is outside this queue and cannot be retracted.
const pictureQueueMaxBytes = 8 << 20
const pictureQueueMaxAge = 2 * time.Second

type queuedPacket struct {
	Packet *Packet
	At     time.Time
}

// pictureKind reports whether a picture can start decoding (IDR/BLA/CRA, or
// H264 IDR) and whether it is an HEVC RASL picture, which a CRA start cannot decode.
func pictureKind(codec string, b []byte) (randomAccess, cra, rasl bool) {
	for len(b) >= 4 {
		n := int(binary.BigEndian.Uint32(b))
		b = b[4:]
		if n < 1 || n > len(b) {
			return false, false, false
		}
		if codec == CodecH264 && b[0]&31 == 5 {
			return true, false, false
		}
		if codec == CodecH265 && n >= 2 {
			switch typ := b[0] >> 1 & 63; {
			case typ >= 16 && typ <= 21:
				return true, typ == 21, false
			case typ == 8 || typ == 9:
				return false, false, true
			case typ < 32:
				return false, false, false // first VCL NAL decides
			}
		}
		b = b[n:]
	}
	return false, false, false
}

func (s *Sender) discardPictures() {
	s.waitKey = true
	for {
		select {
		case item, ok := <-s.buf:
			if !ok {
				return
			}
			s.dequeued(item)
			s.Drops++
		default:
			return
		}
	}
}

// dequeued releases an item's accounting; queueTimes mirrors the channel's FIFO order.
func (s *Sender) dequeued(item queuedPacket) {
	s.queueBytes -= len(item.Packet.Payload)
	if len(s.queueTimes) > 0 {
		s.queueTimes = s.queueTimes[1:]
	}
}

func (s *Sender) enqueuePicture(packet *Packet) {
	now := time.Now()
	if s.queueNow != nil {
		now = s.queueNow()
	}
	if len(s.buf) == cap(s.buf) || s.queueBytes+len(packet.Payload) > pictureQueueMaxBytes ||
		len(s.queueTimes) > 0 && now.Sub(s.queueTimes[0]) > pictureQueueMaxAge {
		s.discardPictures()
	}
	key, cra, rasl := pictureKind(s.pictureCodec, packet.Payload)
	if len(packet.Payload) > pictureQueueMaxBytes || s.waitKey && !key || s.skipRASL && rasl {
		s.Drops++
		return
	}
	if s.waitKey {
		s.waitKey = false
		// Leading pictures after a CRA start reference pictures we never sent.
		s.skipRASL = cra
	} else if s.skipRASL && !rasl {
		s.skipRASL = false
	}
	s.queueBytes += len(packet.Payload)
	s.queueTimes = append(s.queueTimes, now)
	s.buf <- queuedPacket{packet, now}
	s.Bytes += len(packet.Payload)
	s.Packets++
}
