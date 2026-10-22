package exacq

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestLiveRequest(t *testing.T) {
	for _, add := range []bool{false, true} {
		var doc struct {
			XMLName xml.Name
			Streams []struct {
				ID  uint32 `xml:"id,attr"`
				Ref int    `xml:"ref,attr"`
			} `xml:"Stream"`
		}
		if err := xml.Unmarshal(liveRequest([]uint32{459264, 0x20070201}, add), &doc); err != nil {
			t.Fatal(err)
		}
		want := "Drop"
		if add {
			want = "Add"
		}
		if doc.XMLName.Local != want || len(doc.Streams) != 2 || doc.Streams[0].ID != 459264 || doc.Streams[1].ID != 0x20070201 || doc.Streams[0].Ref != 1 || doc.Streams[1].Ref != 1 {
			t.Fatalf("incorrect live request: %+v", doc)
		}
	}
}

func TestSearchRequest(t *testing.T) {
	start, end := time.Unix(1700000000, 123456789), time.Unix(1700000010, 987654321)
	for _, camera := range []uint32{459264, 0x30070205} {
		payload := string(searchRequest(camera, start, end))
		if !strings.Contains(payload, `<eDVR Start="1700000000" End="1700000010" Type="1"><Devices ID="458752"><Device Number="2">`) {
			t.Fatal(payload)
		}
		if camera == 459264 {
			if !strings.Contains(payload, `<Video><Input Number="0"/></Video>`) || strings.Contains(payload, "Context") {
				t.Fatal(payload)
			}
		} else if !strings.Contains(payload, `<Context Number="3"><Video><Input Number="5"/></Video></Context>`) {
			t.Fatal(payload)
		}
	}
}

const searchFixture = `<eDVR Type="1"><Devices ID="458752"><Device Number="2">
<Video><Input Number="0">
<Result Start="1700000000" End="1700000002" Count="21" StreamID="-42" Standard="5" Width="1920" Height="1080" Flags="3"/>
<Result Start="1700000005" End="1700000007" Count="17" StreamID="43" Standard="6" Width="1280" Height="720"/>
</Input></Video>
<Context Number="3"><Video><Input Number="5"><Result Start="1700000008" End="1700000009" Count="10" StreamID="44" Standard="5" Width="640" Height="480"/></Input></Video></Context>
<Audio><Input Number="1"><Result Start="1700000001" End="1700000007" Count="8" StreamID="45" Standard="0" SampleRate="8000" BufSizeSamples="160"/></Input></Audio>
</Device></Devices></eDVR>`

func TestParseSearch(t *testing.T) {
	results, err := parseSearch([]byte(searchFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatal(results)
	}
	first, second := results[0], results[1]
	if first.Camera != 459264 || first.StreamID != 42 || first.Count != 21 || first.Standard != 5 || first.Flags != 3 || first.Width != 1920 || first.Height != 1080 || first.Media != "video" {
		t.Fatalf("first result: %+v", first)
	}
	if second.Start.Sub(first.End) != 3*time.Second || second.StreamID != 43 || second.Standard != 6 {
		t.Fatal("recording gap or stream change was lost")
	}
	var context, audio *SearchResult
	for i := range results {
		if results[i].Camera == 0x30070205 {
			context = &results[i]
		}
		if results[i].Media == "audio" {
			audio = &results[i]
		}
	}
	if context == nil || context.StreamID != 44 || audio == nil || audio.Camera != 0x70281 || audio.SampleRate != 8000 || audio.BufSizeSamples != 160 {
		t.Fatalf("context/audio mapping: %+v", results)
	}
	results, err = parseSearch([]byte(`<eDVR Type="1"/>`))
	if err != nil || len(results) != 0 {
		t.Fatalf("empty recording range: %+v %v", results, err)
	}
	results, err = parseSearch([]byte("<?xml ?>" + searchFixture + "\x00"))
	if err != nil || len(results) != 4 {
		t.Fatalf("vendor XML envelope: %+v %v", results, err)
	}
}

func TestParseSearchRejectsMalformed(t *testing.T) {
	for _, payload := range []string{
		`<eDVR>`, `<other/>`, `<eDVR/><eDVR/>`, `<eDVR/>garbage`,
		"<eDVR>\x00</eDVR>",
		strings.Replace(searchFixture, `Count="21"`, `Count="-1"`, 1),
		strings.Replace(searchFixture, `StreamID="-42"`, `StreamID="4294967296"`, 1),
		strings.Replace(searchFixture, `Start="1700000000"`, `Start="1700000100"`, 1),
		strings.Replace(searchFixture, `Width="1920"`, ``, 1),
		strings.Replace(searchFixture, `ID="458752"`, `ID="458753"`, 1),
		strings.Replace(searchFixture, `Number="2"`, `Number="256"`, 1),
		strings.Replace(searchFixture, `Number="5"`, `Number="128"`, 1),
		strings.Replace(searchFixture, ` ID="458752"`, ``, 1),
		strings.Replace(searchFixture, ` Number="2"`, ``, 1),
		strings.Replace(searchFixture, ` Number="3"`, ``, 1),
		strings.Replace(searchFixture, ` Number="0"`, ``, 1),
	} {
		if _, err := parseSearch([]byte(payload)); err == nil {
			t.Fatalf("accepted malformed search XML: %s", payload)
		}
	}
}

func FuzzParseSearch(f *testing.F) {
	f.Add([]byte(searchFixture))
	f.Add([]byte(`<eDVR Type="1"/>`))
	f.Add([]byte(`<eDVR>`))
	f.Add([]byte("<?xml ?>" + searchFixture + "\x00"))
	f.Fuzz(func(t *testing.T, payload []byte) {
		results, err := parseSearch(payload)
		if err != nil {
			return
		}
		for _, result := range results {
			if result.End.Before(result.Start) {
				t.Fatal("inverted range")
			}
		}
	})
}
