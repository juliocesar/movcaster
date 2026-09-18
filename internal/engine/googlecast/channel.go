package googlecast

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
)

// receiverStatus is the interesting slice of a RECEIVER_STATUS payload.
type receiverStatus struct {
	Type   string `json:"type"`
	Status struct {
		Applications []struct {
			AppID       string `json:"appId"`
			SessionID   string `json:"sessionId"`
			TransportID string `json:"transportId"`
			DisplayName string `json:"displayName"`
		} `json:"applications"`
		Volume struct {
			Level float64 `json:"level"`
			Muted bool    `json:"muted"`
		} `json:"volume"`
	} `json:"status"`
}

// connectPlatform opens the virtual connection to the platform receiver. It
// must precede any receiver-namespace request.
func (c *conn) connectPlatform() error {
	return c.send(receiverID, nsConnection, map[string]any{"type": "CONNECT"})
}

// getReceiverStatus asks the platform receiver for its status.
func (c *conn) getReceiverStatus(ctx context.Context) (*receiverStatus, error) {
	raw, err := c.request(ctx, receiverID, nsReceiver, map[string]any{"type": "GET_STATUS"})
	if err != nil {
		return nil, err
	}
	var st receiverStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ensureApp returns the session/transport ids of the media app, launching it
// only when it is not already running. Reusing a running app keeps a second
// movcaster run from blanking the screen with a relaunch.
func (c *conn) ensureApp(ctx context.Context, appID string) (sessionID, transportID string, err error) {
	st, err := c.getReceiverStatus(ctx)
	if err != nil {
		return "", "", fmt.Errorf("receiver GET_STATUS: %w", err)
	}
	for _, app := range st.Status.Applications {
		if app.AppID == appID && app.TransportID != "" {
			return app.SessionID, app.TransportID, nil
		}
	}

	raw, err := c.request(ctx, receiverID, nsReceiver, map[string]any{"type": "LAUNCH", "appId": appID})
	if err != nil {
		return "", "", fmt.Errorf("LAUNCH %s: %w", appID, err)
	}
	var launched receiverStatus
	if err := json.Unmarshal(raw, &launched); err != nil {
		return "", "", err
	}
	if launched.Type == "LAUNCH_ERROR" {
		var le struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &le)
		return "", "", fmt.Errorf("receiver refused to launch %s: %s", appID, le.Reason)
	}
	for _, app := range launched.Status.Applications {
		if app.AppID == appID && app.TransportID != "" {
			return app.SessionID, app.TransportID, nil
		}
	}
	return "", "", fmt.Errorf("LAUNCH %s: no transportId in RECEIVER_STATUS", appID)
}

// connectApp opens the virtual connection to the app's transportId. Skipping
// this makes every subsequent media message silently vanish — it is the most
// common Cast sender mistake.
func (c *conn) connectApp(transportID string) error {
	return c.send(transportID, nsConnection, map[string]any{"type": "CONNECT"})
}

// setVolume sets the device volume (0.0-1.0) on the platform receiver.
func (c *conn) setVolume(ctx context.Context, level float64) error {
	_, err := c.request(ctx, receiverID, nsReceiver, map[string]any{
		"type":   "SET_VOLUME",
		"volume": map[string]any{"level": level},
	})
	return err
}

// setMuted sets the device mute state on the platform receiver.
func (c *conn) setMuted(ctx context.Context, on bool) error {
	_, err := c.request(ctx, receiverID, nsReceiver, map[string]any{
		"type":   "SET_VOLUME",
		"volume": map[string]any{"muted": on},
	})
	return err
}

// volumeToPercent converts the receiver's 0.0-1.0 level to 0-100, rounding to
// nearest so a SetVolume round-trips.
func volumeToPercent(level float64) int {
	return int(math.Round(level * 100))
}

// percentToVolume converts 0-100 to the receiver's 0.0-1.0, clamping.
func percentToVolume(v int) float64 {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return float64(v) / 100
}
