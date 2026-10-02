// Package protocol reads and writes the messages of the Agent-Integration
// protocol. The shape of every message is in protocol/schema.json at the
// repo root.
//
// Readers ignore unknown fields. A message with an unknown type decodes to
// *Unknown, not an error, so the connection stays open.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Version is the protocol version this Agent speaks, as major.minor.
const Version = "1.2"

// Majors lists every protocol major this Agent supports.
var Majors = []int{1}

// Kind says whether a message needs a reply.
type Kind string

const (
	KindRequest Kind = "request"
	KindReply   Kind = "reply"
	KindEvent   Kind = "event"
)

// Message is one protocol message. The concrete types are in messages.go.
type Message interface {
	header() (messageType string, kind Kind)
}

// ErrMalformed is wrapped by every Decode error.
var ErrMalformed = errors.New("malformed message")

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

type envelope struct {
	Type    *string `json:"type"`
	ID      *string `json:"id"`
	Kind    *Kind   `json:"kind"`
	ReplyTo *string `json:"reply_to"`
}

// rules is a message with rules that struct tags cannot say.
type rules interface {
	checkRules() error
}

var formats = map[string]*regexp.Regexp{
	// The same rules as the uuid, time, and refresh_schedule patterns in
	// schema.json.
	"uuid":             regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`),
	"time":             regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$`),
	"refresh_schedule": regexp.MustCompile(`^(off|every_24h|([01][0-9]|2[0-3]):[0-5][0-9])$`),
}

// Decode reads one frame.
func Decode(frame []byte) (Message, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(frame, &top); err != nil || top == nil {
		return nil, malformed("a message must be a JSON object")
	}
	if err := checkKeyCase(top, []string{"type", "id", "kind", "reply_to"}, "message"); err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		return nil, malformed("%v", err)
	}
	if env.Type == nil || *env.Type == "" || env.ID == nil || *env.ID == "" {
		return nil, malformed("type and id are required")
	}
	if env.Kind == nil || (*env.Kind != KindRequest && *env.Kind != KindReply && *env.Kind != KindEvent) {
		return nil, malformed("kind must be request, reply, or event")
	}
	if *env.Kind == KindReply && env.ReplyTo == nil {
		return nil, malformed("a reply needs reply_to")
	}

	knownType := false
	for key := range knownMessages {
		knownType = knownType || key[0] == *env.Type
	}
	if !knownType {
		return &Unknown{Type: *env.Type, ID: *env.ID, Kind: *env.Kind}, nil
	}
	newMessage, ok := knownMessages[[2]string{*env.Type, string(*env.Kind)}]
	if !ok {
		return nil, malformed("%s cannot be a %s", *env.Type, *env.Kind)
	}
	message := newMessage()
	if err := json.Unmarshal(frame, message); err != nil {
		return nil, malformed("%s: %v", *env.Type, err)
	}
	if err := check(reflect.TypeOf(message).Elem(), frame, *env.Type, ""); err != nil {
		return nil, err
	}
	if withRules, ok := message.(rules); ok {
		if err := withRules.checkRules(); err != nil {
			return nil, malformed("%s: %v", *env.Type, err)
		}
	}
	return message, nil
}

// Encode writes one frame. It refuses a message that Decode would refuse.
func Encode(message Message) ([]byte, error) {
	if _, ok := message.(*Unknown); ok {
		return nil, errors.New("an Unknown message cannot be sent")
	}
	body, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	messageType, kind := message.header()
	fields["type"], _ = json.Marshal(messageType)
	fields["kind"], _ = json.Marshal(kind)
	frame, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if _, err := Decode(frame); err != nil {
		return nil, err
	}
	return frame, nil
}

// UnsupportedReply returns the reply to an unknown message: unsupported for a
// request, and false for an event or a reply, which get no reply.
func UnsupportedReply(message *Unknown, replyID string) (*Unsupported, bool) {
	if message.Kind != KindRequest {
		return nil, false
	}
	return &Unsupported{ID: replyID, ReplyTo: message.ID, RequestType: message.Type}, true
}

// checkKeyCase refuses a key that differs from a known name only by case.
// encoding/json would read it as that name.
func checkKeyCase(fields map[string]json.RawMessage, names []string, path string) error {
	for key := range fields {
		for _, name := range names {
			if key != name && strings.EqualFold(key, name) {
				return malformed("%s has a key with the wrong case: %s", path, key)
			}
		}
	}
	return nil
}

// check reports what encoding/json lets through, at every depth: a missing
// field, a key with the wrong case, null where it is not allowed, and a
// value that breaks the field's enum, format, max, or min tag.
func check(t reflect.Type, raw json.RawMessage, path string, tag reflect.StructTag) error {
	_, options, _ := strings.Cut(tag.Get("json"), ",")
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if t.Kind() == reflect.Pointer && options != "omitempty" {
			return nil
		}
		return malformed("%s must not be null", path)
	}
	switch t.Kind() {
	case reflect.Pointer:
		return check(t.Elem(), raw, path, tag)
	case reflect.Slice:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return malformed("%s must be a list", path)
		}
		if limit, err := strconv.Atoi(tag.Get("max")); err == nil && len(items) > limit {
			return malformed("%s has more than %d items", path, limit)
		}
		for i, item := range items {
			if err := check(t.Elem(), item, fmt.Sprintf("%s[%d]", path, i), tag); err != nil {
				return err
			}
		}
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return malformed("%s must be an object", path)
		}
		if limit, err := strconv.Atoi(tag.Get("min")); err == nil && len(fields) < limit {
			return malformed("%s needs at least %d keys", path, limit)
		}
		names := make([]string, t.NumField())
		for i := range t.NumField() {
			names[i], _, _ = strings.Cut(t.Field(i).Tag.Get("json"), ",")
		}
		if err := checkKeyCase(fields, names, path); err != nil {
			return err
		}
		for i, name := range names {
			field := t.Field(i)
			value, present := fields[name]
			if !present {
				if strings.HasSuffix(field.Tag.Get("json"), ",omitempty") {
					continue
				}
				return malformed("%s.%s is missing", path, name)
			}
			if err := check(field.Type, value, path+"."+name, field.Tag); err != nil {
				return err
			}
		}
	case reflect.String:
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return malformed("%s must be text", path)
		}
		if enum := tag.Get("enum"); enum != "" && !slices.Contains(strings.Split(enum, ","), value) {
			return malformed("%s has an unknown value %q", path, value)
		}
		if format := tag.Get("format"); format != "" && !formats[format].MatchString(value) {
			return malformed("%s must be a %s", path, format)
		}
	}
	return nil
}
