package exacq

import (
	"strings"
	"testing"
)

const testMotionOwner = "12345678-1234-1234-1234-123456789abc"

func TestEventLinkParsingIsConservative(t *testing.T) {
	base := `<eDVR><Events ID="655360"><Event ID="7" AssociationGUID="12345678-1234-1234-1234-123400070200" Log="1"><Devices ID="458752"><Device Number="2"><Video><Input Number="0" Motion="true"/></Video><Codec><Encoder Number="0"><Channel Number="0" Record="true"/></Encoder></Codec></Device></Devices></Event></Events></eDVR>`
	parsed, err := ParseConfig([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.EventLinks[0].Target != -1 {
		t.Fatal("unknown target treated as no action")
	}
	broken := strings.Replace(base, "</Event>", `<Schedule><Entry FirstHour="167" LastHour="168" AlarmEnabled="3"/></Schedule></Event>`, 1)
	if _, err = ParseConfig([]byte(broken)); err == nil {
		t.Fatal("out of bounds schedule accepted")
	}
}
