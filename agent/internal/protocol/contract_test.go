package protocol

// Contract tests: the Agent against the shared protocol examples.
// The Integration runs the same examples in tests/test_protocol_contract.py.
// See protocol/README.md for the example format.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const protocolDir = "../../../protocol"

type example struct {
	Description string          `json:"description"`
	Message     json.RawMessage `json:"message"`
	Produced    json.RawMessage `json:"produced"`
	Reply       json.RawMessage `json:"reply"`
}

func loadExamples(t *testing.T, folder string) map[string]example {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(protocolDir, "examples", folder, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no examples in %s: %v", folder, err)
	}
	examples := make(map[string]example)
	for _, path := range paths {
		var e example
		readJSON(t, path, &e)
		examples[strings.TrimSuffix(filepath.Base(path), ".json")] = e
	}
	return examples
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func loadSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	schema, err := jsonschema.NewCompiler().Compile(filepath.Join(protocolDir, "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

// generic turns JSON into maps, slices, and float64s, so two messages compare by value.
func generic(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	return v
}

func TestExamplesMatchSchema(t *testing.T) {
	schema := loadSchema(t)
	for _, folder := range []string{"valid", "unknown"} {
		for name, e := range loadExamples(t, folder) {
			t.Run(folder+"/"+name, func(t *testing.T) {
				for _, message := range []json.RawMessage{e.Message, e.Produced, e.Reply} {
					if len(message) == 0 || string(message) == "null" {
						continue
					}
					if err := schema.Validate(generic(t, message)); err != nil {
						t.Error(err)
					}
				}
			})
		}
	}
}

func TestInvalidExamplesAreRejectedBySchema(t *testing.T) {
	schema := loadSchema(t)
	for name, e := range loadExamples(t, "invalid") {
		t.Run(name, func(t *testing.T) {
			if schema.Validate(generic(t, e.Message)) == nil {
				t.Errorf("schema accepted it, want rejected: %s", e.Description)
			}
		})
	}
}

func TestAgentRefusesInvalidExamples(t *testing.T) {
	for name, e := range loadExamples(t, "invalid") {
		t.Run(name, func(t *testing.T) {
			if message, err := Decode(e.Message); err == nil {
				t.Errorf("Decode returned %#v, want an error: %s", message, e.Description)
			}
		})
	}
}

func TestAgentDoesNotWriteWhatItWouldRefuse(t *testing.T) {
	for name, message := range map[string]Message{
		"no groups": &Delta{ID: "a"},
		"nil list":  &Delta{ID: "a", Groups: Groups{Disks: &Disks{}}},
	} {
		t.Run(name, func(t *testing.T) {
			if frame, err := Encode(message); err == nil {
				t.Errorf("Encode wrote %s, want an error", frame)
			}
		})
	}
}

// Reading a message and writing it back gives the same message, minus unknown fields.
func TestAgentReadsAndWritesExamples(t *testing.T) {
	for name, e := range loadExamples(t, "valid") {
		t.Run(name, func(t *testing.T) {
			message, err := Decode(e.Message)
			if err != nil {
				t.Fatal(err)
			}
			frame, err := Encode(message)
			if err != nil {
				t.Fatal(err)
			}
			want := e.Produced
			if len(want) == 0 {
				want = e.Message
			}
			if got, want := generic(t, frame), generic(t, want); !reflect.DeepEqual(got, want) {
				t.Errorf("wrote\n%s\nwant\n%s", frame, want)
			}
		})
	}
}

func TestAgentReadsHelloFields(t *testing.T) {
	var e example
	readJSON(t, filepath.Join(protocolDir, "examples/valid/hello_request.json"), &e)

	message, err := Decode(e.Message)
	if err != nil {
		t.Fatal(err)
	}

	hello, ok := message.(*HelloRequest)
	if !ok {
		t.Fatalf("Decode returned %T, want *HelloRequest", message)
	}
	if hello.InstanceID != "3f2b6c1e-8d4a-4b7e-9c21-5a6d7e8f9012" {
		t.Errorf("InstanceID = %q", hello.InstanceID)
	}
	if !reflect.DeepEqual(hello.ProtocolMajors, []int{1}) {
		t.Errorf("ProtocolMajors = %v", hello.ProtocolMajors)
	}
	if hello.Distro.ID == nil || *hello.Distro.ID != "debian" {
		t.Errorf("Distro.ID = %v", hello.Distro.ID)
	}
	if want := []Action{ActionReboot, ActionUpdateRun, ActionAgentUpdate}; !reflect.DeepEqual(hello.EnabledActions, want) {
		t.Errorf("EnabledActions = %v, want %v", hello.EnabledActions, want)
	}
}

// An unknown type is not an error, so the connection stays open.
func TestUnknownTypeIsIgnoredAndARequestGetsUnsupported(t *testing.T) {
	for name, e := range loadExamples(t, "unknown") {
		t.Run(name, func(t *testing.T) {
			message, err := Decode(e.Message)
			if err != nil {
				t.Fatal(err)
			}
			unknown, ok := message.(*Unknown)
			if !ok {
				t.Fatalf("Decode returned %T, want *Unknown", message)
			}

			var want struct{ ID string }
			expectReply := len(e.Reply) > 0 && string(e.Reply) != "null"
			if expectReply {
				if err := json.Unmarshal(e.Reply, &want); err != nil {
					t.Fatal(err)
				}
			}
			reply, ok := UnsupportedReply(unknown, want.ID)
			if ok != expectReply {
				t.Fatalf("UnsupportedReply ok = %v, want %v", ok, expectReply)
			}
			if !ok {
				return
			}
			frame, err := Encode(reply)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(generic(t, frame), generic(t, e.Reply)) {
				t.Errorf("wrote\n%s\nwant\n%s", frame, e.Reply)
			}
		})
	}
}

func TestMalformedFramesAreErrors(t *testing.T) {
	var cases []struct{ Name, Description, Frame string }
	readJSON(t, filepath.Join(protocolDir, "examples/malformed.json"), &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if message, err := Decode([]byte(c.Frame)); err == nil {
				t.Errorf("Decode returned %#v, want an error: %s", message, c.Description)
			}
		})
	}
}

func TestVersionMatchesSchema(t *testing.T) {
	var schema struct {
		Version string `json:"x-protocol-version"`
	}
	readJSON(t, filepath.Join(protocolDir, "schema.json"), &schema)
	if Version != schema.Version || Version != "1.1" {
		t.Errorf("Version = %q, schema says %q, want 1.1", Version, schema.Version)
	}
	if !reflect.DeepEqual(Majors, []int{1}) {
		t.Errorf("Majors = %v, want [1]", Majors)
	}
}
