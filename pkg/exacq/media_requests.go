package exacq

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// liveRequest is the XML payload of a LIVE/REQUEST packet (5/1). The SDK's
// normal media mode uses ref=1 with camera CASIDs, not server stream IDs.
func liveRequest(casids []uint32, add bool) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	root := "Drop"
	if add {
		root = "Add"
	}
	fmt.Fprintf(&b, "<%s>", root)
	for _, id := range casids {
		fmt.Fprintf(&b, `<Stream id="%d" ref="1"/>`, id)
	}
	fmt.Fprintf(&b, "</%s>", root)
	return b.Bytes()
}

// searchRequest is a normal recorded-video STORED/REQUEST payload (6/1).
// Start and End have second precision. The caller selects any subsecond range
// from the returned media and retains preroll needed for decoding.
func searchRequest(camera uint32, start, end time.Time) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	fmt.Fprintf(&b, `<eDVR Start="%d" End="%d" Type="1"><Devices ID="%d"><Device Number="%d">`,
		start.Unix(), end.Unix(), camera&0x0fff0000, (camera>>8)&0xff)
	context := camera >> 28
	if context != 0 {
		fmt.Fprintf(&b, `<Context Number="%d">`, context)
	}
	fmt.Fprintf(&b, `<Video><Input Number="%d"/></Video>`, camera&0x7f)
	if context != 0 {
		b.WriteString("</Context>")
	}
	b.WriteString("</Device></Devices></eDVR>")
	return b.Bytes()
}

// SearchResult describes one recorded interval, including its session stream
// mapping. Count is the protocol packet count before filtering video preroll.
type SearchResult struct {
	Camera         uint32
	StreamID       uint32
	Start          time.Time
	End            time.Time
	Count          uint64
	Standard       int
	Width          int
	Height         int
	Flags          uint32
	Media          string
	SampleRate     int
	BufSizeSamples int
}

type searchResultXML struct {
	Start          string `xml:"Start,attr"`
	End            string `xml:"End,attr"`
	Count          string `xml:"Count,attr"`
	StreamID       string `xml:"StreamID,attr"`
	Standard       string `xml:"Standard,attr"`
	Width          string `xml:"Width,attr"`
	Height         string `xml:"Height,attr"`
	Flags          string `xml:"Flags,attr"`
	SampleRate     string `xml:"SampleRate,attr"`
	BufSizeSamples string `xml:"BufSizeSamples,attr"`
}

type searchInputXML struct {
	Number  *uint32           `xml:"Number,attr"`
	Results []searchResultXML `xml:"Result"`
}

type searchContextXML struct {
	Number *uint32          `xml:"Number,attr"`
	Video  []searchInputXML `xml:"Video>Input"`
	Audio  []searchInputXML `xml:"Audio>Input"`
}

type searchDeviceXML struct {
	Number   *uint32            `xml:"Number,attr"`
	Video    []searchInputXML   `xml:"Video>Input"`
	Audio    []searchInputXML   `xml:"Audio>Input"`
	Contexts []searchContextXML `xml:"Context"`
}

type searchDevicesXML struct {
	ID      *uint32           `xml:"ID,attr"`
	Devices []searchDeviceXML `xml:"Device"`
}

// parseSearch decodes STORED/RESPONSE XML. A valid empty response returns no
// results; malformed metadata returns an error and must not become clean EOF.
func parseSearch(payload []byte) ([]SearchResult, error) {
	if len(payload) > maxDecodedSize {
		return nil, errors.New("exacq: search XML exceeds size limit")
	}
	// Recorders emit this non-conforming declaration and may NUL-terminate XML.
	// Normalize only these known wire details; the body remains strict XML.
	payload = bytes.TrimPrefix(payload, []byte("<?xml ?>"))
	payload = bytes.TrimRight(payload, "\x00")
	var document struct {
		XMLName xml.Name           `xml:"eDVR"`
		Type    string             `xml:"Type,attr"`
		Devices []searchDevicesXML `xml:"Devices"`
	}
	decoder := xml.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("exacq: search XML: %w", err)
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("exacq: search XML: %w", err)
		}
		switch t := token.(type) {
		case xml.CharData:
			if len(bytes.TrimSpace(t)) == 0 {
				continue
			}
		case xml.Comment:
			continue
		}
		return nil, errors.New("exacq: trailing data after search XML")
	}
	if document.Type != "" && document.Type != "1" {
		return nil, fmt.Errorf("exacq: unsupported video search response type %q", document.Type)
	}
	var results []SearchResult
	appendInputs := func(plugin, device, context uint32, media string, inputs []searchInputXML) error {
		if plugin&0x0fff0000 != plugin || device > 255 || context > 15 {
			return errors.New("exacq: invalid search device identifier")
		}
		for _, input := range inputs {
			if input.Number == nil || *input.Number > 127 {
				return errors.New("exacq: invalid search input identifier")
			}
			camera := plugin | device<<8 | context<<28 | *input.Number
			if media == "audio" {
				camera |= 0x80
			}
			for _, raw := range input.Results {
				result, err := decodeSearchResult(raw)
				if err != nil {
					return err
				}
				result.Camera, result.Media = camera, media
				if media == "video" && (raw.Width == "" || raw.Height == "") {
					return errors.New("exacq: search video result lacks dimensions")
				}
				if media == "audio" && (raw.SampleRate == "" || raw.BufSizeSamples == "") {
					return errors.New("exacq: search audio result lacks sample metadata")
				}
				results = append(results, result)
			}
		}
		return nil
	}
	zero := uint32(0)
	for _, group := range document.Devices {
		if group.ID == nil {
			return nil, errors.New("exacq: missing search device group identifier")
		}
		for _, device := range group.Devices {
			if device.Number == nil {
				return nil, errors.New("exacq: missing search device identifier")
			}
			contexts := append([]searchContextXML{{Number: &zero, Video: device.Video, Audio: device.Audio}}, device.Contexts...)
			for _, context := range contexts {
				if context.Number == nil {
					return nil, errors.New("exacq: missing search context identifier")
				}
				if err := appendInputs(*group.ID, *device.Number, *context.Number, "video", context.Video); err != nil {
					return nil, err
				}
				if err := appendInputs(*group.ID, *device.Number, *context.Number, "audio", context.Audio); err != nil {
					return nil, err
				}
			}
		}
	}
	return results, nil
}

func decodeSearchResult(raw searchResultXML) (SearchResult, error) {
	var result SearchResult
	var seconds, endSeconds, count, streamID int64
	values := []struct {
		name string
		raw  string
		to   *int64
	}{
		{"Start", raw.Start, &seconds}, {"End", raw.End, &endSeconds},
		{"Count", raw.Count, &count}, {"StreamID", raw.StreamID, &streamID},
	}
	// ParseSearch in SDK 1.47.27.1 uses QueryIntValue for these attributes and
	// sign-extends Start/End. This XML contract differs from the unsigned
	// seconds field in tagStreamHeader; post-2038 XML is not yet established.
	for _, value := range values {
		n, err := strconv.ParseInt(value.raw, 10, 32)
		if err != nil {
			return result, fmt.Errorf("exacq: invalid search result %s", value.name)
		}
		*value.to = n
	}
	if seconds < 0 || endSeconds < seconds || count < 0 {
		return result, errors.New("exacq: invalid search result interval or count")
	}
	result.Start, result.End = time.Unix(seconds, 0).UTC(), time.Unix(endSeconds, 0).UTC()
	result.Count = uint64(count)
	if streamID < 0 {
		streamID = -streamID
	}
	result.StreamID = uint32(streamID)
	integers := []struct {
		name     string
		raw      string
		to       *int
		required bool
	}{
		{"Standard", raw.Standard, &result.Standard, true},
		{"Width", raw.Width, &result.Width, false},
		{"Height", raw.Height, &result.Height, false},
		{"SampleRate", raw.SampleRate, &result.SampleRate, false},
		{"BufSizeSamples", raw.BufSizeSamples, &result.BufSizeSamples, false},
	}
	for _, value := range integers {
		if value.raw == "" && !value.required {
			continue
		}
		n, err := strconv.ParseInt(value.raw, 10, 32)
		if err != nil || n < 0 {
			return result, fmt.Errorf("exacq: invalid search result %s", value.name)
		}
		*value.to = int(n)
	}
	if raw.Flags != "" {
		flags, err := strconv.ParseUint(raw.Flags, 10, 32)
		if err != nil {
			return result, errors.New("exacq: invalid search result Flags")
		}
		result.Flags = uint32(flags)
	}
	return result, nil
}
