package exacq

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Captured from a fresh recorder without a key; no MAC or key attributes retained.
const emptyLicenseFixture = `<eDVR><Systems><License IsRestricted="0" MaxIPCameras="0" TotalIPCameras="0" AnalogCameras="0" Expiration="0"/></Systems></eDVR>`

func TestEmptyUnlicensedStatus(t *testing.T) {
	license, err := parseLicense([]byte(emptyLicenseFixture), false)
	require.NoError(t, err)
	require.Equal(t, &License{Status: "unlicensed"}, license)
	_, err = parseLicense([]byte(emptyLicenseFixture), true)
	require.ErrorContains(t, err, "incomplete license status", "a keyed response must retain the full status contract")
	for _, name := range []string{"IsRestricted", "MaxIPCameras", "TotalIPCameras", "AnalogCameras", "Expiration"} {
		for _, replacement := range []string{"", name + `="1"`, name + `="SECRET"`} {
			body := strings.Replace(emptyLicenseFixture, name+`="0"`, replacement, 1)
			_, err = parseLicense([]byte(body), false)
			require.Error(t, err, name)
			require.NotContains(t, err.Error(), "SECRET")
		}
	}
	for _, name := range []string{"IsBadLicense", "Trial", "Enterprise", "EnterpriseEx", "Time"} {
		body := strings.Replace(emptyLicenseFixture, "<License ", "<License "+name+`="0" `, 1)
		_, err = parseLicense([]byte(body), false)
		require.ErrorContains(t, err, "incomplete license status", name)
	}
}

func TestReadLicenseEmptyRecorder(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		t.Run(map[bool]string{false: "without key", true: "with key"}[keyed], func(t *testing.T) {
			client, server := sessionPair(t)
			done := make(chan error, 1)
			go func() {
				p, err := server.readPacket()
				if err != nil {
					done <- err
					return
				}
				if p.Type != packetLicense || p.Subtype != 1 {
					done <- errors.New("unexpected license request")
					return
				}
				key := make([]byte, 22)
				if keyed {
					key[21] = 1
				}
				if err = server.sendPacket(packetLicense, 2, key); err != nil {
					done <- err
					return
				}
				p, err = server.readPacket()
				if err != nil {
					done <- err
					return
				}
				if p.Type != packetStatus || p.Subtype != 1 {
					done <- errors.New("unexpected status request")
					return
				}
				if err = server.sendPacket(packetStatus, 2, []byte(`<eDVR><Servers/></eDVR>`)); err != nil {
					done <- err
					return
				}
				done <- server.sendPacket(packetStatus, 2, []byte(emptyLicenseFixture))
			}()
			license, err := client.ReadLicense(context.Background())
			if keyed {
				require.ErrorContains(t, err, "incomplete license status")
			} else {
				require.NoError(t, err)
				require.Equal(t, &License{Status: "unlicensed"}, license)
			}
			require.NoError(t, <-done)
		})
	}
}

func TestRefreshStatusEmptyRecorder(t *testing.T) {
	client, server := sessionPair(t)
	done := make(chan error, 1)
	go func() {
		if _, err := server.readPacket(); err != nil {
			done <- err
			return
		}
		for _, body := range []string{
			`<eDVR><Servers/></eDVR>`, `<eDVR><Analytics/></eDVR>`, `<eDVR><Users/></eDVR>`,
			strings.Replace(emptyLicenseFixture, "</eDVR>", `<Memory Throttling="0"/><Storage ID="1" RecordingMbps="0"/></eDVR>`, 1),
		} {
			if err := server.sendPacket(packetStatus, 2, []byte(body)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	status, err := client.RefreshStatus(context.Background())
	require.NoError(t, err)
	require.Len(t, status.Storage, 1)
	require.NotNil(t, status.MemoryThrottling)
	require.False(t, *status.MemoryThrottling)
	require.NoError(t, <-done)
}

func TestRefreshStatusRejectsIncompleteLicensedShape(t *testing.T) {
	for _, name := range []string{"IsBadLicense", "Trial", "IsRestricted", "Enterprise", "MaxIPCameras", "Expiration", "Time"} {
		t.Run(name, func(t *testing.T) {
			root, err := parseConfigXML([]byte(licenseFixture))
			require.NoError(t, err)
			value := root.child("Systems").child("License").attr(name)
			body := strings.Replace(licenseFixture, name+`="`+value+`"`, "", 1)
			client, server := sessionPair(t)
			done := make(chan error, 1)
			go func() {
				if _, err := server.readPacket(); err != nil {
					done <- err
					return
				}
				done <- server.sendPacket(packetStatus, 2, []byte(body))
			}()
			_, err = client.RefreshStatus(context.Background())
			require.ErrorContains(t, err, "incomplete license status")
			require.NoError(t, <-done)
		})
	}
}
