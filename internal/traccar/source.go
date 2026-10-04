// Package traccar reads devices and position history from a Traccar database
// and renders positions in the exact JSON shape Traccar's JSON forwarder posts.
package traccar

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Config describes how to reach the Traccar database. Mode is "tcp" or "socket".
type Config struct {
	Mode     string `json:"mode"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Socket   string `json:"socket"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	Database string `json:"database"`
}

func (c Config) DSN() (string, error) {
	mc := mysql.NewConfig()
	mc.User = c.User
	mc.Passwd = c.Password
	mc.DBName = c.Database
	switch c.Mode {
	case "socket":
		if c.Socket == "" {
			return "", errors.New("socket path is required")
		}
		mc.Net, mc.Addr = "unix", c.Socket
	case "tcp", "":
		if c.Host == "" {
			return "", errors.New("host is required")
		}
		port := c.Port
		if port == 0 {
			port = 3306
		}
		mc.Net, mc.Addr = "tcp", net.JoinHostPort(c.Host, strconv.Itoa(port))
	default:
		return "", fmt.Errorf("unknown connection mode %q", c.Mode)
	}
	if c.Database == "" {
		return "", errors.New("database is required")
	}
	// TIMESTAMP columns are returned in the session time zone: pin it to UTC so the
	// relay never depends on the MySQL server's own time zone.
	mc.ParseTime = true
	mc.Loc = time.UTC
	mc.Params = map[string]string{"time_zone": "'+00:00'"}
	mc.Timeout = 10 * time.Second
	mc.ReadTimeout = 5 * time.Minute
	mc.AllowNativePasswords = true
	return mc.FormatDSN(), nil
}

type Source struct {
	db *sql.DB
	// column name -> data type, for optional columns that differ between Traccar versions
	positionColumns map[string]string
	positionSelect  string
}

func Open(ctx context.Context, cfg Config) (*Source, error) {
	dsn, err := cfg.DSN()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	// Small pool: this is a production database serving Traccar itself.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(10 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s := &Source{db: db}
	if err := s.inspect(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Source) Close() error { return s.db.Close() }

func (s *Source) inspect(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = 'tc_positions'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	s.positionColumns = map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return err
		}
		s.positionColumns[strings.ToLower(name)] = strings.ToLower(typ)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, required := range []string{"id", "deviceid", "fixtime", "latitude", "longitude"} {
		if _, ok := s.positionColumns[required]; !ok {
			return errors.New("this database has no Traccar tc_positions table (missing column " + required + ")")
		}
	}
	cols := []string{"id", "protocol", "deviceid", "servertime", "devicetime", "fixtime", "valid", "latitude",
		"longitude", "altitude", "speed", "course", "address", "attributes", "accuracy", "network", "geofenceids"}
	sel := make([]string, len(cols))
	for i, c := range cols {
		if _, ok := s.positionColumns[c]; ok {
			sel[i] = c
		} else {
			sel[i] = "NULL"
		}
	}
	s.positionSelect = strings.Join(sel, ", ")
	return nil
}

type Info struct {
	Version         string     `json:"version"`
	Devices         int64      `json:"devices"`
	PositionsApprox int64      `json:"positionsApprox"`
	OldestFix       *time.Time `json:"oldestFix"`
	NewestFix       *time.Time `json:"newestFix"`
}

func (s *Source) Info(ctx context.Context) (Info, error) {
	var info Info
	if err := s.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&info.Version); err != nil {
		return info, err
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tc_devices").Scan(&info.Devices); err != nil {
		return info, err
	}
	// Exact COUNT(*) on a large position table is slow; the optimizer estimate is enough here.
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(table_rows, 0) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = 'tc_positions'`).Scan(&info.PositionsApprox)
	// A global MIN/MAX(fixtime) scans the whole table; per device it is one index lookup each.
	var oldest, newest sql.NullTime
	_ = s.db.QueryRowContext(ctx, `SELECT MIN(first), MAX(last) FROM (SELECT
		(SELECT MIN(fixtime) FROM tc_positions p WHERE p.deviceid = d.id) AS first,
		(SELECT MAX(fixtime) FROM tc_positions p WHERE p.deviceid = d.id) AS last
		FROM tc_devices d) x`).Scan(&oldest, &newest)
	if oldest.Valid {
		info.OldestFix = &oldest.Time
	}
	if newest.Valid {
		info.NewestFix = &newest.Time
	}
	return info, nil
}

// Device is a tc_devices row, serialized like Traccar's Device model.
type Device struct {
	ID             int64           `json:"id"`
	Attributes     json.RawMessage `json:"attributes"`
	GroupID        int64           `json:"groupId"`
	CalendarID     int64           `json:"calendarId"`
	Name           string          `json:"name"`
	UniqueID       string          `json:"uniqueId"`
	Status         *string         `json:"status"`
	LastUpdate     *Time           `json:"lastUpdate"`
	PositionID     int64           `json:"positionId"`
	Phone          *string         `json:"phone"`
	Model          *string         `json:"model"`
	Contact        *string         `json:"contact"`
	Category       *string         `json:"category"`
	Disabled       bool            `json:"disabled"`
	ExpirationTime *Time           `json:"expirationTime"`
}

// Devices reads tc_devices with SELECT * so it works across Traccar schema versions.
func (s *Source) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT * FROM tc_devices ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var devices []Device
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		d := Device{Attributes: json.RawMessage("{}")}
		for i, c := range cols {
			v := values[i]
			switch strings.ToLower(c) {
			case "id":
				d.ID = asInt(v)
			case "attributes":
				d.Attributes = asJSON(v, "{}")
			case "groupid":
				d.GroupID = asInt(v)
			case "calendarid":
				d.CalendarID = asInt(v)
			case "name":
				d.Name = asString(v)
			case "uniqueid":
				d.UniqueID = asString(v)
			case "status":
				d.Status = asStringPtr(v)
			case "lastupdate":
				d.LastUpdate = asTime(v)
			case "positionid":
				d.PositionID = asInt(v)
			case "phone":
				d.Phone = asStringPtr(v)
			case "model":
				d.Model = asStringPtr(v)
			case "contact":
				d.Contact = asStringPtr(v)
			case "category":
				d.Category = asStringPtr(v)
			case "disabled":
				d.Disabled = asInt(v) != 0
			case "expirationtime":
				d.ExpirationTime = asTime(v)
			}
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *Source) Count(ctx context.Context, deviceID int64, from, to time.Time) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tc_positions WHERE deviceid = ? AND fixtime BETWEEN ? AND ?",
		deviceID, from.UTC(), to.UTC()).Scan(&n)
	return n, err
}

// Cursor is the last position delivered for a device: (fixtime, id) keyset.
type Cursor struct {
	FixTime time.Time
	ID      int64
}

func (c Cursor) IsZero() bool { return c.ID == 0 }

// Position is a tc_positions row, serialized like Traccar's Position model.
type Position struct {
	ID          int64           `json:"id"`
	Attributes  json.RawMessage `json:"attributes"`
	DeviceID    int64           `json:"deviceId"`
	Protocol    *string         `json:"protocol"`
	ServerTime  *Time           `json:"serverTime"`
	DeviceTime  *Time           `json:"deviceTime"`
	FixTime     *Time           `json:"fixTime"`
	Valid       bool            `json:"valid"`
	Latitude    float64         `json:"latitude"`
	Longitude   float64         `json:"longitude"`
	Altitude    Number          `json:"altitude"`
	Speed       Number          `json:"speed"`
	Course      Number          `json:"course"`
	Address     *string         `json:"address"`
	Accuracy    Number          `json:"accuracy"`
	Network     json.RawMessage `json:"network"`
	GeofenceIDs json.RawMessage `json:"geofenceIds"`
}

// Page returns up to limit positions of a device in [from, to], strictly after the cursor,
// ordered by (fixtime, id). It uses the (deviceid, fixtime) index; no OFFSET.
func (s *Source) Page(ctx context.Context, deviceID int64, from, to time.Time, after Cursor, limit int) ([]Position, error) {
	q := "SELECT " + s.positionSelect + " FROM tc_positions WHERE deviceid = ? AND fixtime BETWEEN ? AND ?"
	args := []any{deviceID, from.UTC(), to.UTC()}
	if !after.IsZero() {
		q += " AND (fixtime > ? OR (fixtime = ? AND id > ?))"
		args = append(args, after.FixTime.UTC(), after.FixTime.UTC(), after.ID)
	}
	q += " ORDER BY fixtime, id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	is32 := func(col string) bool { return s.positionColumns[col] == "float" }
	var out []Position
	for rows.Next() {
		var (
			p                                 Position
			protocol, address                 sql.NullString
			attributes, network, geofences    sql.NullString
			serverTime, deviceTime, fixTime   sql.NullTime
			valid                             sql.NullInt64
			altitude, speed, course, accuracy sql.NullFloat64
		)
		if err := rows.Scan(&p.ID, &protocol, &p.DeviceID, &serverTime, &deviceTime, &fixTime, &valid,
			&p.Latitude, &p.Longitude, &altitude, &speed, &course, &address, &attributes, &accuracy,
			&network, &geofences); err != nil {
			return nil, err
		}
		p.Protocol = nullString(protocol)
		p.Address = nullString(address)
		p.ServerTime, p.DeviceTime, p.FixTime = nullTime(serverTime), nullTime(deviceTime), nullTime(fixTime)
		p.Valid = valid.Int64 != 0
		p.Altitude = Number{altitude.Float64, is32("altitude")}
		p.Speed = Number{speed.Float64, is32("speed")}
		p.Course = Number{course.Float64, is32("course")}
		p.Accuracy = Number{accuracy.Float64, is32("accuracy")}
		p.Attributes = asJSON(attributes.String, "{}")
		p.Network = asJSON(network.String, "null")
		p.GeofenceIDs = asJSON(geofences.String, "null")
		out = append(out, p)
	}
	return out, rows.Err()
}

// Payload renders the body Traccar's JSON forwarder would post for this position.
func Payload(d Device, p Position) ([]byte, error) {
	return json.Marshal(struct {
		Position Position `json:"position"`
		Device   Device   `json:"device"`
	}{p, d})
}

// ImportPayload is one batch for the PowerFleet Server history import:
// {"device": …, "positions": [ … ]} with the same objects Payload uses.
func ImportPayload(d Device, ps []Position) ([]byte, error) {
	return json.Marshal(struct {
		Device    Device     `json:"device"`
		Positions []Position `json:"positions"`
	}{d, ps})
}

// Time marshals like Traccar: 2026-10-02T22:12:55.000+00:00
type Time struct{ time.Time }

func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.UTC().Format("2006-01-02T15:04:05.000+00:00") + `"`), nil
}

// Number keeps the shortest representation of FLOAT (32-bit) columns, e.g. 12.3 instead of 12.300000190734863.
type Number struct {
	V    float64
	Is32 bool
}

func (n Number) MarshalJSON() ([]byte, error) {
	bits := 64
	if n.Is32 {
		bits = 32
	}
	return []byte(strconv.FormatFloat(n.V, 'f', -1, bits)), nil
}

func asJSON(v any, fallback string) json.RawMessage {
	s := strings.TrimSpace(asString(v))
	if s == "" || !json.Valid([]byte(s)) {
		return json.RawMessage(fallback)
	}
	return json.RawMessage(s)
}

func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func asStringPtr(v any) *string {
	if v == nil {
		return nil
	}
	s := asString(v)
	return &s
}

func asInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int32:
		return int64(x)
	case uint64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
		return 0
	case []byte:
		n, _ := strconv.ParseInt(string(x), 10, 64)
		return n
	}
	return 0
}

func asTime(v any) *Time {
	if t, ok := v.(time.Time); ok {
		return &Time{t}
	}
	return nil
}

func nullString(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func nullTime(t sql.NullTime) *Time {
	if !t.Valid {
		return nil
	}
	return &Time{t.Time}
}
