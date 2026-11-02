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

func randomAccessPicture(codec string, b []byte) bool {
	for len(b) >= 4 {
		n := int(binary.BigEndian.Uint32(b))
		b = b[4:]
		if n < 1 || n > len(b) {
			return false
		}
		if codec == CodecH264 && b[0]&31 == 5 {
			return true
		}
		// Only IDR/BLA are independent of preceding pictures. CRA may have RASL.
		if codec == CodecH265 && n >= 2 {
			typ := b[0] >> 1 & 63
			if typ >= 16 && typ <= 20 {
				return true
			}
		}
		b = b[n:]
	}
	return false
}

func (s *Sender) discardPictures() {
	s.waitKey = true
	for {
		select {
		case item, ok := <-s.buf:
			if !ok {
				return
			}
			s.queueBytes -= len(item.Packet.Payload)
			s.Drops++
		default:
			return
		}
	}
}

func (s *Sender) enqueuePicture(packet *Packet) {
	now := time.Now()
	if s.queueNow != nil {
		now = s.queueNow()
	}
	// Track the oldest queued arrival without taking data from the channel.
	// After a dequeue, oldest may be older than reality: this is conservative.
	if len(s.buf) == 0 {
		s.queueOldest = now
	}
	if len(s.buf) == cap(s.buf) || s.queueBytes+len(packet.Payload) > pictureQueueMaxBytes || now.Sub(s.queueOldest) > pictureQueueMaxAge {
		s.discardPictures()
	}
	key := randomAccessPicture(s.pictureCodec, packet.Payload)
	if len(packet.Payload) > pictureQueueMaxBytes || s.waitKey && !key {
		s.Drops++
		return
	}
	if s.waitKey {
		s.waitKey = false
		s.queueOldest = now
	}
	s.queueBytes += len(packet.Payload)
	s.buf <- queuedPacket{packet, now}
	s.Bytes += len(packet.Payload)
	s.Packets++
}
