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
	"slices"
	"strings"
)

// Version is the protocol version this Agent speaks, as major.minor.
const Version = "1.0"

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

// Decode reads one frame.
func Decode(frame []byte) (Message, error) {
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
	for key := range newKnown {
		knownType = knownType || key[0] == *env.Type
	}
	if !knownType {
		return &Unknown{Type: *env.Type, ID: *env.ID, Kind: *env.Kind}, nil
	}
	newMessage, ok := newKnown[[2]string{*env.Type, string(*env.Kind)}]
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
	return message, nil
}

// Encode writes one frame.
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
	return json.Marshal(fields)
}

// UnsupportedReply returns the reply to an unknown message: unsupported for a
// request, and false for an event or a reply, which get no reply.
func UnsupportedReply(message *Unknown, replyID string) (*Unsupported, bool) {
	if message.Kind != KindRequest {
		return nil, false
	}
	return &Unsupported{ID: replyID, ReplyTo: message.ID, RequestType: message.Type}, true
}

// check reports what encoding/json lets through: a missing field (every
// field without omitempty is required), null where the type is not a pointer,
// and a value outside a field's enum tag. It looks at every depth.
func check(t reflect.Type, raw json.RawMessage, path, enum string) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if t.Kind() == reflect.Pointer {
			return nil
		}
		return malformed("%s must not be null", path)
	}
	switch t.Kind() {
	case reflect.Pointer:
		return check(t.Elem(), raw, path, enum)
	case reflect.Slice:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return malformed("%s must be a list", path)
		}
		for i, item := range items {
			if err := check(t.Elem(), item, fmt.Sprintf("%s[%d]", path, i), enum); err != nil {
				return err
			}
		}
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return malformed("%s must be an object", path)
		}
		for i := range t.NumField() {
			field := t.Field(i)
			name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			value, present := fields[name]
			if !present {
				if options == "omitempty" {
					continue
				}
				return malformed("%s.%s is missing", path, name)
			}
			if err := check(field.Type, value, path+"."+name, field.Tag.Get("enum")); err != nil {
				return err
			}
		}
	case reflect.String:
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return malformed("%s must be text", path)
		}
		if enum != "" && !slices.Contains(strings.Split(enum, ","), value) {
			return malformed("%s has an unknown value %q", path, value)
		}
	}
	return nil
}
