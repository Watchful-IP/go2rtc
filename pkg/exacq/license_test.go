package exacq

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const licenseFixture = `<eDVR><Systems><License IsBadLicense="0" Trial="1" IsRestricted="0" Enterprise="1" EnterpriseEx="1" MaxIPCameras="4" Expiration="1796342400" Time="1792544000" Binary="SECRET-KEY-MUST-NOT-ESCAPE" Mac="SECRET-MAC"/></Systems></eDVR>`

func TestLicenseStatus(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, status string
		key, expires           bool
	}{
		{"trial", "", "", "trial", true, true},
		{"invalid", `IsBadLicense="0"`, `IsBadLicense="1"`, "invalid", true, true},
		{"expiredTrial", `Time="1792544000"`, `Time="1796342400"`, "expired", true, false},
		{"unlicensed", "", "", "unlicensed", false, true},
		{"licensed", `Trial="1"`, `Trial="0"`, "licensed", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := licenseFixture
			if tc.from != "" {
				body = strings.ReplaceAll(body, tc.from, tc.to)
			}
			license, err := parseLicense([]byte(body), tc.key)
			require.NoError(t, err)
			require.Equal(t, tc.status, license.Status)
			require.Equal(t, tc.key, license.Enterprise)
			require.EqualValues(t, 4, license.IPCameraSlots)
			require.Equal(t, tc.expires, license.ExpiresAt != nil)
		})
	}
	body := strings.ReplaceAll(strings.ReplaceAll(licenseFixture, `Trial="1"`, `Trial="0"`), `IsRestricted="0"`, `IsRestricted="1"`)
	license, err := parseLicense([]byte(body), true)
	require.NoError(t, err)
	require.Equal(t, "restricted", license.Status)
	body = strings.ReplaceAll(strings.ReplaceAll(licenseFixture, `Trial="1"`, `Trial="0"`), `Expiration="1796342400"`, `Expiration="0"`)
	license, err = parseLicense([]byte(body), true)
	require.NoError(t, err)
	require.Equal(t, "expired", license.Status, "match the SDK's explicit Time >= Expiration comparison")
	require.Nil(t, license.ExpiresAt)
	for _, body := range []string{
		`<eDVR/>`, `<eDVR><Systems><License/></Systems></eDVR>`,
		strings.ReplaceAll(licenseFixture, `Time="1792544000"`, ""),
		strings.ReplaceAll(licenseFixture, `MaxIPCameras="4"`, `MaxIPCameras="SECRET"`),
		strings.ReplaceAll(licenseFixture, `Trial="1"`, `Trial="SECRET"`),
	} {
		_, err = parseLicense([]byte(body), true)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "SECRET")
	}
}

func TestLicenseKeyPresence(t *testing.T) {
	key := make([]byte, 22)
	key[0] = 1 // MAC does not establish a license.
	present, err := licenseKeyPresent(key)
	require.NoError(t, err)
	require.False(t, present)
	key[21] = 1
	present, err = licenseKeyPresent(key)
	require.NoError(t, err)
	require.True(t, present)
	_, err = licenseKeyPresent(key[:21])
	require.Error(t, err)
}

func TestReadLicenseProtocol(t *testing.T) {
	client, server := sessionPair(t)
	done := make(chan error, 1)
	go func() {
		p, err := server.readPacket()
		if err != nil {
			done <- err
			return
		}
		if p.Type != packetLicense || p.Subtype != 1 || len(p.Payload) != 0 {
			done <- errors.New("wrong license request")
			return
		}
		key := make([]byte, 22)
		key[21] = 1
		if err = server.sendPacket(packetLicense, 2, key); err != nil {
			done <- err
			return
		}
		if p, err = server.readPacket(); err != nil {
			done <- err
			return
		}
		if p.Type != packetStatus || p.Subtype != 1 || len(p.Payload) != 0 {
			done <- errors.New("wrong status request")
			return
		}
		// Actual recorder updates have the same 3/2 type/subtype as the
		// full requested reply, without a Delta flag or a license section.
		for _, partial := range []string{
			`<eDVR><Servers><Server/></Servers></eDVR>`,
			`<eDVR><Users/></eDVR>`,
		} {
			if err = server.sendPacket(packetStatus, 2, []byte(partial)); err != nil {
				done <- err
				return
			}
		}
		done <- server.sendPacket(packetStatus, 2, []byte(licenseFixture))
	}()
	license, err := client.ReadLicense(context.Background())
	require.NoError(t, err)
	require.Equal(t, "trial", license.Status)
	require.NoError(t, <-done)
}

func TestReadLicenseRejectsMalformedLicenseBearingStatus(t *testing.T) {
	for _, body := range []string{
		`<eDVR><Systems><License Time="1700000000"/></Systems></eDVR>`,
		`<eDVR><Systems><License/><License/></Systems></eDVR>`,
		`<eDVR><Systems>`,
	} {
		client, server := sessionPair(t)
		done := make(chan error, 1)
		go func() {
			if _, err := server.readPacket(); err != nil {
				done <- err
				return
			}
			if err := server.sendPacket(packetLicense, 2, make([]byte, 22)); err != nil {
				done <- err
				return
			}
			if _, err := server.readPacket(); err != nil {
				done <- err
				return
			}
			done <- server.sendPacket(packetStatus, 2, []byte(body))
		}()
		_, err := client.ReadLicense(context.Background())
		require.Error(t, err)
		require.Error(t, client.conn.SetReadDeadline(time.Now().Add(time.Second)), "failed status must close its session")
		require.NoError(t, <-done)
	}
}

func TestReadLicensePartialStatusDeadline(t *testing.T) {
	client, server := sessionPair(t)
	client.options.Timeout = 80 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := server.readPacket(); err != nil {
			return
		}
		if server.sendPacket(packetLicense, 2, make([]byte, 22)) != nil {
			return
		}
		if _, err := server.readPacket(); err != nil {
			return
		}
		for {
			if server.sendPacket(packetStatus, 2, []byte(`<eDVR><Memory Throttling="0"/></eDVR>`)) != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	start := time.Now()
	_, err := client.ReadLicense(context.Background())
	require.Error(t, err)
	require.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond, "partial status must not be mistaken for the requested license reply")
	require.Less(t, time.Since(start), time.Second)
	<-done
}

func TestReadLicenseDeadline(t *testing.T) {
	client, server := sessionPair(t)
	client.options.Timeout = 30 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := server.readPacket(); err != nil {
			return
		}
		for {
			time.Sleep(5 * time.Millisecond)
			if err := server.sendPacket(packetDiscover, 2, []byte(`<eDVR/>`)); err != nil {
				return
			}
		}
	}()
	start := time.Now()
	_, err := client.ReadLicense(context.Background())
	// The socket deadline and context timer can race; either timeout must
	// still end the operation despite continuing unrelated traffic.
	require.Error(t, err)
	require.Less(t, time.Since(start), time.Second)
	<-done
}

func TestLabLicense(t *testing.T) {
	if os.Getenv("EXACQ_TEST_LICENSE") != "1" {
		t.Skip("set EXACQ_TEST_LICENSE=1 for read-only lab validation")
	}
	u := producerLabURL(t)
	source, err := ParseURL(u.String())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := Connect(ctx, source.options())
	require.NoError(t, err)
	defer c.Close()
	license, err := c.ReadLicense(ctx)
	require.NoError(t, err)
	t.Logf("license status=%s enterprise=%t cameraSlots=%d expires=%v", license.Status, license.Enterprise, license.IPCameraSlots, license.ExpiresAt)
}
