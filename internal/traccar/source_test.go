package traccar

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPayloadMatchesTraccarFormat(t *testing.T) {
	fix := &Time{time.Date(2026, 10, 2, 22, 12, 55, 0, time.UTC)}
	proto := "osmand"
	p := Position{ID: 7, DeviceID: 15, Protocol: &proto, ServerTime: fix, DeviceTime: fix, FixTime: fix, Valid: true,
		Latitude: 33.6, Longitude: -7.6, Altitude: Number{30, true}, Speed: Number{float64(float32(12.3)), true},
		Course: Number{45, true}, Accuracy: Number{5, false},
		Attributes: json.RawMessage(`{"batteryLevel":88.0,"motion":true}`), Network: json.RawMessage("null"), GeofenceIDs: json.RawMessage("null")}
	d := Device{ID: 15, Name: "860000000000019", UniqueID: "860000000000019", Attributes: json.RawMessage("{}")}
	raw, err := Payload(d, p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{
		`"fixTime":"2026-10-02T22:12:55.000+00:00"`, // Traccar's date format
		`"speed":12.3`, // FLOAT column without float32 noise
		`"attributes":{"batteryLevel":88.0,"motion":true}`, // attributes as JSON, not a string
		`"network":null`, `"device":{"id":15,`, `"uniqueId":"860000000000019"`, `"name":"860000000000019"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s\n%s", want, s)
		}
	}
}

// loc=UTC is the driver default (omitted from the DSN); the session time_zone must be explicit.
func TestDSNPinsUTC(t *testing.T) {
	dsn, err := Config{Mode: "socket", Socket: "/run/mysqld/mysqld.sock", User: "u", Password: "p", Database: "traccar"}.DSN()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dsn, "unix(/run/mysqld/mysqld.sock)") || !strings.Contains(dsn, "time_zone=%27%2B00%3A00%27") {
		t.Fatalf("dsn: %s", dsn)
	}
}
