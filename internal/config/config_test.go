package config

import (
	"io"
	"testing"
	"time"
)

func environment(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

var home = environment(map[string]string{"HOME": "/home/owner"})

func TestDefaultsAndOverrides(t *testing.T) {
	c, err := Parse([]string{"-api-key-file", "/tmp/example-key", "-temperature", "1.3"}, home, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "gemini-3.8-live" || c.APIKeyFile != "/tmp/example-key" || c.Temperature != 1.3 || c.Listen != "127.0.0.1:8080" || c.SpeechStart != "low" || c.SpeechPrefix != 200*time.Millisecond || c.StatePath != "/home/owner/.local/state/talker/tasks.json" {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestStatePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"XDG state home", nil, map[string]string{"HOME": "/home/owner", "XDG_STATE_HOME": "/var/state"}, "/var/state/talker/tasks.json"},
		{"XDG default", nil, map[string]string{"HOME": "/home/owner"}, "/home/owner/.local/state/talker/tasks.json"},
		{"relative XDG ignored", nil, map[string]string{"HOME": "/home/owner", "XDG_STATE_HOME": "state"}, "/home/owner/.local/state/talker/tasks.json"},
		{"environment", nil, map[string]string{"HOME": "/home/owner", "TALKER_STATE": "/srv/talker.json"}, "/srv/talker.json"},
		{"flag", []string{"-state", "ledger.json"}, map[string]string{"TALKER_STATE": "/srv/talker.json"}, "ledger.json"},
		{"flag without home", []string{"-state", "/srv/ledger.json"}, nil, "/srv/ledger.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse(tc.args, environment(tc.env), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if c.StatePath != tc.want {
				t.Fatalf("state path = %q, want %q", c.StatePath, tc.want)
			}
		})
	}
}

func TestStatePathRequiresLocation(t *testing.T) {
	for _, env := range []map[string]string{nil, {"HOME": "relative"}, {"XDG_STATE_HOME": "relative"}} {
		if _, err := Parse(nil, environment(env), io.Discard); err == nil {
			t.Errorf("%v: expected error", env)
		}
	}
}

func TestRejectUnsafeConfiguration(t *testing.T) {
	for _, args := range [][]string{{"-listen", "no-port-here"}, {"-speech-start", "medium"}, {"-speech-prefix", "5s"}, {"-speech-prefix", "-1ms"}, {"-temperature", "NaN"}, {"-temperature", "2.1"}, {"-quiet-period", "0s"}, {"surprise"}} {
		t.Run(args[0]+args[len(args)-1], func(t *testing.T) {
			if _, err := Parse(args, home, io.Discard); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
