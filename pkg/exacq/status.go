package exacq

import (
	"context"
	"errors"
	"math"
	"time"
)

// Status reports only attributes actually supplied by the recorder. A missing
// connection state is unknown, not online or offline.
type Status struct {
	Connections      map[uint32]bool `json:"connections"`
	Cameras          []CameraStatus  `json:"cameras"`
	Storage          []StorageStatus `json:"storage,omitempty"`
	MemoryThrottling *bool           `json:"memoryThrottling,omitempty"`
}

type CameraStatus struct {
	ID         uint32    `json:"id"`
	Motion     *bool     `json:"motion,omitempty"`
	RecordIdle *bool     `json:"recordIdle,omitempty"`
	Updated    time.Time `json:"updated"`
}

type StorageStatus struct {
	ID            uint32        `json:"id"`
	RecordingMbps float64       `json:"recordingMbps"`
	Drives        []DriveStatus `json:"drives"`
}

type DriveStatus struct {
	Name   string `json:"name"`
	SizeGB uint32 `json:"sizeGB"`
	FreeGB uint32 `json:"freeGB"`
	Alarm  uint32 `json:"alarm"`
}

func ParseStatus(data []byte, inventory *Inventory) (*Status, error) {
	root, err := parseConfigXML(data)
	if err != nil {
		return nil, err
	}
	return parseStatusRoot(root, inventory)
}

func parseStatusRoot(root *configNode, inventory *Inventory) (*Status, error) {
	p := configParser{}
	status := &Status{Connections: make(map[uint32]bool)}
	for _, memory := range root.children("Memory") {
		status.MemoryThrottling = p.optionalBool(memory, "Throttling")
	}
	for _, storage := range root.children("Storage") {
		s := StorageStatus{ID: p.number(storage, "ID", math.MaxUint32, true), RecordingMbps: p.decimal(storage, "RecordingMbps", false)}
		for _, drive := range storage.children("Drive") {
			s.Drives = append(s.Drives, DriveStatus{Name: drive.attr("Name"), SizeGB: p.number(drive, "SizeGB", math.MaxUint32, false), FreeGB: p.number(drive, "FreeGB", math.MaxUint32, false), Alarm: p.number(drive, "Alarm", math.MaxUint32, false)})
		}
		status.Storage = append(status.Storage, s)
	}
	indices := make(map[uint32]int)
	for _, devices := range root.children("Devices") {
		plugin := p.number(devices, "ID", 0x0fff0000, true)
		for _, device := range devices.children("Device") {
			deviceID := plugin | p.number(device, "Number", 255, true)<<8
			if device.has("Connected") {
				status.Connections[deviceID] = p.boolean(device, "Connected", false)
			}
			parseContext := func(node *configNode, context uint32) {
				for _, video := range node.children("Video") {
					for _, input := range video.children("Input") {
						inputContext := context
						if input.has("Context") {
							inputContext = p.number(input, "Context", 15, true)
						}
						id := deviceID | inputContext<<28 | p.number(input, "Number", 127, true)
						if !input.has("Motion") && !input.has("RecordIdle") && !input.has("Loss") {
							continue
						}
						index, found := indices[id]
						if !found {
							index = len(status.Cameras)
							indices[id] = index
							status.Cameras = append(status.Cameras, CameraStatus{ID: id})
						}
						camera := &status.Cameras[index]
						if input.has("Motion") {
							camera.Motion = p.optionalBool(input, "Motion")
						}
						if input.has("RecordIdle") {
							camera.RecordIdle = p.optionalBool(input, "RecordIdle")
						}
						if input.has("Loss") {
							status.Connections[id] = !p.boolean(input, "Loss", false)
						}
						if input.has("Time") {
							updated := time.Unix(int64(p.number(input, "Time", math.MaxUint32, true)), int64(p.number(input, "Milliseconds", 999, false))*int64(time.Millisecond))
							if updated.After(camera.Updated) {
								camera.Updated = updated
							}
						}
					}
				}
				for _, encoder := range node.child("Codec").children("Encoder") {
					encoderNumber := p.number(encoder, "Number", 7, true)
					for _, channel := range encoder.children("Channel") {
						if !channel.has("Connected") {
							continue
						}
						number := p.number(channel, "Number", 127, true)
						id := deviceID | context<<28 | encoderNumber<<4 + number
						if inventory != nil {
							for _, stream := range inventory.Streams {
								if stream.DeviceID == deviceID && stream.Context == context && stream.Encoder == encoderNumber && stream.Channel == number {
									id = stream.ID
									break
								}
							}
						}
						status.Connections[id] = p.boolean(channel, "Connected", false)
					}
				}
			}
			parseContext(device, 0)
			for _, context := range device.children("Context") {
				parseContext(context, p.number(context, "Number", 15, true))
			}
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	return status, nil
}

// RefreshStatus queries native 3/1 -> 3/2 status and updates connection/motion
// fields in a new Inventory snapshot. Use a dedicated control connection.
func (c *Client) RefreshStatus(ctx context.Context) (result *Status, err error) {
	defer func() { err = classify(err, PhaseStatus) }()
	timeout := c.options.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c.readMu.Lock()
	defer c.readMu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.sendPacket(packetStatus, 1, nil); err != nil {
		return nil, err
	}
	bytesRead := 0
	for i := 0; i < 128; i++ {
		packet, err := c.readNext(ctx)
		if err != nil {
			return nil, err
		}
		bytesRead += len(packet.Payload)
		if bytesRead > maxConfigBytes {
			return nil, errors.New("exacq: status response byte limit exceeded")
		}
		if packet.Type != packetStatus || packet.Subtype != 2 {
			continue
		}
		if packet.Status != 0 {
			return nil, withStatus(errorf(CodeProtocolError, PhaseStatus, "recorder rejected the status request (status %d)", int(int16(packet.Status))), packet.Status)
		}
		root, parseErr := parseConfigXML(packet.Payload)
		if parseErr != nil {
			return nil, parseErr
		}
		// Unsolicited Servers/Analytics/Users updates use the same 3/2
		// header without Delta. Require a complete licensed or empty-unlicensed
		// Systems/License shape before publishing a requested health snapshot.
		if len(root.child("Systems").children("License")) == 0 {
			continue
		}
		if _, parseErr = parseLicenseRoot(root, false); parseErr != nil {
			return nil, parseErr
		}
		inv := c.Inventory()
		status, err := parseStatusRoot(root, inv)
		if err != nil {
			return nil, err
		}
		if inv != nil {
			clone := *inv
			clone.Devices = append([]Device(nil), inv.Devices...)
			clone.Cameras = append([]Camera(nil), inv.Cameras...)
			clone.Streams = append([]Stream(nil), inv.Streams...)
			for i := range clone.Devices {
				if connected, found := status.Connections[clone.Devices[i].ID]; found {
					clone.Devices[i].Connected = &connected
				}
			}
			for i := range clone.Cameras {
				if connected, found := status.Connections[clone.Cameras[i].ID]; found {
					clone.Cameras[i].Connected = &connected
				}
				for _, camera := range status.Cameras {
					if camera.ID == clone.Cameras[i].ID && camera.Motion != nil {
						clone.Cameras[i].Motion = camera.Motion
					}
				}
			}
			for i := range clone.Streams {
				if connected, found := status.Connections[clone.Streams[i].ID]; found {
					clone.Streams[i].Connected = &connected
				}
			}
			c.inventoryMu.Lock()
			c.inventory = &clone
			c.inventoryMu.Unlock()
		}
		return status, nil
	}
	return nil, errors.New("exacq: status response limit exceeded")
}
