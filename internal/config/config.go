package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen, Model, Voice, Language      string
	HermesURL, HermesKey, HermesKeyFile string
	StatePath, EventToken               string
	Instruction                         string
	APIKey, APIKeyFile                  string
	Check                               bool
	Temperature                         float64
	QuietPeriod                         time.Duration
	SpeechStart                         string
	SpeechPrefix                        time.Duration
}

func Parse(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	var cfg Config
	env := func(name, fallback string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return fallback
	}
	fs := flag.NewFlagSet("talker", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.Listen, "listen", env("TALKER_LISTEN", "127.0.0.1:8080"), "HTTP listen address; put an authenticating proxy or SSH tunnel in front if not loopback")
	fs.StringVar(&cfg.Model, "model", env("TALKER_MODEL", "gemini-3.8-live"), "Gemini Developer API Live model ID")
	fs.StringVar(&cfg.HermesKeyFile, "hermes-key-file", getenv("HERMES_API_KEY_FILE"), "file containing the Hermes API_SERVER_KEY (alternative to HERMES_API_KEY)")
	fs.StringVar(&cfg.APIKeyFile, "api-key-file", getenv("GEMINI_API_KEY_FILE"), "file containing the server-side Gemini API key (alternative to GEMINI_API_KEY)")
	fs.BoolVar(&cfg.Check, "check", false, "test token creation and one brief Live audio response, then exit")
	fs.StringVar(&cfg.Voice, "voice", env("TALKER_VOICE", "Aoede"), "Gemini prebuilt voice")
	fs.StringVar(&cfg.Language, "language", env("TALKER_LANGUAGE", "en-US"), "BCP-47 speech language the model must stick to; empty lets it auto-detect")
	fs.StringVar(&cfg.HermesURL, "hermes-url", getenv("HERMES_URL"), "Hermes API Server base URL, including optional /p/profile prefix")
	fs.StringVar(&cfg.StatePath, "state", env("TALKER_STATE", defaultStatePath(getenv)), "private persistent task and notification ledger")
	fs.StringVar(&cfg.Instruction, "instruction", getenv("TALKER_INSTRUCTION"), "additional voice/personality preferences")
	fs.StringVar(&cfg.SpeechStart, "speech-start", env("TALKER_SPEECH_START", "low"), "Gemini start-of-speech sensitivity: low ignores clicks and keyboard noise; high catches soft or brief speech")
	var temp, quiet, prefix string
	fs.StringVar(&temp, "temperature", env("TALKER_TEMPERATURE", "1.1"), "speech variation, from 0 to 2")
	fs.StringVar(&quiet, "quiet-period", env("TALKER_QUIET_PERIOD", "2s"), "minimum conversational silence before background announcements")
	fs.StringVar(&prefix, "speech-prefix", env("TALKER_SPEECH_PREFIX", "200ms"), "speech duration required before Gemini commits to a turn; shorter transients are ignored")
	cfg.HermesKey, cfg.EventToken = getenv("HERMES_API_KEY"), getenv("TALKER_EVENT_TOKEN")
	cfg.APIKey = getenv("GEMINI_API_KEY")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments")
	}
	var err error
	cfg.Temperature, err = strconv.ParseFloat(temp, 64)
	if err != nil || math.IsNaN(cfg.Temperature) || math.IsInf(cfg.Temperature, 0) || cfg.Temperature < 0 || cfg.Temperature > 2 {
		return cfg, errors.New("temperature must be between 0 and 2")
	}
	cfg.QuietPeriod, err = time.ParseDuration(quiet)
	if err != nil || cfg.QuietPeriod < 500*time.Millisecond || cfg.QuietPeriod > time.Minute {
		return cfg, errors.New("quiet-period must be between 500ms and 1m")
	}
	if cfg.SpeechStart = strings.ToLower(cfg.SpeechStart); cfg.SpeechStart != "low" && cfg.SpeechStart != "high" {
		return cfg, errors.New("speech-start must be low or high")
	}
	cfg.SpeechPrefix, err = time.ParseDuration(prefix)
	if err != nil || cfg.SpeechPrefix < 0 || cfg.SpeechPrefix > 2*time.Second {
		return cfg, errors.New("speech-prefix must be between 0 and 2s")
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return cfg, fmt.Errorf("listen address: %w", err)
	}
	if cfg.StatePath == "" {
		return cfg, errors.New("state path is required: set -state, TALKER_STATE, XDG_STATE_HOME or HOME")
	}
	return cfg, nil
}

// defaultStatePath places the ledger in the XDG Base Directory state home. The
// spec requires relative XDG_STATE_HOME values to be ignored, not resolved.
func defaultStatePath(getenv func(string) string) string {
	dir := getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(dir) {
		home := getenv("HOME")
		if !filepath.IsAbs(home) {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "talker", "tasks.json")
}

func FromEnvironment() (Config, error) { return Parse(os.Args[1:], os.Getenv, os.Stderr) }
