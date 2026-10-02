package milestone

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func readAll(t *testing.T, stream string) ([]*response, error) {
	rd := newReader(strings.NewReader(stream))
	var out []*response
	for {
		res, err := rd.read()
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
}

func TestReaderMessages(t *testing.T) {
	stream := `<?xml version="1.0"?><methodresponse><methodname>connect</methodname><connected>yes</connected></methodresponse>` + "\r\n\r\n" +
		"ImageResponse\r\nRequestId: 2\r\nPrev: 0\r\nCurrent: 0\r\nNext: 0\r\nContent-length: 0\r\nContent-type: application/x-genericbytedata-octet-stream\r\n\r\n\r\n\r\n" +
		"ImageResponse\r\nRequestId: 3\r\nCurrent: 5\r\nContent-length: 3\r\n\r\nabc\r\n\r\n" +
		"ImageResponse\r\nRequestId: 4\r\nCurrent: 6\r\nContent-type: multipart/related; boundary=b0; type=\"application/x-genericbytedata-octet-stream\"\r\n\r\n" +
		"--b0\r\nContent-length: 2\r\nContent-type: x\r\n\r\nde\r\n--b0\r\nContent-length: 1\r\n\r\nf\r\n--b0--\r\n\r\n"
	res, err := readAll(t, stream)
	require.EqualError(t, err, "EOF")
	require.Len(t, res, 4)
	require.Equal(t, "yes", xmlValue(res[0].xml, "connected"))
	require.Empty(t, res[1].parts, "empty database")
	require.Equal(t, [][]byte{[]byte("abc")}, res[2].parts)
	require.Equal(t, int64(5), res[2].int64("current"))
	require.Equal(t, [][]byte{[]byte("de"), []byte("f")}, res[3].parts)
}

func TestReaderRejectsDesync(t *testing.T) {
	for name, stream := range map[string]string{
		"garbage":         "HTTP/1.1 200 OK\r\n\r\n",
		"bad length":      "ImageResponse\r\nContent-length: x\r\n\r\n",
		"oversized":       "ImageResponse\r\nContent-length: 999999999\r\n\r\n",
		"missing trailer": "ImageResponse\r\nContent-length: 1\r\n\r\naXXXX",
		"bad boundary":    "ImageResponse\r\nContent-type: multipart/related; boundary=b\r\n\r\n--c\r\n",
		"endless header":  "ImageResponse\r\n" + strings.Repeat("A: b\r\n", 100),
	} {
		_, err := readAll(t, stream)
		require.ErrorIs(t, err, errDesync, name)
	}
}

func gbdPacket(codec uint16, flags uint16, at int64, payload []byte) []byte {
	b := make([]byte, videoHeaderSize, videoHeaderSize+len(payload))
	binary.BigEndian.PutUint16(b, gbdVideoPacket)
	binary.BigEndian.PutUint32(b[2:], uint32(videoHeaderSize+len(payload)))
	binary.BigEndian.PutUint16(b[6:], codec)
	binary.BigEndian.PutUint16(b[10:], flags)
	binary.BigEndian.PutUint64(b[12:], uint64(at))
	binary.BigEndian.PutUint64(b[20:], uint64(at))
	return append(b, payload...)
}

func TestParseFrames(t *testing.T) {
	video := gbdPacket(codecH264, flagSync, 1000, []byte{0, 0, 0, 1, 0x65, 1})
	audio := make([]byte, 42)
	binary.BigEndian.PutUint16(audio, gbdAudioPacket)
	binary.BigEndian.PutUint32(audio[2:], 42)
	multi := make([]byte, multiHeaderSize)
	binary.BigEndian.PutUint16(multi, gbdMultiPacket)
	part := bytes.Join([][]byte{multi, video, audio}, nil)

	fs, err := parseFrames(part, nil)
	require.NoError(t, err)
	require.Len(t, fs, 1, "audio is skipped")
	require.Equal(t, frame{codec: codecH264, key: true, sync: 1000, time: 1000, data: []byte{0, 0, 0, 1, 0x65, 1}}, fs[0])

	fs, err = parseFrames([]byte{0xFF, 0xD8, 0xFF, 0xE0}, nil)
	require.NoError(t, err)
	require.Equal(t, uint16(codecJPEG), fs[0].codec, "bare JPEG from recorder-side transcoding")

	block := make([]byte, 36)
	binary.BigEndian.PutUint16(block, 0x000A)
	_, err = parseFrames(block, nil)
	require.ErrorIs(t, err, errVideoBlock)

	_, err = parseFrames(video[:20], nil)
	require.ErrorIs(t, err, errDesync, "truncated header")
	_, err = parseFrames(video[:len(video)-1], nil)
	require.ErrorIs(t, err, errDesync, "truncated payload")

	zero := gbdPacket(codecH264, 0, 0, nil)
	binary.BigEndian.PutUint16(zero, gbdAudioPacket)
	binary.BigEndian.PutUint32(zero[2:], 0)
	_, err = parseFrames(zero, nil)
	require.ErrorIs(t, err, errDesync, "zero length cannot advance")
}
