package config

import (
	"io"
	"testing"
	"time"
)

func TestDefaultsAndOverrides(t *testing.T) {
	c, err := Parse([]string{"-api-key-file", "/tmp/example-key", "-temperature", "1.3"}, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "gemini-3.8-live" || c.APIKeyFile != "/tmp/example-key" || c.Temperature != 1.3 || c.Listen != "127.0.0.1:8080" || c.SpeechStart != "low" || c.SpeechPrefix != 200*time.Millisecond {
		t.Fatalf("unexpected config: %+v", c)
	}
}

func TestRejectUnsafeConfiguration(t *testing.T) {
	for _, args := range [][]string{{"-listen", "no-port-here"}, {"-speech-start", "medium"}, {"-speech-prefix", "5s"}, {"-speech-prefix", "-1ms"}, {"-temperature", "NaN"}, {"-temperature", "2.1"}, {"-quiet-period", "0s"}, {"surprise"}} {
		t.Run(args[0]+args[len(args)-1], func(t *testing.T) {
			if _, err := Parse(args, func(string) string { return "" }, io.Discard); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
