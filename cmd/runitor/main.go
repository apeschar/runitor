// Copyright 2020 - 2022, Berk D. Demir and the runitor contributors
// SPDX-License-Identifier: 0BSD
package main // import "bdd.fi/x/runitor/cmd/runitor"

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	. "bdd.fi/x/runitor/internal" //lint:ignore ST1001 internal
)

// RunConfig sets the behavior of a run.
type RunConfig struct {
	Quiet                   bool     // No cmd stdout
	Silent                  bool     // No cmd stdout or stderr
	NoStartPing             bool     // Don't send Start ping
	NoOutputInPing          bool     // Don't send command std{out, err} with Success and Failure pings
	NoRunId                 bool     // Don't generate and send a run id per run in pings
	PingBodyLimitIsExplicit bool     // Explicit limit via flags
	PingBodyLimit           uint     // Truncate ping body to last N bytes
	OnSuccess               PingType // Ping type to send when command exits successfully
	OnNonzeroExit           PingType // Ping type to send when command exits with a nonzero code
	OnExecFail              PingType // Ping type to send when runitor cannot execute the command
}

// Globals used for building help and identification strings.

// Name is the name of this command.
const Name string = "runitor"

// Homepage is the URL to the canonical website describing this command.
const Homepage string = "https://bdd.fi/x/runitor"

// Version is the version string that gets overridden at link time by the
// release build scripts.
//
// If the binary was build with `go install`, main module's version will be set
// as the value of this variable.
var Version string = ""

func releaseVersion() string {
	if len(Version) == 0 {
		if bi, ok := debug.ReadBuildInfo(); ok {
			Version = bi.Main.Version
		}
	}

	return Version
}

type handleParams struct {
	uuid, slug, pingKey string
}

// Handle composes the final check handle string to be used in the API URL
// based on precedence or returns an error if a coexisting parameter isn't
// passed.
func (c *handleParams) Handle() (handle string, err error) {
	gotUUID, gotSlug, gotPingKey := len(c.uuid) > 0, len(c.slug) > 0, len(c.pingKey) > 0

	switch {
	case gotUUID:
		handle = c.uuid
	case gotSlug && gotPingKey:
		handle = c.pingKey + "/" + c.slug
	case gotSlug:
		err = errors.New("must also pass ping key either with '-ping-key PK' or HC_PING_KEY environment variable")
	case gotPingKey:
		err = errors.New("must also pass check slug with '-slug SL' or CHECK_SLUG environment variable")
	default:
		err = errors.New("must pass either a check UUID or check slug along with project ping key")
	}

	return
}

// FromFlagOrEnv is a helper to return flg if it isn't an empty string or look
// up the environment variable env and return its value if it's set.
// If neither are set returned value is an empty string.
func FromFlagOrEnv(flg, env string) string {
	if len(flg) == 0 {
		v, ok := os.LookupEnv(env)
		if ok && len(v) > 0 {
			return v
		}
	}

	return flg
}

func main() {
	apiURL := flag.String("api-url", DefaultBaseURL, "API URL. Takes precedence over HC_API_URL environment variable")
	apiRetries := flag.Uint("api-retries", DefaultRetries, "Number of times an API request will be retried if it fails with a transient error")
	apiTimeout := flag.Duration("api-timeout", DefaultTimeout, "Client timeout per request")
	pingKey := flag.String("ping-key", "", "Ping Key. Takes precedence over HC_PING_KEY environment variable")
	slug := flag.String("slug", "", "Slug of check. Requires a ping key. Takes precedence over CHECK_SLUG environment variable")
	uuid := flag.String("uuid", "", "UUID of check. Takes precedence over CHECK_UUID environment variable")
	every := flag.Duration("every", 0, "If non-zero, periodically run command at specified interval")
	quiet := flag.Bool("quiet", false, "Don't capture command's stdout")
	silent := flag.Bool("silent", false, "Don't capture command's stdout or stderr")
	onSuccess := pingTypeFlag("on-success", PingTypeSuccess, "Ping type to send when command exits successfully")
	onNonzeroExit := pingTypeFlag("on-nonzero-exit", PingTypeExitCode, "Ping type to send when command exits with a nonzero code")
	onExecFail := pingTypeFlag("on-exec-fail", PingTypeFail, "Ping type to send when runitor cannot execute the command")
	noStartPing := flag.Bool("no-start-ping", false, "Don't send start ping")
	noOutputInPing := flag.Bool("no-output-in-ping", false, "Don't send command's output in pings")
	noRunId := flag.Bool("no-run-id", false, "Don't generate and send a run id per run in pings")
	pingBodyLimit := flag.Uint("ping-body-limit", 10_000, "If non-zero, truncate the ping body to its last N bytes, including a truncation notice.")
	version := flag.Bool("version", false, "Show version")

	reqHeaders := make(map[string]string)
	flag.Func("req-header", "Additional request header as \"key: value\" string", func(s string) error {
		kv := strings.SplitN(s, ":", 2)
		if len(kv) != 2 {
			return errors.New("header not in 'key: value' format")
		}

		reqHeaders[kv[0]] = kv[1]

		return nil
	})

	flag.Parse()

	if *version {
		fmt.Println(Name, releaseVersion())
		os.Exit(0)
	}

	ch := &handleParams{
		uuid:    FromFlagOrEnv(*uuid, "CHECK_UUID"),
		slug:    FromFlagOrEnv(*slug, "CHECK_SLUG"),
		pingKey: FromFlagOrEnv(*pingKey, "HC_PING_KEY"),
	}
	handle, err := ch.Handle()
	if err != nil {
		log.Fatal(err)
	}

	// api-url flag vs HC_API_URL env var vs default value.
	//
	// The reason we cannot use FromFlagOrEnv() here is because we set a
	// non-empty string default value for -api-url flag. We need to figure
	// out if we're explicitly passed a flag or not to decide if we should
	// read the alternate HC_API_URL environment variable.
	urlFromArgs := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "api-url" {
			urlFromArgs = true
		}
	})

	if !urlFromArgs {
		if v, ok := os.LookupEnv("HC_API_URL"); ok && len(v) > 0 {
			apiURL = &v
		}
	}

	pingBodyLimitFromArgs := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "ping-body-limit" {
			pingBodyLimitFromArgs = true
		}
	})

	if flag.NArg() < 1 {
		log.Fatal("missing command")
	}

	retries := Max(0, *apiRetries) // has to be >= 0

	cmd := flag.Args()
	client := &APIClient{
		BaseURL: *apiURL,
		Retries: retries,
		Client: &http.Client{
			Transport: NewDefaultTransportWithResumption(),
			Timeout:   *apiTimeout,
		},
		UserAgent:  fmt.Sprintf("%s/%s (+%s)", Name, releaseVersion(), Homepage),
		ReqHeaders: reqHeaders,
	}

	cfg := RunConfig{
		Quiet:                   *quiet || *silent,
		Silent:                  *silent,
		NoStartPing:             *noStartPing,
		NoOutputInPing:          *noOutputInPing,
		NoRunId:                 *noRunId,
		PingBodyLimitIsExplicit: pingBodyLimitFromArgs,
		PingBodyLimit:           *pingBodyLimit,
		OnSuccess:               *onSuccess,
		OnNonzeroExit:           *onNonzeroExit,
		OnExecFail:              *onExecFail,
	}

	// Save this invocation so we don't repeat ourselves.
	task := func() int {
		return Run(cmd, cfg, handle, client)
	}

	exitCode := task()

	// One-shot mode. Exit with command's exit code.
	if *every == 0 {
		os.Exit(exitCode)
	}

	// Task scheduler mode. Run the command periodically at specified interval.
	ticker := time.NewTicker(*every)
	runNow := make(chan os.Signal, 1)
	signal.Notify(runNow, syscall.SIGALRM)

	for {
		select {
		case <-ticker.C:
			task()

		case <-runNow:
			ticker.Reset(*every)
			task()
		}
	}
}

// Run function runs the cmd line, tees its output to terminal & ping body as
// configured in cfg and pings the monitoring API to signal start, and then
// success or failure of execution. Returns the exit code from the ran command
// unless execution has failed, in such case 1 is returned.
func Run(cmd []string, cfg RunConfig, handle string, p Pinger) int {
	var rid string
	var err error
	if !cfg.NoRunId {
		rid, err = NewUUID4()
		if err != nil {
			panic("XXX")
		}
	}

	if !cfg.NoStartPing {
		icfg, err := p.PingStart(handle, rid)
		if err != nil {
			log.Print("Ping(start): ", err)
		} else if instanceLimit, ok := icfg.PingBodyLimit.Get(); ok {
			if cfg.PingBodyLimitIsExplicit {
				// Command line flag `-ping-body-limit` was used and
				// the service instance returned a `Ping-Body-Limit` header.
				// Pick the smaller value.
				cfg.PingBodyLimit = Min(cfg.PingBodyLimit, instanceLimit)
			} else {
				// Let the instance override the runitor default up to 10MB.
				//
				// TODO(bdd):
				// We impose this limit for now because current
				// ring buffer implementation tries to eagerly
				// allocate a zero filled array at this
				// capacity.
				cfg.PingBodyLimit = Min(instanceLimit, 10_000_000)
			}
		}
	}

	var (
		ringbuf  *RingBuffer
		pingbody io.ReadWriter
	)

	if cfg.PingBodyLimit > 0 {
		ringbuf = NewRingBuffer(int(cfg.PingBodyLimit))
		pingbody = io.ReadWriter(ringbuf)
	} else {
		pingbody = new(bytes.Buffer)
	}

	var mw io.Writer
	if cfg.NoOutputInPing {
		mw = io.MultiWriter(os.Stdout)
	} else {
		mw = io.MultiWriter(os.Stdout, pingbody)
	}

	// WARNING:
	// cmdStdout and cmdStderr either need to be the same Writer or either
	// of them nil. With two different writers the order of stdout and
	// stderr writes cannot be preserved.
	var cmdStdout, cmdStderr io.Writer
	if !cfg.Quiet {
		cmdStdout = mw
	}
	if !cfg.Silent {
		cmdStderr = mw
	}

	exitCode, err := Exec(cmd, cmdStdout, cmdStderr)
	var ping PingType
	switch {
	case exitCode == 0 && err == nil:
		ping = cfg.OnSuccess

	case exitCode > 0 && err != nil:
		// Successfully executed the command.
		// Command exited with nonzero code.
		fmt.Fprintf(pingbody, "\n[%s] %v", Name, err)
		ping = cfg.OnNonzeroExit

	case exitCode == -1 && err != nil:
		// Could not execute the command.
		// Write to host stderr and the ping body.
		w := io.MultiWriter(os.Stderr, pingbody)
		fmt.Fprintf(w, "[%s] %v\n", Name, err)
		ping = cfg.OnExecFail
		exitCode = 1
	}

	if ringbuf != nil && ringbuf.Wrapped() {
		fmt.Fprintf(pingbody, "\n[%s] Output truncated to last %d bytes.", Name, ringbuf.Cap())
	}

	switch ping {
	case PingTypeSuccess:
		_, err = p.PingSuccess(handle, rid, pingbody)
	case PingTypeFail:
		_, err = p.PingFail(handle, rid, pingbody)
	case PingTypeLog:
		_, err = p.PingLog(handle, rid, pingbody)
	default:
		// A safe default: PingExitCode
		// It's too late error out here.
		// Command got executed. We need to deliver a ping.
		_, err = p.PingExitCode(handle, rid, exitCode, pingbody)
	}

	if err != nil {
		log.Printf("Ping(%s): %v\n", ping.String(), err)
	}

	return exitCode
}

// Exec function executes cmd[0] with parameters cmd[1:] and redirects its stdout & stderr to passed
// writers of corresponding parameter names.
func Exec(cmd []string, stdout, stderr io.Writer) (exitCode int, err error) {
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, stdout, stderr

	err = c.Run()
	exitCode = c.ProcessState.ExitCode()

	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && exitCode == -1 {
			// Killed with a signal.
			exitCode = 1
			err = fmt.Errorf("%w", ee)
			return
		}
	}

	// On Windows applications can use any 32-bit integer as the exit code.
	// Healthchecks.io API only allows [0-255].
	// So we clamp it.
	if runtime.GOOS == "windows" && exitCode > 255 {
		exitCode = 1 // ¯\_(ツ)_/¯
	}

	return
}
